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
	"github.com/tidwall/gjson"

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

func TestPortableResponsesHistoryPipeline(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		for _, enabled := range []bool{false, true} {
			for _, replaceRole := range []bool{false, true} {
				t.Run(fmt.Sprintf("stream=%t/compat=%t/replace=%t", streaming, enabled, replaceRole), func(t *testing.T) {
					upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						body, err := io.ReadAll(r.Body)
						require.NoError(t, err)
						items := gjson.GetBytes(body, "input").Array()
						var kinds []string
						for _, item := range items {
							kinds = append(kinds, item.Get("type").String())
						}
						instruction := 1
						if enabled {
							assert.Equal(t, []string{"function_call", "function_call", "function_call_output", "function_call_output", "message"}, kinds)
							instruction = 4
						} else {
							assert.Equal(t, []string{"function_call", "message", "function_call", "function_call_output", "function_call_output"}, kinds)
						}
						role := "developer"
						if enabled && replaceRole {
							role = "system"
						}
						assert.Equal(t, role, items[instruction].Get("role").String())
						assert.Equal(t, "path guidance", items[instruction].Get("content").String())
						response := `{"id":"resp_test","object":"response","status":"completed","model":"deepseek-flash","output":[{"type":"message","id":"msg_test","role":"assistant","status":"completed","content":[{"type":"output_text","text":"OK"}]}],"usage":{"input_tokens":10,"output_tokens":1,"total_tokens":11}}`
						if streaming {
							w.Header().Set("Content-Type", "text/event-stream")
							// DeepSeek marks both tool preambles and final answers as
							// final_answer at start; only done carries the decided phase.
							for _, phase := range []string{"commentary", "final_answer"} {
								fmt.Fprintf(w, "event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"id\":\"msg_%s\",\"phase\":\"final_answer\",\"content\":[]}}\n\n", phase)
								fmt.Fprintf(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"item_id\":\"msg_%s\",\"delta\":\"OK\"}\n\n", phase)
								fmt.Fprintf(w, "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"id\":\"msg_%s\",\"phase\":\"%s\",\"content\":[{\"type\":\"output_text\",\"text\":\"OK\"}]}}\n\n", phase, phase)
							}
							fmt.Fprintf(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":%s}\n\n", response)
						} else {
							w.Header().Set("Content-Type", "application/json")
							fmt.Fprint(w, response)
						}
					}))
					defer upstream.Close()
					client := enttest.NewEntClient(t, "sqlite3", "file:"+t.Name()+"?mode=memory&_fk=0")
					defer client.Close()
					ctx := ent.NewContext(authz.WithTestBypass(t.Context()), client)
					project := createTestProject(t, ctx, client)
					ch, err := client.Channel.Create().SetType(channel.TypeOpenaiResponses).SetName("Portable Responses").
						SetBaseURL(upstream.URL + "/v1").SetCredentials(objects.ChannelCredentials{APIKey: "test-key"}).
						SetSettings(&objects.ChannelSettings{PassThroughBody: new(true), TransformOptions: objects.TransformOptions{ReplaceDeveloperRoleWithSystem: replaceRole}}).
						SetSupportedModels([]string{"deepseek-flash"}).SetDefaultTestModel("deepseek-flash").Save(ctx)
					require.NoError(t, err)
					outbound, err := responses.NewOutboundTransformer(ch.BaseURL, ch.Credentials.APIKey)
					require.NoError(t, err)
					channelService, requestService, systemService, usageLogService := setupTestServices(t, client)
					o := &ChatCompletionOrchestrator{
						channelSelector: &staticChannelSelector{candidates: channelsToTestCandidates([]*biz.Channel{{Channel: ch, Outbound: outbound}}, "deepseek-flash")},
						Inbound:         responses.NewInboundTransformer(), RequestService: requestService,
						ChannelService: channelService, PromptProvider: &stubPromptProvider{},
						SystemService: systemService, UsageLogService: usageLogService,
						PipelineFactory: pipeline.NewFactory(httpclient.NewHttpClientWithClient(upstream.Client())),
						ModelMapper:     NewModelMapper(), channelLimiterManager: NewChannelLimiterManager(),
					}
					if enabled {
						o = o.WithAgentCompatibility(AgentCompatibilityConfig{PortableModels: []string{"deepseek-flash"}})
					}
					ctx = shared.WithResponsesAPI(contexts.WithProjectID(ctx, project.ID))
					body := fmt.Sprintf(`{"model":"deepseek-flash","stream":%t,"input":[{"type":"function_call","name":"probe","call_id":"a","arguments":"{}"},{"type":"message","role":"developer","content":"path guidance"},{"type":"function_call","name":"probe","call_id":"b","arguments":"{}"},{"type":"function_call_output","call_id":"a","output":"one"},{"type":"function_call_output","call_id":"b","output":"two"}]}`, streaming)
					result, err := o.Process(ctx, &httpclient.Request{Method: "POST", URL: "/v1/responses", Body: []byte(body)})
					require.NoError(t, err)
					if streaming {
						defer result.ChatCompletionStream.Close()
						completed := false
						var starts, ends, deltas []string
						for result.ChatCompletionStream.Next() {
							event := gjson.ParseBytes(result.ChatCompletionStream.Current().Data)
							switch event.Get("type").String() {
							case "response.output_item.added":
								starts = append(starts, event.Get("item.phase").String())
							case "response.output_item.done":
								ends = append(ends, event.Get("item.phase").String())
							case "response.output_text.delta":
								deltas = append(deltas, event.Get("delta").String())
							case "response.completed":
								completed = true
							}
						}
						require.NoError(t, result.ChatCompletionStream.Err())
						require.True(t, completed)
						if enabled {
							require.Equal(t, []string{"commentary", "commentary"}, starts)
						} else {
							require.Equal(t, []string{"final_answer", "final_answer"}, starts)
						}
						require.Equal(t, []string{"commentary", "final_answer"}, ends)
						require.Equal(t, []string{"OK", "OK"}, deltas)
					} else {
						require.Contains(t, string(result.ChatCompletion.Body), "OK")
					}
				})
			}
		}
	}
}

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
