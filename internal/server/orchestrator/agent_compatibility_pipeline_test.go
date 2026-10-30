package orchestrator

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/pipeline"
	"github.com/looplj/axonhub/llm/transformer/deepseek"
	"github.com/looplj/axonhub/llm/transformer/openai/responses"
	"github.com/looplj/axonhub/llm/transformer/shared"
)

// Exercise Process itself: normalization must happen before the Responses
// parser, and the plaintext marker must be added after Chat -> Responses conversion.
func TestAgentCompatibilityPortablePipeline(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", streaming), func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if !assert.NoError(t, err) || !assert.Contains(t, string(body), "Compute 37*19") ||
					!assert.Equal(t, "/v1/chat/completions", r.URL.Path) {
					http.Error(w, "lost task", 400)
					return
				}
				call := `{"id":"call_1","type":"function","function":{"name":"collaboration__send_message","arguments":"{\"target\":\"/root\",\"message\":\"703\"}"}}`
				if !streaming {
					w.Header().Set("Content-Type", "application/json")
					fmt.Fprintf(w, `{"id":"test","object":"chat.completion","model":"deepseek-flash","choices":[{"index":0,"message":{"role":"assistant","tool_calls":[%s]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":10,"total_tokens":20}}`, call)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				call = strings.Replace(call, `{"id":`, `{"index":0,"id":`, 1)
				fmt.Fprintf(w, "data: %s\n\n", fmt.Sprintf(`{"id":"test","object":"chat.completion.chunk","model":"deepseek-flash","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[%s]},"finish_reason":null}]}`, call))
				fmt.Fprint(w, "data: {\"id\":\"test\",\"object\":\"chat.completion.chunk\",\"model\":\"deepseek-flash\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":10,\"total_tokens\":20}}\n\ndata: [DONE]\n\n")
			}))
			defer upstream.Close()
			client := enttest.NewEntClient(t, "sqlite3", "file:"+t.Name()+"?mode=memory&_fk=0")
			defer client.Close()
			ctx := ent.NewContext(authz.WithTestBypass(t.Context()), client)
			project := createTestProject(t, ctx, client)
			ch, err := client.Channel.Create().SetType(channel.TypeDeepseek).SetName("Portable test").
				SetBaseURL(upstream.URL + "/v1").SetCredentials(objects.ChannelCredentials{APIKey: "test-key"}).
				SetSupportedModels([]string{"deepseek-flash"}).SetDefaultTestModel("deepseek-flash").Save(ctx)
			require.NoError(t, err)
			outbound, err := deepseek.NewOutboundTransformer(ch.BaseURL, ch.Credentials.APIKey)
			require.NoError(t, err)
			channelService, requestService, systemService, usageLogService := setupTestServices(t, client)
			o := (&ChatCompletionOrchestrator{
				channelSelector: &staticChannelSelector{candidates: channelsToTestCandidates([]*biz.Channel{{Channel: ch, Outbound: outbound}}, "deepseek-flash")},
				Inbound:         responses.NewInboundTransformer(), RequestService: requestService,
				ChannelService: channelService, PromptProvider: &stubPromptProvider{},
				SystemService: systemService, UsageLogService: usageLogService,
				PipelineFactory: pipeline.NewFactory(httpclient.NewHttpClientWithClient(upstream.Client())),
				ModelMapper:     NewModelMapper(), channelLimiterManager: NewChannelLimiterManager(),
			}).WithAgentCompatibility(AgentCompatibilityConfig{PortableModels: []string{"deepseek-flash"}})
			ctx = shared.WithResponsesAPI(contexts.WithProjectID(ctx, project.ID))
			body := fmt.Sprintf(`{"model":"deepseek-flash","stream":%t,"input":[{"type":"agent_message","content":[{"type":"input_text","text":"Compute 37*19"}]}],"tools":[{"type":"namespace","name":"collaboration","tools":[{"type":"function","name":"send_message","parameters":{"type":"object","properties":{"target":{"type":"string"},"message":{"type":"string"}}}}]}]}`, streaming)
			result, err := o.Process(ctx, &httpclient.Request{Method: "POST", URL: "/v1/responses", Body: []byte(body)})
			require.NoError(t, err)
			if !streaming {
				require.Contains(t, string(result.ChatCompletion.Body), `"encrypted_function_args":[]`)
				return
			}
			defer result.ChatCompletionStream.Close()
			found := false
			for result.ChatCompletionStream.Next() {
				event := result.ChatCompletionStream.Current()
				var v struct {
					Type string `json:"type"`
					Item struct {
						Name      string          `json:"name"`
						Namespace string          `json:"namespace"`
						Encrypted json.RawMessage `json:"encrypted_function_args"`
					} `json:"item"`
				}
				if json.Unmarshal(event.Data, &v) == nil && v.Type == "response.output_item.done" && v.Item.Name == "send_message" {
					found = true
					require.Equal(t, "collaboration", v.Item.Namespace)
					require.JSONEq(t, "[]", string(v.Item.Encrypted))
				}
			}
			require.NoError(t, result.ChatCompletionStream.Err())
			require.True(t, found, "expected a restored native collaboration call")
		})
	}
}
