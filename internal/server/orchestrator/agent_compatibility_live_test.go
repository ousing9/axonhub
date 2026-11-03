package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

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
	"github.com/looplj/axonhub/llm/oauth"
	"github.com/looplj/axonhub/llm/pipeline"
	"github.com/looplj/axonhub/llm/streams"
	"github.com/looplj/axonhub/llm/transformer"
	"github.com/looplj/axonhub/llm/transformer/openai/codex"
	"github.com/looplj/axonhub/llm/transformer/openai/responses"
	"github.com/looplj/axonhub/llm/transformer/shared"
)

type liveAgentReport struct {
	sync.Mutex
	Handshakes        map[string]int   `json:"handshakes"`
	Requests          map[string]int   `json:"requests"`
	Events            []map[string]any `json:"events"`
	WarmInput         int64            `json:"root_warm_input_tokens"`
	WarmCached        int64            `json:"root_warm_cached_tokens"`
	previousRootInput []any
}

func (r *liveAgentReport) event(v map[string]any) {
	r.Lock()
	defer r.Unlock()
	r.Events = append(r.Events, v)
}

// Missing usage is unknown, not a cache miss. Keep the response identity and
// original detail object so a later audit can distinguish omission from zero.
func liveAgentUsageEvent(model, session string, response gjson.Result) map[string]any {
	event := map[string]any{
		"model": model, "session": session, "response_id": response.Get("id").String(),
		"input_tokens_details": response.Get("usage.input_tokens_details").Value(),
	}
	for field, path := range map[string]string{
		"input_tokens":  "usage.input_tokens",
		"cached_tokens": "usage.input_tokens_details.cached_tokens",
	} {
		value := response.Get(path)
		if value.Type != gjson.Number {
			event[field] = nil
			event["usage_error"] = "missing or non-numeric token usage"
		} else {
			event[field] = value.Int()
		}
	}
	return event
}

func TestAgentCompatibilityLiveUsagePresence(t *testing.T) {
	for _, tc := range []struct {
		name, usage string
		cached      any
		invalid     bool
	}{
		{"explicit zero", `{"input_tokens":100,"input_tokens_details":{"cached_tokens":0}}`, int64(0), false},
		{"cache hit", `{"input_tokens":100,"input_tokens_details":{"cached_tokens":80}}`, int64(80), false},
		{"missing cached tokens", `{"input_tokens":100,"input_tokens_details":{}}`, nil, true},
		{"missing details", `{"input_tokens":100}`, nil, true},
		{"null cached tokens", `{"input_tokens":100,"input_tokens_details":{"cached_tokens":null}}`, nil, true},
		{"string cached tokens", `{"input_tokens":100,"input_tokens_details":{"cached_tokens":"0"}}`, nil, true},
		{"missing input", `{"input_tokens_details":{"cached_tokens":80}}`, int64(80), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			event := liveAgentUsageEvent("gpt-test", "session-test", gjson.Parse(`{"id":"resp-test","usage":`+tc.usage+`}`))
			// Verify the persisted report as well as the value used by the cache gate.
			encoded, err := json.Marshal(event)
			require.NoError(t, err)
			require.Equal(t, tc.cached, event["cached_tokens"])
			require.Equal(t, tc.invalid, gjson.GetBytes(encoded, "usage_error").Exists())
			require.Equal(t, "resp-test", event["response_id"])
			if tc.cached == nil {
				require.Equal(t, gjson.Null, gjson.GetBytes(encoded, "cached_tokens").Type)
			}
		})
	}
}

// Observe after all production request middleware, including header persistence.
// Embedding the concrete outbound preserves its passthrough/finalizer policies.
type liveAgentOutbound struct {
	*codex.OutboundTransformer
	report *liveAgentReport
}

func (o *liveAgentOutbound) CustomizeExecutor(e pipeline.Executor) pipeline.Executor {
	return &liveAgentExecutor{Executor: o.OutboundTransformer.CustomizeExecutor(e), report: o.report}
}

type liveAgentExecutor struct {
	pipeline.Executor
	report *liveAgentReport
}

func (e *liveAgentExecutor) DoStream(ctx context.Context, req *httpclient.Request) (streams.Stream[*httpclient.StreamEvent], error) {
	session, _ := shared.GetSessionID(ctx)
	key := gjson.GetBytes(req.Body, "model").String() + "/" + session
	e.report.Lock()
	e.report.Requests[key]++
	// Compare successive real outbound request prefixes. Inserting transient
	// guidance at the end of each request (the original regression) fails here.
	if gjson.GetBytes(req.Body, "model").String() == "gpt-6-astra" {
		var input []any
		if err := json.Unmarshal([]byte(gjson.GetBytes(req.Body, "input").Raw), &input); err != nil {
			e.report.Events = append(e.report.Events, map[string]any{"request_prefix_error": err.Error()})
		} else {
			previous := e.report.previousRootInput
			if len(input) < len(previous) || !reflect.DeepEqual(previous, input[:len(previous)]) && len(previous) > 0 {
				e.report.Events = append(e.report.Events, map[string]any{"request_prefix_error": "previous outbound input changed"})
			}
			e.report.previousRootInput = input
		}
	}
	e.report.Unlock()
	original := req.OnResponseHeaders
	req.OnResponseHeaders = func(ctx context.Context, h http.Header) {
		if h.Get("Sec-Websocket-Accept") != "" {
			e.report.Lock()
			e.report.Handshakes[key]++
			e.report.Unlock()
		}
		if original != nil {
			original(ctx, h)
		}
	}
	return e.Executor.DoStream(ctx, req)
}

// Paid, explicit opt-in acceptance. Uses real Process middleware and an in-memory
// test database. Endpoints are fixed to official providers; no running gateway,
// production database, personal Codex config, or project context is used.
func TestLiveAgentCompatibilityIsolated(t *testing.T) {
	if os.Getenv("AXONHUB_AGENT_ISOLATED_LIVE") != "1" {
		t.Skip("set AXONHUB_AGENT_ISOLATED_LIVE=1 for paid isolated acceptance")
	}
	var baseline, mixed *liveAgentReport
	if !t.Run("baseline", func(t *testing.T) { baseline = runLiveAgentCompatibility(t, false) }) {
		return
	}
	if !t.Run("mixed", func(t *testing.T) { mixed = runLiveAgentCompatibility(t, true) }) {
		return
	}
	baselineRatio := float64(baseline.WarmCached) / float64(baseline.WarmInput)
	mixedRatio := float64(mixed.WarmCached) / float64(mixed.WarmInput)
	t.Logf("warm GPT cache: baseline %.2f%%, mixed %.2f%%", 100*baselineRatio, 100*mixedRatio)
	// A live sample is variable; reject a material (>5 percentage point) loss.
	// Exact prefix/connection continuity is separately enforced by the deterministic
	// TestAgentCompatibilityPreservesWebSocketContinuation regression.
	require.GreaterOrEqual(t, mixedRatio, baselineRatio-0.05, "mixed agents must preserve warm GPT caching")
}

func runLiveAgentCompatibility(t *testing.T, mixed bool) *liveAgentReport {
	t.Helper()
	var cfg struct {
		OAuth         oauth.OAuthCredentials
		DeepSeekKey   string
		DeepSeekModel string
		DeepSeekURL   string
		Catalog       string
	}
	raw, err := os.ReadFile(os.Getenv("AXONHUB_AGENT_ISOLATED_CONFIG"))
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &cfg))
	require.NotEmpty(t, cfg.OAuth.AccessToken)
	require.NotEmpty(t, cfg.DeepSeekKey)
	if cfg.DeepSeekModel == "" {
		cfg.DeepSeekModel = "deepseek-flash"
	}
	if cfg.DeepSeekURL == "" {
		cfg.DeepSeekURL = "https://api.deepseek.com"
	}
	reportDir := os.Getenv("AXONHUB_AGENT_TEST_REPORT_DIR")
	require.NotEmpty(t, reportDir)
	mode := "baseline"
	if mixed {
		mode = "mixed"
	}
	reportDir = filepath.Join(reportDir, mode)
	work := t.TempDir()
	require.NoError(t, os.MkdirAll(reportDir, 0700))
	report := &liveAgentReport{Handshakes: map[string]int{}, Requests: map[string]int{}}
	defer func() {
		report.Lock()
		defer report.Unlock()
		b, _ := json.MarshalIndent(report, "", "  ")
		require.NoError(t, os.WriteFile(filepath.Join(reportDir, "pipeline-report.json"), b, 0600))
	}()
	db := enttest.NewEntClient(t, "sqlite3", "file:"+t.Name()+"?mode=memory&cache=shared&_fk=0&_busy_timeout=10000")
	defer db.Close()
	ctx := ent.NewContext(authz.WithTestBypass(t.Context()), db)
	project := createTestProject(t, ctx, db)
	channelService, requestService, systemService, usageLogService := setupTestServices(t, db)
	native, err := codex.NewOutboundTransformer(codex.Params{TokenProvider: oauth.NewStaticTokenProvider(&cfg.OAuth), Transport: responses.TransportWebSocket})
	require.NoError(t, err)
	defer native.Stop()
	portable, err := responses.NewOutboundTransformer(cfg.DeepSeekURL, cfg.DeepSeekKey)
	require.NoError(t, err)
	processors := map[string]*ChatCompletionOrchestrator{}
	var compatibility AgentCompatibilityConfig
	if mixed {
		compatibility.PortableModels = []string{"deepseek-flash", "deepseek-v4.1-flash"}
	}
	for _, spec := range []struct {
		models   []string
		kind     channel.Type
		out      transformer.Outbound
		settings *objects.ChannelSettings
		url      string
	}{
		{[]string{"gpt-6-astra", "gpt-6-luna"}, channel.TypeCodex, &liveAgentOutbound{native, report}, &objects.ChannelSettings{PassThroughBody: new(true)}, "https://chatgpt.com/backend-api/codex"},
		{[]string{cfg.DeepSeekModel}, channel.TypeOpenaiResponses, portable, &objects.ChannelSettings{PassThroughBody: new(true), TransformOptions: objects.TransformOptions{ReplaceDeveloperRoleWithSystem: true}}, cfg.DeepSeekURL},
	} {
		ch, err := db.Channel.Create().SetType(spec.kind).SetName(spec.models[0]).SetBaseURL(spec.url).
			SetCredentials(objects.ChannelCredentials{APIKey: "synthetic-not-used"}).SetSupportedModels(spec.models).SetDefaultTestModel(spec.models[0]).SetSettings(spec.settings).Save(ctx)
		require.NoError(t, err)
		for _, model := range spec.models {
			processors[model] = (&ChatCompletionOrchestrator{
				channelSelector: &staticChannelSelector{candidates: channelsToTestCandidates([]*biz.Channel{{Channel: ch, Outbound: spec.out}}, model)},
				Inbound:         responses.NewInboundTransformer(), RequestService: requestService, ChannelService: channelService,
				PromptProvider: &stubPromptProvider{}, SystemService: systemService, UsageLogService: usageLogService,
				PipelineFactory: pipeline.NewFactory(httpclient.NewHttpClient()), ModelMapper: NewModelMapper(), channelLimiterManager: NewChannelLimiterManager(),
			}).WithAgentCompatibility(compatibility)
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/v1/responses" {
			http.NotFound(w, r)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
		if err != nil {
			http.Error(w, "read failed", 400)
			return
		}
		model := gjson.GetBytes(body, "model").String()
		o := processors[model]
		if o == nil {
			http.Error(w, "model not allowed by isolated test", 400)
			return
		}
		session := r.Header.Get("Session_id")
		if session == "" {
			session = gjson.GetBytes(body, "prompt_cache_key").String()
		}
		if session == "" {
			http.Error(w, "session identity required", 400)
			return
		}
		rc := ent.NewContext(authz.WithTestBypass(r.Context()), db)
		rc = shared.WithResponsesAPI(shared.WithSessionID(shared.WithSessionScope(contexts.WithProjectID(rc, project.ID), "isolated-agent-test"), session))
		result, err := o.Process(rc, &httpclient.Request{Method: "POST", URL: r.URL.String(), Headers: r.Header.Clone(), Body: body})
		if err != nil {
			if errors.Is(err, context.Canceled) && errors.Is(r.Context().Err(), context.Canceled) {
				report.event(map[string]any{"model": model, "client_canceled": true})
			} else {
				report.event(map[string]any{"model": model, "error": err.Error()})
			}
			http.Error(w, err.Error(), 502)
			return
		}
		s := result.ChatCompletionStream
		if s == nil {
			http.Error(w, "stream expected", 502)
			return
		}
		defer s.Close()
		w.Header().Set("Content-Type", "text/event-stream")
		for s.Next() {
			ev := s.Current()
			v := gjson.ParseBytes(ev.Data)
			switch v.Get("type").String() {
			case "response.output_item.done":
				item := v.Get("item")
				if item.Get("type").String() == "function_call" {
					args := gjson.Parse(item.Get("arguments").String())
					nonce, _ := os.ReadFile(filepath.Join(work, "nonce.txt"))
					report.event(map[string]any{"target": args.Get("target").String(), "task_name": args.Get("task_name").String(), "nonce_reply": len(nonce) > 0 && strings.Contains(args.Get("message").String(), strings.TrimSpace(string(nonce))), "model": model, "session": session, "tool": item.Get("namespace").String() + "." + item.Get("name").String(), "plaintext_marker": item.Get("encrypted_function_args").Raw == "[]", "ciphertext": strings.Contains(item.Get("arguments").String(), "gAAAA")})
				}
			case "response.completed":
				report.event(liveAgentUsageEvent(model, session, v.Get("response")))
			case "error", "response.failed":
				report.event(map[string]any{"model": model, "failed_event": v.Get("type").String()})
			}
			if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Type, ev.Data); err != nil {
				return
			}
			w.(http.Flusher).Flush()
		}
		if err := s.Err(); err != nil {
			if errors.Is(err, context.Canceled) && errors.Is(r.Context().Err(), context.Canceled) {
				report.event(map[string]any{"model": model, "client_canceled": true})
			} else {
				report.event(map[string]any{"model": model, "stream_error": err.Error()})
			}
		}
	}))
	defer server.Close()
	home := t.TempDir()
	catalog, err := os.ReadFile(cfg.Catalog)
	require.NoError(t, err)
	catalogPath := filepath.Join(home, "models.json")
	require.NoError(t, os.WriteFile(catalogPath, catalog, 0600))
	prompt := `This is an explicitly authorized native V2 agent acceptance test. Use the normal GPT and DeepSeek model names. Spawn exactly one deepseek-flash child with task_name=ds_probe and fork_turns=none. Address subsequent messages and followup_task to /root/ds_probe. Its initial task: use the shell to compute 37*19 and write only 703 to ds-initial.txt; generate a random 16-hex-character nonce using the shell and write it to nonce.txt; explicitly send_message both results to /root; wait for a separate message before final completion. Immediately send that child a separate message to write only TOKEN-82ca to ds-message.txt. Wait for its initial work to complete. Send a followup_task to that SAME child to read nonce.txt, compute 23*17, write only 391 to ds-followup.txt, and explicitly send_message its result and the original nonce to /root. Also spawn one gpt-6-luna child using the native collaboration tool, fork_turns=none; ask it to compute 29*31 with the shell and write only 899 to luna.txt. Wait for all work, then read and verify all five files. Do not write these files yourself or perform the child tasks yourself. Do not spawn additional children. Return PASS and the received nonce only after all checks succeed. All work is in this directory: ` + work
	if !mixed {
		prompt = strings.Replace(prompt, "Spawn exactly one deepseek-flash child", "Spawn exactly one gpt-6-luna child", 1)
	} else {
		prompt = strings.ReplaceAll(prompt, "deepseek-flash", cfg.DeepSeekModel)
	}
	runCtx, cancel := context.WithTimeout(t.Context(), 8*time.Minute)
	defer cancel()
	args := []string{"exec", "--skip-git-repo-check", "--ephemeral", "--json", "-C", work,
		"-c", `model="gpt-6-astra"`, "-c", `model_reasoning_effort="low"`, "-c", `model_provider="isolated_test"`,
		"-c", `model_providers.isolated_test.name="Isolated compatibility test"`, "-c", `model_providers.isolated_test.base_url="` + server.URL + `/v1"`,
		"-c", `model_providers.isolated_test.wire_api="responses"`, "-c", `model_providers.isolated_test.env_key="ISOLATED_TEST_KEY"`,
		"-c", `model_catalog_json="` + catalogPath + `"`, "-c", `features.multi_agent_v2.enabled=true`, "-c", `features.hooks=false`,
		"-c", `approval_policy="never"`, "-c", `sandbox_mode="danger-full-access"`, prompt}
	cmd := exec.CommandContext(runCtx, "codex", args...)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"), "CODEX_HOME=" + home, "ISOLATED_TEST_KEY=synthetic-not-used", "LANG=C.UTF-8"}
	output, runErr := cmd.CombinedOutput()
	require.NoError(t, os.WriteFile(filepath.Join(reportDir, "native-cli.jsonl"), output, 0600))
	require.NoError(t, runErr, "see protected runtime report")
	server.Close() // Drain handlers before inspecting the complete report.
	for name, want := range map[string]string{"ds-initial.txt": "703", "ds-followup.txt": "391", "ds-message.txt": "TOKEN-82ca", "luna.txt": "899"} {
		data, err := os.ReadFile(filepath.Join(work, name))
		require.NoError(t, err)
		require.Equal(t, want, strings.TrimSpace(string(data)))
	}
	nonce, err := os.ReadFile(filepath.Join(work, "nonce.txt"))
	require.NoError(t, err)
	require.Regexp(t, `^[0-9a-f]{16}$`, strings.TrimSpace(string(nonce)))
	require.Contains(t, string(output), "PASS "+strings.TrimSpace(string(nonce)))
	report.Lock()
	defer report.Unlock()
	var childMessages, plainCalls, nativeCalls, nonceReplies, rootRounds int
	lifecycle := map[string]int{}
	for _, ev := range report.Events {
		require.Nil(t, ev["usage_error"], "missing usage cannot be treated as a zero-cache response")
		require.Nil(t, ev["request_prefix_error"])
		require.Nil(t, ev["error"])
		require.Nil(t, ev["stream_error"])
		require.Nil(t, ev["failed_event"])
		if ev["model"] == cfg.DeepSeekModel && ev["tool"] == "collaboration.send_message" && ev["plaintext_marker"] == true {
			childMessages++
			if ev["nonce_reply"] == true && ev["target"] == "/root" {
				nonceReplies++
			}
		}
		if ev["model"] == "gpt-6-astra" && ev["plaintext_marker"] == true {
			plainCalls++
			tool, _ := ev["tool"].(string)
			lifecycle[tool]++
			if tool == "collaboration.spawn_agent" {
				require.Equal(t, "ds_probe", ev["task_name"])
			}
			if tool == "collaboration.send_message" || tool == "collaboration.followup_task" {
				require.Equal(t, "/root/ds_probe", ev["target"])
			}
		}
		if ev["model"] == "gpt-6-astra" && ev["tool"] == "collaboration.spawn_agent" && ev["ciphertext"] == true {
			nativeCalls++
		}
		if ev["model"] == "gpt-6-astra" {
			if input, ok := ev["input_tokens"].(int64); ok {
				rootRounds++
				if rootRounds > 1 {
					report.WarmInput += input
					report.WarmCached += ev["cached_tokens"].(int64)
				}
			}
		}
	}
	if mixed {
		require.Equal(t, 1, lifecycle["collaboration.spawn_agent"])
		require.GreaterOrEqual(t, lifecycle["collaboration.send_message"], 1)
		require.GreaterOrEqual(t, lifecycle["collaboration.followup_task"], 1)
		require.GreaterOrEqual(t, nonceReplies, 2)
		require.GreaterOrEqual(t, childMessages, 2)
		require.GreaterOrEqual(t, plainCalls, 3)
		require.Equal(t, 1, nativeCalls)
	} else {
		require.Zero(t, plainCalls)
	}
	require.Greater(t, report.WarmInput, int64(0))
	require.Greater(t, report.WarmCached, int64(0))
	for key, n := range report.Requests {
		require.Greater(t, n, 1)
		// Codex 0.166 changes output metadata when replaying history, even with
		// adaptation disabled. Record reconnects; do not mistake them for cache misses.
		require.Positive(t, report.Handshakes[key])
		require.LessOrEqual(t, report.Handshakes[key], n)
		t.Logf("%s: %d requests, %d handshakes", key, n, report.Handshakes[key])
	}
	return report
}
