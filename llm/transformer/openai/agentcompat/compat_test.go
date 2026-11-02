package agentcompat

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

const nativeCatalog = `{"model":"gpt-6.1-sol","tools":[{"type":"namespace","name":"collaboration","tools":[{"type":"function","name":"spawn_agent","parameters":{"type":"object","properties":{"model":{"type":"string"},"message":{"type":"string","encrypted":true}}}},{"type":"function","name":"send_message","parameters":{"type":"object","properties":{"message":{"type":"string","encrypted":true}}}},{"type":"function","name":"followup_task","parameters":{"type":"object","properties":{"message":{"type":"string","encrypted":true}}}}]}],"input":[{"type":"message","role":"user","content":"test"}]}`

func TestPreservesNativeCatalogAndEncryptedGPTHistory(t *testing.T) {
	var original map[string]any
	require.NoError(t, json.Unmarshal([]byte(nativeCatalog), &original))
	result, err := RewriteRequest([]byte(nativeCatalog), false)
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.Unmarshal(result, &got))
	require.Equal(t, original["tools"].([]any)[0], got["tools"].([]any)[0], "original GPT V2 tool schema must be untouched")
	require.Equal(t, original["input"], got["input"], "tool injection must not add hidden history messages")
	require.Len(t, got["tools"], 2)
	for _, tool := range object(got["tools"].([]any)[1])["tools"].([]any) {
		require.NotContains(t, object(object(object(object(tool)["parameters"])["properties"])["message"]), "encrypted")
	}
	input := []byte(`{"input":[{"type":"function_call","namespace":"collaboration","name":"spawn_agent","arguments":"{\"model\":\"gpt-6-luna\",\"message\":\"gAAAAciphertext\"}"},{"type":"agent_message","content":[{"type":"encrypted_content","encrypted_content":"gAAAAciphertext"}]}]}`)
	result, err = RewriteRequest(input, false)
	require.NoError(t, err)
	require.Equal(t, input, result)
}

func TestPortableTaskAndOpaqueFailure(t *testing.T) {
	input := []byte(`{"input":[{"type":"agent_message","author":"/root","recipient":"/root/ds","content":[{"type":"input_text","text":"NEW_TASK\n37*19"}]}]}`)
	got, err := RewriteRequest(input, true)
	require.NoError(t, err)
	require.JSONEq(t, `{"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"NEW_TASK\n37*19"}]}]}`, string(got))
	_, err = RewriteRequest([]byte(`{"input":[{"type":"agent_message","content":[{"type":"encrypted_content","encrypted_content":"opaque"}]}]}`), true)
	require.ErrorContains(t, err, "plaintext")
}

func TestNativeResponsePreservedAndDeepSeekCallRestored(t *testing.T) {
	input := []byte(`{"type":"response.output_item.done","item":{"type":"function_call","namespace":"collaboration","name":"spawn_agent","arguments":"{\"model\":\"gpt-6-luna\",\"message\":\"gAAAAciphertext\"}"}}`)
	got, err := RestoreResponse(input, false)
	require.NoError(t, err)
	require.Equal(t, input, got)
	for _, name := range []string{"dispatch_task", "deliver_message", "continue_task"} {
		input := []byte(`{"type":"response.output_item.added","item":{"type":"function_call","namespace":"axonhub_v2","name":"` + name + `","arguments":""}}`)
		got, err := RestoreResponse(input, false)
		require.NoError(t, err)
		var result map[string]any
		require.NoError(t, json.Unmarshal(got, &result))
		item := object(result["item"])
		require.Equal(t, "collaboration", item["namespace"])
		require.Equal(t, []any{}, item["encrypted_function_args"])
	}
	input = []byte(`{"type":"function_call","namespace":"collaboration","name":"send_message","arguments":"{\"target\":\"/root\",\"message\":\"result=703\"}"}`)
	got, err = RestoreResponse(input, true)
	require.NoError(t, err)
	require.Contains(t, string(got), `"encrypted_function_args":[]`)
}

func TestOrdinaryPayloadAndToolOutputUntouched(t *testing.T) {
	input := []byte(`{ "model":"gpt-6.1-sol", "input":[{"type":"function_call_output","call_id":"x","output":"axonhub_v2.dispatch_task"}], "tools":[{"type":"image_generation"}] }`)
	got, err := RewriteRequest(input, false)
	require.NoError(t, err)
	require.Equal(t, input, got)
	_, err = RewriteRequest([]byte(`{"tools":[{"type":"namespace","name":"axonhub_v2"}]}`), false)
	require.ErrorContains(t, err, "conflicts")
}

func TestPlaintextHistoryAndInvalidAdapterCalls(t *testing.T) {
	input := []byte(`{"input":[{"type":"function_call","namespace":"collaboration","name":"send_message","arguments":"{\"target\":\"ds\",\"message\":\"follow up\"}"},{"type":"function_call","namespace":"collaboration","name":"list_agents","arguments":"{\"message\":\"unchanged\"}"}]}`)
	got, err := RewriteRequest(input, false)
	require.NoError(t, err)
	require.Contains(t, string(got), `"name":"deliver_message"`)
	var result map[string]any
	require.NoError(t, json.Unmarshal(got, &result))
	require.Equal(t, "collaboration", object(result["input"].([]any)[1])["namespace"])
	for _, arguments := range []string{`{"model":"gpt-6-luna","message":"task"}`, `{"model":"deepseek-flash","message":"gAAAAciphertext"}`, `not json`} {
		call, err := json.Marshal(map[string]any{"type": "function_call", "namespace": Namespace, "name": "dispatch_task", "arguments": arguments})
		require.NoError(t, err)
		_, err = RestoreResponse(call, false)
		require.Error(t, err)
	}
}

func TestPortableStreamingDefersFinalPhase(t *testing.T) {
	// Captured DeepSeek sequence: a message starts final, but ends commentary
	// immediately before a tool call. Final answers use the same start event.
	start := []byte(`{"type":"response.output_item.added","sequence_number":9007199254740993,"output_index":1,"item":{"type":"message","role":"assistant","id":"msg_1","status":"in_progress","phase":"final_answer","content":[]}}`)
	got, err := RestoreResponse(start, true)
	require.NoError(t, err)
	require.JSONEq(t, `{"type":"response.output_item.added","sequence_number":9007199254740993,"output_index":1,"item":{"type":"message","role":"assistant","id":"msg_1","status":"in_progress","phase":"commentary","content":[]}}`, string(got))
	again, err := RestoreResponse(got, true)
	require.NoError(t, err)
	require.Equal(t, got, again)
	for _, phase := range []string{"commentary", "final_answer"} {
		for _, body := range []string{
			`{"type":"response.output_item.done","item":{"type":"message","role":"assistant","id":"msg_1","phase":"` + phase + `","content":[{"type":"output_text","text":"visible text"}]}}`,
			`{"type":"response.completed","response":{"output":[{"type":"message","role":"assistant","phase":"` + phase + `"}]}}`,
			`{"object":"response","output":[{"type":"message","role":"assistant","phase":"` + phase + `"}]}`,
		} {
			got, err := RestoreResponse([]byte(body), true)
			require.NoError(t, err)
			require.Equal(t, body, string(got), "completed phases and non-stream responses remain authoritative")
		}
	}
	got, err = RestoreResponse(start, false)
	require.NoError(t, err)
	require.Equal(t, start, got, "native GPT must retain early final-answer rendering")
}

func TestPortablePhasePreservesOtherEvents(t *testing.T) {
	for _, body := range []string{
		`{"type":"response.output_text.delta","item_id":"msg_1","delta":"hello"}`,
		`{"type":"response.output_item.added","item":{"type":"reasoning","phase":"final_answer"}}`,
		`{"type":"response.output_item.added","item":{"type":"message","role":"user","phase":"final_answer"}}`,
		`{"type":"response.output_item.added","item":{"type":"message","role":"assistant","phase":"partial_answer"}}`,
		`{"type":"response.output_item.added","item":{"type":"message","role":"assistant"}}`,
		`{"type":"response.output_item.added","item":{"type":"function_call","name":"probe","call_id":"c1","arguments":""}}`,
		`{"type":"response.incomplete","response":{"status":"incomplete","output":[{"type":"message","role":"assistant","phase":"final_answer"}]}}`,
		`{"type":"response.failed","response":{"error":{"message":"failure"}}}`,
		`{"type":"error","message":"failure"}`,
		`[DONE]`,
	} {
		got, err := RestoreResponse([]byte(body), true)
		require.NoError(t, err)
		require.Equal(t, body, string(got))
	}
	_, err := RestoreResponse([]byte(`{"type":`), true)
	require.Error(t, err)
}
