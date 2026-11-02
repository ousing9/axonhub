package responses

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/transformer/openai/agentcompat"
)

// Exercise the adapter with the real WebSocket executor, not just JSON prefix
// comparisons: the client never receives gateway-only guidance in its history.
func TestAgentCompatibilityPreservesWebSocketContinuation(t *testing.T) {
	for _, embeddedCatalog := range []bool{false, true} {
		t.Run(fmt.Sprintf("additional_tools=%t", embeddedCatalog), func(t *testing.T) {
			var upgrades, turns atomic.Int32
			payloads := make(chan map[string]any, 5)
			upgrader := websocket.Upgrader{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				upgrades.Add(1)
				conn, err := upgrader.Upgrade(w, r, nil)
				if err != nil {
					t.Error(err)
					return
				}
				defer conn.Close()
				for {
					var payload map[string]any
					if err := conn.ReadJSON(&payload); err != nil {
						return
					}
					payloads <- payload
					n := turns.Add(1)
					output := map[string]any{
						"type": "function_call", "id": fmt.Sprintf("fc_%d", n),
						"call_id": fmt.Sprintf("call_%d", n), "name": "probe", "arguments": "{}",
					}
					if n == 2 {
						output["namespace"], output["name"] = "axonhub_v2", "deliver_message"
						output["arguments"] = `{"target":"ds","message":"continue"}`
					} else if n == 3 {
						output["namespace"], output["name"] = "collaboration", "spawn_agent"
						output["arguments"] = `{"model":"gpt-test","message":"gAAAAopaque"}`
					}
					err := conn.WriteJSON(map[string]any{
						"type": "response.completed",
						"response": map[string]any{
							"id": fmt.Sprintf("resp_%d", n), "status": "completed",
							"output": []any{output},
						},
					})
					if err != nil {
						t.Error(err)
						return
					}
				}
			}))
			defer server.Close()
			executor := NewWebSocketExecutor(nil)
			defer executor.Close()
			var catalog []any
			require.NoError(t, json.Unmarshal([]byte(`[{"type":"namespace","name":"collaboration","tools":[{"type":"function","name":"spawn_agent","parameters":{"type":"object","properties":{"message":{"type":"string","encrypted":true}}}},{"type":"function","name":"send_message","parameters":{"type":"object","properties":{"message":{"type":"string","encrypted":true}}}},{"type":"function","name":"followup_task","parameters":{"type":"object","properties":{"message":{"type":"string","encrypted":true}}}}]}]`), &catalog))
			user := map[string]any{"type": "message", "role": "user", "content": "Run the probe."}
			history := []any{user}
			if embeddedCatalog {
				history = append([]any{map[string]any{"type": "additional_tools", "role": "developer", "tools": catalog}}, history...)
			}
			var firstCatalog any
			for round := 0; round < 5; round++ {
				if round == 4 {
					// A genuine edit to old history must still force a fresh connection.
					user["content"] = "Changed task: run the probe."
				}
				body := map[string]any{"model": "gpt-test", "instructions": "Use tools.", "input": history}
				if !embeddedCatalog {
					body["tools"] = catalog
				}
				raw, err := json.Marshal(body)
				require.NoError(t, err)
				adapted, err := agentcompat.RewriteRequest(raw, false)
				require.NoError(t, err)
				stream, err := executor.DoStream(webSocketTestContext(), &httpclient.Request{
					Method: http.MethodPost, URL: server.URL + "/v1/responses",
					Headers: http.Header{webSocketSessionHeader: []string{"compat-continuation"}},
					Auth:    &httpclient.AuthConfig{Type: httpclient.AuthTypeBearer, APIKey: "test-key"}, Body: adapted,
				})
				require.NoError(t, err)
				require.True(t, stream.Next())
				var event struct {
					Response struct {
						Output []any `json:"output"`
					} `json:"response"`
				}
				restored, err := agentcompat.RestoreResponse(stream.Current().Data, false)
				require.NoError(t, err)
				require.NoError(t, json.Unmarshal(restored, &event))
				for _, raw := range event.Response.Output {
					// Codex clears this client-only marker when replaying to a
					// custom provider (core/src/client.rs, build_responses_request).
					delete(raw.(map[string]any), "encrypted_function_args")
				}
				require.False(t, stream.Next())
				require.NoError(t, stream.Err())
				require.NoError(t, stream.Close())
				select {
				case payload := <-payloads:
					input := payload["input"].([]any)
					if round == 0 || round == 4 {
						require.NotContains(t, payload, "previous_response_id")
					} else {
						require.Equal(t, fmt.Sprintf("resp_%d", round), payload["previous_response_id"])
						require.Len(t, input, 1, "send only the new tool result")
						require.Equal(t, "function_call_output", input[0].(map[string]any)["type"])
					}
					if !embeddedCatalog {
						if round == 0 {
							firstCatalog = payload["tools"]
						}
						require.Equal(t, firstCatalog, payload["tools"])
					}
				case <-time.After(5 * time.Second):
					t.Fatal("missing upstream request")
				}
				wantConnections := int32(1)
				if round == 4 {
					wantConnections = 2
				}
				require.Equal(t, wantConnections, upgrades.Load())
				history = append(history, event.Response.Output...)
				history = append(history, map[string]any{"type": "function_call_output", "call_id": fmt.Sprintf("call_%d", round+1), "output": "ok"})
			}
		})
	}
}
