package orchestrator

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

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
	return &agentCompatibilityMiddleware{outbound: &PersistentOutboundTransformer{state: state}, config: AgentCompatibilityConfig{NativeChannelIDs: []int{36}, PortableModels: []string{"deepseek-flash"}}}, state
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
	_, err := m.prepareRequest(ctx, &httpclient.Request{APIFormat: string(llm.APIFormatOpenAIResponse), Body: []byte(`{"input":[]}`)})
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
	_, err := m.prepareRequest(ctx, &httpclient.Request{APIFormat: string(llm.APIFormatOpenAIResponse), Body: []byte(`{"input":[]}`)})
	require.NoError(t, err)
	event := &httpclient.StreamEvent{Type: "response.output_item.done", Data: []byte(`{"type":"response.output_item.done","item":{"type":"function_call","namespace":"axonhub_v2","name":"continue_task","arguments":""}}`)}
	s, err := m.OnOutboundRawStream(ctx, streams.SliceStream([]*httpclient.StreamEvent{event}))
	require.NoError(t, err)
	defer s.Close()
	// A retry on an unrelated channel must not activate rewriting there, nor
	// change the behavior of a stream already bound to the first attempt.
	state.CurrentCandidate.Channel.ID = 99
	_, err = m.prepareRequest(ctx, &httpclient.Request{APIFormat: string(llm.APIFormatOpenAIResponse), Body: []byte("not parsed")})
	require.NoError(t, err)
	require.False(t, m.active)
	require.True(t, s.Next())
	require.Contains(t, string(s.Current().Data), `"name":"followup_task"`)
	require.Contains(t, string(event.Data), `"name":"continue_task"`, "do not mutate events shared with persistence")
	require.False(t, s.Next())
	require.NoError(t, s.Err())
}

func TestAgentCompatibilityRejectsOpaqueTaskAndMissingPassthrough(t *testing.T) {
	m, state := compatibilityTestState()
	ctx := context.Background()
	req := &httpclient.Request{APIFormat: string(llm.APIFormatOpenAIResponse), Body: []byte(`{"model":"deepseek-flash","input":[{"type":"agent_message","content":[{"type":"encrypted_content","encrypted_content":"ciphertext"}]}]}`)}
	_, _, err := preparePortableAgentRequest(req, m.config)
	require.ErrorContains(t, err, "plaintext")
	state.PassThroughApplied = false
	_, err = m.prepareRequest(ctx, req)
	require.ErrorContains(t, err, "passthrough")
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
