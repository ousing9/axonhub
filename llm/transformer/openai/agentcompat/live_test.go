package agentcompat

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Explicitly opt in: this test makes paid requests through the caller's gateway.
// The HTTP fixture exists only for the test; deployment uses the AxonHub middleware.
func TestLiveNativeCodex(t *testing.T) {
	if os.Getenv("AXONHUB_AGENT_LIVE") != "1" {
		t.Skip("set AXONHUB_AGENT_LIVE=1 for live native-agent acceptance")
	}
	upstream := os.Getenv("AXONHUB_AGENT_TEST_URL")
	require.NotEmpty(t, upstream)
	catalog := os.Getenv("AXONHUB_AGENT_TEST_CATALOG")
	require.NotEmpty(t, catalog)
	deployed := os.Getenv("AXONHUB_AGENT_TEST_DEPLOYED") == "1"
	portableModel := os.Getenv("AXONHUB_AGENT_TEST_PORTABLE_MODEL")
	if portableModel == "" {
		portableModel = "deepseek-flash"
	}
	dir := t.TempDir()
	var mu sync.Mutex
	var observations []string
	observe := func(s string) { mu.Lock(); observations = append(observations, s); mu.Unlock() }
	client := &http.Client{Timeout: 120 * time.Second}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		var v map[string]any
		_ = json.Unmarshal(body, &v)
		model := text(v["model"])
		portable := model == portableModel
		isResponses := strings.HasSuffix(r.URL.Path, "/responses")
		if isResponses {
			if !deployed {
				body, err = RewriteRequest(body, portable, []string{portableModel})
			}
			if err != nil {
				observe("rewrite-error: " + err.Error())
				http.Error(w, err.Error(), 400)
				return
			}
			observe("request-model: " + model)
		}
		request, err := http.NewRequestWithContext(r.Context(), r.Method, strings.TrimRight(upstream, "/")+r.URL.Path, bytes.NewReader(body))
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		request.Header = r.Header.Clone()
		request.Header.Del("Accept-Encoding")
		response, err := client.Do(request)
		if err != nil {
			observe("upstream-error")
			http.Error(w, "upstream unavailable", 502)
			return
		}
		defer response.Body.Close()
		for k, vs := range response.Header {
			if k != "Content-Length" && k != "Content-Encoding" {
				for _, val := range vs {
					w.Header().Add(k, val)
				}
			}
		}
		w.WriteHeader(response.StatusCode)
		if response.StatusCode != 200 || !isResponses {
			_, _ = io.Copy(w, response.Body)
			return
		}
		if !strings.Contains(response.Header.Get("Content-Type"), "text/event-stream") {
			observe("unexpected non-SSE response")
			_, _ = io.Copy(w, response.Body)
			return
		}
		reader := bufio.NewReader(response.Body)
		for {
			line, readErr := reader.ReadBytes('\n')
			if bytes.HasPrefix(line, []byte("data: ")) {
				data := bytes.TrimSpace(line[6:])
				var event map[string]any
				_ = json.Unmarshal(data, &event)
				if text(event["type"]) == "response.output_item.done" {
					item := object(event["item"])
					if text(item["type"]) == "function_call" {
						observe("call: " + model + " " + text(item["namespace"]) + "." + text(item["name"]) + " encrypted=" + boolText(strings.Contains(text(item["arguments"]), "gAAAA")))
					}
				}
				fixed, fixErr := data, error(nil)
				if !deployed {
					fixed, fixErr = RestoreResponse(data, portable, []string{portableModel})
				}
				if fixErr != nil {
					observe("restore-error: " + fixErr.Error())
					return
				}
				line = append(append([]byte("data: "), fixed...), '\n')
			}
			if len(line) > 0 {
				if _, err := w.Write(line); err != nil {
					return
				}
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
			}
			if readErr != nil {
				return
			}
		}
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	prompt := `This is an explicitly authorized native V2 subagent acceptance test. Keep all agents on V2. Spawn a deepseek-flash child with fork_turns=none. Ask it to use a shell tool to compute 37*19 and write only 703 to ds-initial.txt in this working directory, then explicitly send_message its result to /root. Wait for completion. Send a followup task to that SAME child to compute 23*17 with a shell tool, write only 391 to ds-followup.txt, and send_message its result to /root. Immediately after spawning the DeepSeek child, send it a separate message asking it to write only TOKEN-82ca into ds-message.txt; tell its initial task to wait for this message before final completion. Also spawn a gpt-6-luna child with fork_turns=none using the standard native collaboration tool; have it use a shell tool to compute 29*31 and write only 899 to luna.txt. Also spawn a gpt-6.1-sol native child with fork_turns=none to use a shell tool to compute 31*37 and write only 1147 to sol.txt. Wait for all tasks and inspect all five files. Do not execute child computations or write their files yourself. Do not spawn other models. Return PASS only after checking all five artifacts and both DeepSeek messages. Working directory: ` + dir
	prompt = strings.ReplaceAll(prompt, "deepseek-flash", portableModel)
	parent := os.Getenv("AXONHUB_AGENT_TEST_PARENT")
	if parent == "" {
		parent = "gpt-6.1-sol"
	}
	args := []string{"exec", "--skip-git-repo-check", "--ephemeral", "--json", "-C", dir,
		"-c", `model="` + parent + `"`, "-c", `model_reasoning_effort="low"`, "-c", `model_provider="agent_compat_test"`,
		"-c", `model_providers.agent_compat_test.name="AxonHub compatibility acceptance"`,
		"-c", `model_providers.agent_compat_test.base_url="` + server.URL + `/v1"`,
		"-c", `model_providers.agent_compat_test.wire_api="responses"`,
		"-c", `model_providers.agent_compat_test.env_key="OPENAI_API_KEY"`,
		"-c", `model_catalog_json="` + catalog + `"`, "-c", `features.multi_agent_v2.enabled=true`,
		"-c", `features.hooks=false`, "-c", `approval_policy="never"`, "-c", `sandbox_mode="danger-full-access"`, prompt}
	cmd := exec.CommandContext(ctx, "codex", args...)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"), "CODEX_HOME=" + t.TempDir(), "OPENAI_API_KEY=" + os.Getenv("OPENAI_API_KEY"), "LANG=C.UTF-8"}
	output, err := cmd.CombinedOutput()
	if reportDir := os.Getenv("AXONHUB_AGENT_TEST_REPORT_DIR"); reportDir != "" {
		require.NoError(t, os.MkdirAll(reportDir, 0700))
		require.NoError(t, os.WriteFile(filepath.Join(reportDir, "native-cli.jsonl"), output, 0600))
		mu.Lock()
		encoded, _ := json.MarshalIndent(observations, "", "  ")
		mu.Unlock()
		require.NoError(t, os.WriteFile(filepath.Join(reportDir, "wire-observations.json"), encoded, 0600))
	}
	mu.Lock()
	observed := strings.Join(observations, "\n")
	mu.Unlock()
	t.Log(observed)
	require.NoError(t, err, "CLI tail: %s", tail(output, 4000))
	for _, model := range []string{parent, portableModel, "gpt-6-luna", "gpt-6.1-sol"} {
		require.Contains(t, observed, "request-model: "+model)
	}
	for _, name := range []string{"dispatch_task", "deliver_message", "continue_task"} {
		namespace := "axonhub_v2"
		if deployed {
			namespace = "collaboration"
			for original, alias := range aliases {
				if name == alias {
					name = original
					break
				}
			}
		}
		require.Contains(t, observed, "call: "+parent+" "+namespace+"."+name+" encrypted=false")
	}
	require.GreaterOrEqual(t, strings.Count(observed, "call: "+parent+" collaboration.spawn_agent encrypted=true"), 2)
	require.GreaterOrEqual(t, strings.Count(observed, "call: "+portableModel+" collaboration.send_message encrypted=false"), 2)
	require.NotContains(t, observed, "rewrite-error:")
	require.NotContains(t, observed, "restore-error:")
	for name, want := range map[string]string{"ds-initial.txt": "703", "ds-followup.txt": "391", "luna.txt": "899", "sol.txt": "1147", "ds-message.txt": "TOKEN-82ca"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		require.NoError(t, err, "CLI tail: %s", tail(output, 4000))
		require.Equal(t, want, strings.TrimSpace(string(data)))
	}
}

func boolText(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
func tail(b []byte, n int) string {
	if len(b) > n {
		b = b[len(b)-n:]
	}
	return string(b)
}
