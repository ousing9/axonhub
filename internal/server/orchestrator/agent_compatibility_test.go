package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/streams"
	"github.com/looplj/axonhub/llm/transformer/deepseek"
	"github.com/looplj/axonhub/llm/transformer/openai/responses"
)

func compatibilityTestState() (*agentCompatibilityMiddleware, *PersistenceState) {
	state := &PersistenceState{
		CurrentCandidate: &ChannelModelsCandidate{Channel: &biz.Channel{Channel: &ent.Channel{
			ID: 36, Settings: &objects.ChannelSettings{PassThroughBody: new(true)},
		}}},
		PassThroughApplied: true,
		LlmRequest:         &llm.Request{APIFormat: llm.APIFormatOpenAIResponse},
		RawProviderRequest: &httpclient.Request{APIFormat: string(llm.APIFormatOpenAIResponse)},
	}
	return &agentCompatibilityMiddleware{outbound: &PersistentOutboundTransformer{state: state}, config: AgentCompatibilityConfig{PortableModels: []string{"deepseek-flash"}}}, state
}

func TestPortablePassthroughHonorsRoleOption(t *testing.T) {
	m, state := compatibilityTestState()
	m.portable = true
	ch := state.CurrentCandidate.Channel
	ch.Settings.TransformOptions.ReplaceDeveloperRoleWithSystem = true
	raw := &httpclient.Request{APIFormat: string(llm.APIFormatOpenAIResponse), Body: []byte(`{"input":[{"type":"message","role":"developer","content":"instruction"},{"role":"developer","content":"implicit message"},{"type":"function_call_output","role":"developer","output":"opaque data"},{"type":"message","role":"user","content":"request"}]}`)}
	got, err := m.prepareRequest(t.Context(), raw)
	require.NoError(t, err)
	require.JSONEq(t, `{"input":[{"type":"message","role":"system","content":"instruction"},{"role":"system","content":"implicit message"},{"type":"function_call_output","role":"developer","output":"opaque data"},{"type":"message","role":"user","content":"request"}]}`, string(got.Body))
	require.Contains(t, string(raw.Body), `"role":"developer","content":"instruction"`, "do not mutate persisted input")
	require.False(t, m.active, "portable requests must not enable GPT response rewrites")
	ch.Settings.TransformOptions.ReplaceDeveloperRoleWithSystem = false
	got, err = m.prepareRequest(t.Context(), raw)
	require.NoError(t, err)
	require.Same(t, raw, got)
	// Retry/failover must use the newly selected channel's option.
	ch.Settings = nil
	got, err = m.prepareRequest(t.Context(), raw)
	require.NoError(t, err)
	require.Same(t, raw, got)
}

func TestAgentCompatibilityDisabledAndOtherFormats(t *testing.T) {
	m, _ := compatibilityTestState()
	ctx := context.Background()
	req := &httpclient.Request{APIFormat: string(llm.APIFormatOpenAIImageGeneration), Body: []byte("native image payload")}
	got, err := m.prepareRequest(ctx, req)
	require.NoError(t, err)
	require.Same(t, req, got)
	require.False(t, m.active)
	m.config = AgentCompatibilityConfig{}
	req.APIFormat = string(llm.APIFormatOpenAIResponse)
	got, err = m.prepareRequest(ctx, req)
	require.NoError(t, err)
	require.Same(t, req, got, "disabled compatibility must not parse or mutate requests")
	raw := &httpclient.Response{StatusCode: 200, Body: []byte("untouched native response")}
	result, err := m.OnOutboundRawResponse(ctx, raw)
	require.NoError(t, err)
	require.Same(t, raw, result)
}

func TestAgentCompatibilityRestorationBeforePassthroughCapture(t *testing.T) {
	m, state := compatibilityTestState()
	ctx := context.Background()
	_, err := m.prepareRequest(ctx, &httpclient.Request{APIFormat: string(llm.APIFormatOpenAIResponse), Body: []byte(`{"previous_response_id":"resp_previous","input":[]}`)})
	require.NoError(t, err)
	raw := &httpclient.Response{StatusCode: 200, Body: []byte(`{"output":[{"type":"function_call","namespace":"axonhub_v2","name":"dispatch_task","arguments":""}]}`)}
	restored, err := m.OnOutboundRawResponse(ctx, raw)
	require.NoError(t, err)
	_, err = captureRawProviderResponse(m.outbound, nil).OnOutboundRawResponse(ctx, restored)
	require.NoError(t, err)
	result, err := applyPassThroughResponse(m.outbound, nil).OnInboundRawResponse(ctx, &httpclient.Response{})
	require.NoError(t, err)
	require.Contains(t, string(result.Body), `"namespace":"collaboration"`)
	require.Contains(t, string(result.Body), `"encrypted_function_args":[]`)
	require.NotSame(t, raw, result)
	require.Same(t, state.RawProviderResponse, result)
}

func TestAgentCompatibilityRetryAndStreamState(t *testing.T) {
	m, state := compatibilityTestState()
	ctx := context.Background()
	_, err := m.prepareRequest(ctx, &httpclient.Request{APIFormat: string(llm.APIFormatOpenAIResponse), Body: []byte(`{"previous_response_id":"resp_previous","input":[]}`)})
	require.NoError(t, err)
	event := &httpclient.StreamEvent{Type: "response.output_item.done", Data: []byte(`{"type":"response.output_item.done","item":{"type":"function_call","namespace":"axonhub_v2","name":"continue_task","arguments":""}}`)}
	s, err := m.OnOutboundRawStream(ctx, streams.SliceStream([]*httpclient.StreamEvent{event}))
	require.NoError(t, err)
	defer s.Close()
	// A retry on a non-passthrough channel must not activate rewriting there, nor
	// change the behavior of a stream already bound to the first attempt.
	state.CurrentCandidate.Channel.ID = 99
	state.PassThroughApplied = false
	_, err = m.prepareRequest(ctx, &httpclient.Request{APIFormat: string(llm.APIFormatOpenAIResponse), Body: []byte("not parsed")})
	require.NoError(t, err)
	require.False(t, m.active)
	require.True(t, s.Next())
	require.Contains(t, string(s.Current().Data), `"name":"followup_task"`)
	require.Contains(t, string(event.Data), `"name":"continue_task"`, "do not mutate events shared with persistence")
	require.False(t, s.Next())
	require.NoError(t, s.Err())
}

func TestAgentCompatibilityRejectsOpaqueTaskAndBypassesOtherProtocols(t *testing.T) {
	m, state := compatibilityTestState()
	ctx := context.Background()
	req := &httpclient.Request{APIFormat: string(llm.APIFormatOpenAIResponse), Body: []byte(`{"model":"deepseek-flash","input":[{"type":"agent_message","content":[{"type":"encrypted_content","encrypted_content":"ciphertext"}]}]}`)}
	_, _, err := preparePortableAgentRequest(req, m.config)
	require.ErrorContains(t, err, "plaintext")
	state.PassThroughApplied = false
	_, err = m.prepareRequest(ctx, req)
	require.NoError(t, err)
	require.False(t, m.active)
}

func TestAgentCompatibilityPortableChatCompletionsRoundTrip(t *testing.T) {
	ctx := context.Background()
	m, _ := compatibilityTestState()
	raw := &httpclient.Request{Body: []byte(`{"model":"deepseek-flash","input":[{"type":"agent_message","author":"/root","content":[{"type":"input_text","text":"Compute 37*19 and report to /root"}]}],"tools":[{"type":"namespace","name":"collaboration","tools":[{"type":"function","name":"send_message","parameters":{"type":"object","properties":{"target":{"type":"string"},"message":{"type":"string"}}}}]}]}`)}
	prepared, active, err := preparePortableAgentRequest(raw, m.config)
	require.NoError(t, err)
	require.True(t, active)
	require.Contains(t, string(raw.Body), `"agent_message"`, "preserve source history")
	inbound := responses.NewInboundTransformer()
	unified, err := inbound.TransformRequest(ctx, prepared)
	require.NoError(t, err)
	outbound, err := deepseek.NewOutboundTransformer("https://example.invalid", "test-key")
	require.NoError(t, err)
	providerReq, err := outbound.TransformRequest(ctx, unified)
	require.NoError(t, err)
	require.Contains(t, string(providerReq.Body), "Compute 37*19 and report to /root")
	require.Contains(t, string(providerReq.Body), `"role":"user"`)
	require.Contains(t, string(providerReq.Body), "collaboration__send_message")
	providerResp := &httpclient.Response{StatusCode: 200, Body: []byte(`{"id":"test","object":"chat.completion","model":"deepseek-flash","choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"collaboration__send_message","arguments":"{\"target\":\"/root\",\"message\":\"703\"}"}}]},"finish_reason":"tool_calls"}]}`)}
	response, err := outbound.TransformResponse(ctx, providerResp)
	require.NoError(t, err)
	response.TransformerMetadata = unified.TransformerMetadata
	converted, err := inbound.TransformResponse(ctx, response)
	require.NoError(t, err)
	portable := &portableAgentResponseMiddleware{}
	restored, err := portable.OnInboundRawResponse(ctx, converted)
	require.NoError(t, err)
	require.Contains(t, string(restored.Body), `"namespace":"collaboration"`)
	require.Contains(t, string(restored.Body), `"name":"send_message"`)
	require.Contains(t, string(restored.Body), `"encrypted_function_args":[]`)
	require.NotContains(t, string(converted.Body), "encrypted_function_args")
	stream, err := portable.OnInboundRawStream(ctx, streams.SliceStream([]*httpclient.StreamEvent{{Data: converted.Body}}))
	require.NoError(t, err)
	defer stream.Close()
	require.True(t, stream.Next())
	require.JSONEq(t, string(restored.Body), string(stream.Current().Data))
	require.False(t, stream.Next())
	require.NoError(t, stream.Err())
}

func TestAgentCompatibilityPortableModelScope(t *testing.T) {
	for _, cfg := range []AgentCompatibilityConfig{{}, {PortableModels: []string{"deepseek-flash"}}} {
		req := &httpclient.Request{Body: []byte(`{"model":"gpt-6-astra","input":[{"type":"agent_message","content":[{"type":"encrypted_content"}]}]}`)}
		got, active, err := preparePortableAgentRequest(req, cfg)
		require.NoError(t, err)
		require.False(t, active)
		require.Same(t, req, got)
	}
}

func TestNativeAgentCompatibilityBypass(t *testing.T) {
	m, state := compatibilityTestState()
	m.config.PortableModels = nil    // Empty configuration disables all adaptation.
	state.PassThroughApplied = false // Bypass must not impose new passthrough requirements.
	req := &httpclient.Request{APIFormat: string(llm.APIFormatOpenAIResponse), Body: []byte("not parsed"), Headers: http.Header{"Content-Length": {"10"}}}
	got, err := m.prepareRequest(t.Context(), req)
	require.NoError(t, err)
	require.Same(t, req, got)
	require.Equal(t, "not parsed", string(got.Body))
	require.Equal(t, "10", got.Headers.Get("Content-Length"))
	require.False(t, m.active)
	for _, status := range []int{200, 400, 500} {
		resp := &httpclient.Response{StatusCode: status, Body: []byte("not parsed")}
		got, err := m.OnOutboundRawResponse(t.Context(), resp)
		require.NoError(t, err)
		require.Same(t, resp, got)
	}
	stream := streams.SliceStream([]*httpclient.StreamEvent{{Data: []byte("not parsed")}})
	defer stream.Close()
	gotStream, err := m.OnOutboundRawStream(t.Context(), stream)
	require.NoError(t, err)
	require.Same(t, stream, gotStream)
}

func TestNativeCompatibilityIndependentOfChannel(t *testing.T) {
	m, state := compatibilityTestState()
	m.config.PortableModels = []string{"deepseek-flash", "deepseek-v4.1-flash"}
	var functions []any
	for _, name := range []string{"spawn_agent", "send_message", "followup_task"} {
		functions = append(functions, map[string]any{"type": "function", "name": name, "parameters": map[string]any{"type": "object", "properties": map[string]any{"message": map[string]any{"type": "string", "encrypted": true}}}})
	}
	body, err := json.Marshal(map[string]any{"model": "gpt-6-astra", "input": []any{}, "tools": []any{map[string]any{"type": "namespace", "name": "collaboration", "tools": functions}}})
	require.NoError(t, err)
	var previous []byte
	for _, id := range []int{36, 61, 999} {
		t.Run(fmt.Sprint(id), func(t *testing.T) {
			state.CurrentCandidate.Channel.ID = id
			req := &httpclient.Request{APIFormat: string(llm.APIFormatOpenAIResponse), Body: body}
			got, err := m.prepareRequest(t.Context(), req)
			require.NoError(t, err)
			require.True(t, m.active)
			require.Contains(t, string(got.Body), "dispatch_task")
			require.Contains(t, string(got.Body), "deepseek-v4.1-flash")
			if previous != nil {
				require.Equal(t, previous, got.Body, "failover must not change the catalog or cache prefix")
			}
			previous = got.Body
			args, err := json.Marshal(map[string]string{"model": "deepseek-v4.1-flash", "message": "task"})
			require.NoError(t, err)
			response, err := json.Marshal(map[string]any{"output": []any{map[string]any{"type": "function_call", "namespace": "axonhub_v2", "name": "dispatch_task", "arguments": string(args)}}})
			require.NoError(t, err)
			restored, err := m.OnOutboundRawResponse(t.Context(), &httpclient.Response{StatusCode: 200, Body: response})
			require.NoError(t, err)
			require.Contains(t, string(restored.Body), "spawn_agent")
			require.Contains(t, string(restored.Body), "deepseek-v4.1-flash")
		})
	}
}

func TestNativeCompatibilityOrdinaryRequestIsUntouched(t *testing.T) {
	m, _ := compatibilityTestState()
	req := &httpclient.Request{APIFormat: string(llm.APIFormatOpenAIResponse), Body: []byte(`{"model":"gpt-6-astra","input":"hello","tools":[{"type":"function","name":"probe"}]}`), Headers: http.Header{"Content-Length": {"96"}}}
	got, err := m.prepareRequest(t.Context(), req)
	require.NoError(t, err)
	require.Same(t, req, got)
	require.Equal(t, "96", got.Headers.Get("Content-Length"))
	require.False(t, m.active)
	resp := &httpclient.Response{StatusCode: 200, Body: []byte(`{"type":"response.output_item.added","item":{"type":"message","role":"assistant","phase":"final_answer"}}`)}
	restored, err := m.OnOutboundRawResponse(t.Context(), resp)
	require.NoError(t, err)
	require.Same(t, resp, restored)
}

func TestPortableStreamInfersMissingMessagePhases(t *testing.T) {
	events := []*httpclient.StreamEvent{
		{Type: "response.output_item.added", Data: []byte(`{"type":"response.output_item.added","item":{"type":"message","role":"assistant","id":"msg_tool","phase":null,"content":[]}}`)},
		{Type: "response.output_text.delta", Data: []byte(`{"type":"response.output_text.delta","item_id":"msg_tool","delta":"working"}`)},
		{Type: "response.output_item.done", Data: []byte(`{"type":"response.output_item.done","item":{"type":"message","role":"assistant","id":"msg_tool","phase":null,"content":[]}}`)},
		{Type: "response.output_item.added", Data: []byte(`{"type":"response.output_item.added","item":{"type":"function_call","id":"fc_1","name":"probe"}}`)},
		{Type: "response.output_item.done", Data: []byte(`{"type":"response.output_item.done","item":{"type":"function_call","id":"fc_1","name":"probe"}}`)},
		{Type: "response.completed", Data: []byte(`{"type":"response.completed","response":{"status":"completed"}}`)},
	}
	stream := &agentCompatibilityStream{Stream: streams.SliceStream(events), portable: true}
	defer stream.Close()
	var got []string
	for stream.Next() {
		data := stream.Current().Data
		got = append(got, gjson.GetBytes(data, "type").String()+":"+gjson.GetBytes(data, "item.phase").String())
	}
	require.NoError(t, stream.Err())
	require.Equal(t, []string{
		"response.output_item.added:commentary",
		"response.output_text.delta:",
		"response.output_item.done:commentary",
		"response.output_item.added:",
		"response.output_item.done:",
		"response.completed:",
	}, got)
	for _, event := range events {
		require.NotContains(t, string(event.Data), `"phase":"commentary"`, "source events must remain unchanged")
	}
}

func TestPortableStreamInfersFinalPhaseAtCompletion(t *testing.T) {
	events := []*httpclient.StreamEvent{
		{Type: "response.output_item.added", Data: []byte(`{"type":"response.output_item.added","item":{"type":"message","role":"assistant","id":"msg_final","content":[]}}`)},
		{Type: "response.output_text.delta", Data: []byte(`{"type":"response.output_text.delta","item_id":"msg_final","delta":"done"}`)},
		{Type: "response.output_item.done", Data: []byte(`{"type":"response.output_item.done","item":{"type":"message","role":"assistant","id":"msg_final","content":[]}}`)},
		{Type: "response.completed", Data: []byte(`{"type":"response.completed","response":{"status":"completed"}}`)},
	}
	stream := &agentCompatibilityStream{Stream: streams.SliceStream(events), portable: true}
	defer stream.Close()
	var phases []string
	for stream.Next() {
		if typ := gjson.GetBytes(stream.Current().Data, "type").String(); typ == "response.output_item.added" || typ == "response.output_item.done" {
			phases = append(phases, gjson.GetBytes(stream.Current().Data, "item.phase").String())
		}
	}
	require.NoError(t, stream.Err())
	require.Equal(t, []string{"commentary", "final_answer"}, phases)
}

func TestNativeStreamDoesNotInferMissingMessagePhases(t *testing.T) {
	event := &httpclient.StreamEvent{Type: "response.output_item.added", Data: []byte(`{"type":"response.output_item.added","item":{"type":"message","role":"assistant","id":"msg","content":[]}}`)}
	stream := &agentCompatibilityStream{Stream: streams.SliceStream([]*httpclient.StreamEvent{event}), portable: false, portableModels: []string{"deepseek-flash"}}
	defer stream.Close()
	require.True(t, stream.Next())
	require.Equal(t, string(event.Data), string(stream.Current().Data))
	require.False(t, stream.Next())
	require.NoError(t, stream.Err())
}
