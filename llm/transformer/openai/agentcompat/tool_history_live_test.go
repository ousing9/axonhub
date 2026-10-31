package agentcompat

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// Opt-in paid acceptance. All prompts and results are synthetic; no project or
// production conversation is copied into a fixture or sent to another provider.
func TestLivePortableToolHistory(t *testing.T) {
	if os.Getenv("AXONHUB_AGENT_LIVE") != "1" {
		t.Skip("set AXONHUB_AGENT_LIVE=1 for live DeepSeek acceptance")
	}
	upstream := strings.TrimRight(os.Getenv("AXONHUB_AGENT_TEST_URL"), "/")
	require.NotEmpty(t, upstream)
	client := &http.Client{Timeout: 90 * time.Second}
	post := func(body []byte) (int, []byte) {
		r, err := http.NewRequestWithContext(t.Context(), http.MethodPost, upstream+"/v1/responses", bytes.NewReader(body))
		require.NoError(t, err)
		r.Header.Set("Authorization", "Bearer "+os.Getenv("OPENAI_API_KEY"))
		r.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(r)
		require.NoError(t, err)
		defer resp.Body.Close()
		data, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		return resp.StatusCode, data
	}
	for _, stream := range []bool{false, true} {
		for _, role := range []string{"developer", "system"} {
			t.Run(fmt.Sprintf("stream=%t/role=%s", stream, role), func(t *testing.T) {
				body := []byte(fmt.Sprintf(`{"model":"deepseek-flash","stream":%t,"store":false,"max_output_tokens":256,"input":[
				{"role":"user","content":"Report both probe results, then say HISTORY_OK."},
				{"type":"reasoning","content":[{"type":"reasoning_text","text":"I will run the two probes."}],"summary":[]},
				{"type":"function_call","call_id":"probe_a","name":"probe","arguments":"{}"},
				{"type":"message","role":"%s","content":"Preserve both probe results."},
				{"type":"function_call","call_id":"probe_b","name":"probe","arguments":"{}"},
				{"type":"function_call_output","call_id":"probe_a","output":"703"},
				{"type":"function_call_output","call_id":"probe_b","output":"391"}],
				"tools":[{"type":"function","name":"probe","parameters":{"type":"object","properties":{}}}]}`, stream, role))
				if os.Getenv("AXONHUB_HISTORY_EXPECT_UNPATCHED") == "1" {
					status, data := post(body)
					require.Equal(t, 400, status)
					require.Contains(t, string(data), "No tool output found")
				}
				fixed, err := RewriteRequest(body, true)
				require.NoError(t, err)
				status, data := post(fixed)
				require.Equal(t, 200, status)
				require.Contains(t, string(data), "HISTORY_OK")
				require.Contains(t, string(data), "703")
				require.Contains(t, string(data), "391")
				if stream {
					require.Contains(t, string(data), `"type":"response.completed"`)
				} else {
					var response map[string]any
					require.NoError(t, json.Unmarshal(data, &response))
					require.Equal(t, "completed", response["status"])
				}
				// A truly missing result must still fail upstream.
				v, err := decode(body)
				require.NoError(t, err)
				items := v["input"].([]any)
				v["input"] = items[:len(items)-1]
				missing, err := json.Marshal(v)
				require.NoError(t, err)
				missing, err = RewriteRequest(missing, true)
				require.NoError(t, err)
				status, _ = post(missing)
				require.Equal(t, 400, status)
			})
		}
	}
}

func TestLivePortableMainAgent(t *testing.T) {
	if os.Getenv("AXONHUB_AGENT_LIVE") != "1" {
		t.Skip("set AXONHUB_AGENT_LIVE=1 for live DeepSeek main-agent acceptance")
	}
	upstream := strings.TrimRight(os.Getenv("AXONHUB_AGENT_TEST_URL"), "/")
	require.NotEmpty(t, upstream)
	var injected atomic.Int32
	client := &http.Client{Timeout: 90 * time.Second}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
		if err != nil {
			http.Error(w, "read failed", 400)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/responses") && gjson.GetBytes(body, "model").String() == "deepseek-flash" {
			v, decodeErr := decode(body)
			if decodeErr != nil {
				http.Error(w, "invalid JSON", 400)
				return
			}
			items, _ := v["input"].([]any)
			for i, raw := range items {
				if text(object(raw)["type"]) == "function_call" {
					// Deterministically reproduce Codex path-guidance insertion.
					guidance := map[string]any{"type": "message", "role": "developer", "content": "Synthetic path guidance: preserve the existing task and report the actual shell results."}
					next := append([]any{}, items[:i+1]...)
					next = append(next, guidance)
					v["input"] = append(next, items[i+1:]...)
					injected.Add(1)
					break
				}
			}
			body, err = json.Marshal(v)
			if err == nil {
				body, err = RewriteRequest(body, true)
			}
			if err != nil {
				http.Error(w, "rewrite failed", 400)
				return
			}
		}
		request, err := http.NewRequestWithContext(r.Context(), r.Method, upstream+r.URL.Path, bytes.NewReader(body))
		if err != nil {
			http.Error(w, "request failed", 500)
			return
		}
		request.Header = r.Header.Clone()
		request.Header.Del("Accept-Encoding")
		resp, err := client.Do(request)
		if err != nil {
			http.Error(w, "upstream unavailable", 502)
			return
		}
		defer resp.Body.Close()
		w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
		w.WriteHeader(resp.StatusCode)
		// Flush every chunk so Codex sees a live stream.
		buf := make([]byte, 4096)
		for {
			n, err := resp.Body.Read(buf)
			if n > 0 {
				if _, writeErr := w.Write(buf[:n]); writeErr != nil {
					return
				}
				w.(http.Flusher).Flush()
			}
			if err != nil {
				return
			}
		}
	}))
	defer server.Close()
	for _, parent := range []string{"deepseek-flash", "gpt-6-astra"} {
		t.Run(parent, func(t *testing.T) {
			injected.Store(0)
			dir := t.TempDir()
			prompt := "Use shell tools to compute 37*19 and 23*17 in separate calls, preferably in parallel. Then use another shell call to save their actual results as two lines in result.txt. Read result.txt to verify it. Do not delegate. Reply MAIN_HISTORY_OK after verification."
			if parent != "deepseek-flash" {
				prompt = "This is an authorized native V2 mixed-agent acceptance test. Spawn a deepseek-flash subagent with fork_turns=none to use shell tools to compute 37*19 and 23*17 separately, write the results as two lines in result.txt, read it, and explicitly send_message the result to /root. Wait for completion, then send a followup_task to that SAME child asking it to use a shell to compute 17*19 and save 323 in followup.txt and send_message to /root. Also spawn a gpt-6-luna native subagent with fork_turns=none to compute 29*31 using shell and write 899 in luna.txt. Wait for all results and verify all three files. Do not do the children's computations or write their files yourself. Return MAIN_HISTORY_OK after verification. All files must be written in this shared working directory: " + dir
			}
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
			defer cancel()
			args := []string{"exec", "--skip-git-repo-check", "--ephemeral", "--json", "-C", dir,
				"-c", `model="` + parent + `"`, "-c", `model_reasoning_effort="low"`,
				"-c", `model_provider="history_test"`, "-c", `model_providers.history_test.name="history test"`,
				"-c", `model_providers.history_test.base_url="` + server.URL + `/v1"`,
				"-c", `model_providers.history_test.wire_api="responses"`,
				"-c", `model_providers.history_test.env_key="OPENAI_API_KEY"`,
				"-c", `features.hooks=false`, "-c", `approval_policy="never"`, "-c", `sandbox_mode="danger-full-access"`,
				prompt}
			output, err := exec.CommandContext(ctx, "codex", args...).CombinedOutput()
			require.NoError(t, err, "Codex failed: %s", tail(output, 1500))
			require.Positive(t, injected.Load(), "acceptance must exercise injected guidance")
			result, err := os.ReadFile(filepath.Join(dir, "result.txt"))
			require.NoError(t, err)
			require.Equal(t, "703\n391", strings.TrimSpace(string(result)))
			require.Contains(t, string(output), "MAIN_HISTORY_OK")
			require.NotContains(t, string(output), "No tool output found")
			if parent != "deepseek-flash" {
				for name, expected := range map[string]string{"followup.txt": "323", "luna.txt": "899"} {
					data, err := os.ReadFile(filepath.Join(dir, name))
					require.NoError(t, err)
					require.Equal(t, expected, strings.TrimSpace(string(data)))
				}
			}
			t.Logf("parent=%s completed; injected guidance in %d DeepSeek continuation requests", parent, injected.Load())
		})
	}
}
