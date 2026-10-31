package agentcompat

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPortableToolBatchGuidance(t *testing.T) {
	// Mirrors the failed desktop request: one call, injected path guidance,
	// four more calls, then five outputs (including a multipart image result).
	input := []byte(`{"model":"deepseek-flash","input":[
		{"type":"reasoning","content":[{"type":"reasoning_text","text":"inspect"}],"encrypted_content":"opaque"},
		{"type":"function_call","call_id":"a","name":"exec_command","arguments":"{}"},
		{"type":"message","role":"developer","content":"docs/AGENTS.md guidance"},
		{"type":"function_call","call_id":"b","arguments":"{}"},
		{"type":"function_call","call_id":"c","arguments":"{}"},
		{"type":"function_call","call_id":"d","arguments":"{}"},
		{"type":"function_call","call_id":"e","arguments":"{}"},
		{"type":"function_call_output","call_id":"b","output":"second"},
		{"role":"system","content":"another hook"},
		{"type":"function_call_output","call_id":"a","output":[{"type":"input_text","text":"first"},{"type":"input_image","image_url":"data:image/png;base64,fixture"}]},
		{"type":"function_call_output","call_id":"c","output":"third","counter":9223372036854775807},
		{"type":"function_call_output","call_id":"d","output":"fourth"},
		{"type":"function_call_output","call_id":"e","output":"fifth"},
		{"role":"user","content":"next turn"},
		{"type":"custom_tool_call","call_id":"patch","name":"apply_patch","input":"patch content"},
		{"role":"developer","content":"patch guidance"},
		{"type":"custom_tool_call_output","call_id":"patch","output":"failed: fixture"}
	]}`)
	before, err := decode(input)
	require.NoError(t, err)
	got, err := RewriteRequest(input, true)
	require.NoError(t, err)
	after, err := decode(got)
	require.NoError(t, err)
	source := before["input"].([]any)
	var want []any
	for _, index := range []int{0, 1, 3, 4, 5, 6, 7, 9, 10, 11, 12, 2, 8, 13, 14, 16, 15} {
		want = append(want, source[index])
	}
	require.Equal(t, want, after["input"], "all payloads, numeric precision and relative orders must survive")
	second, err := RewriteRequest(got, true)
	require.NoError(t, err)
	require.Equal(t, got, second, "normalization must be idempotent")
	native, err := RewriteRequest(input, false)
	require.NoError(t, err)
	require.Equal(t, input, native, "GPT history must remain byte-identical")
}

func TestPortableToolHistoryBoundaries(t *testing.T) {
	a := `{"type":"function_call","call_id":"a","arguments":"{}"}`
	b := `{"type":"function_call","call_id":"b","arguments":"{}"}`
	oa := `{"type":"function_call_output","call_id":"a","output":"ok"}`
	ob := `{"type":"function_call_output","call_id":"b","output":"ok"}`
	d := `{"type":"message","role":"developer","content":"guidance"}`
	for name, items := range map[string]string{
		"already adjacent":   a + `,` + b + `,` + ob + `,` + oa,
		"incomplete":         a + `,` + d + `,` + b + `,` + oa,
		"duplicate call":     a + `,` + d + `,` + a + `,` + oa,
		"duplicate output":   a + `,` + d + `,` + oa + `,` + oa,
		"orphan output":      a + `,` + d + `,` + oa + `,` + ob,
		"output before call": oa + `,` + a,
		"empty id":           `{"type":"function_call","call_id":""},` + d + `,{"type":"function_call_output","call_id":"","output":"ok"}`,
		"mismatched kind":    a + `,` + d + `,{"type":"custom_tool_call_output","call_id":"a","output":"ok"}`,
		"user boundary":      a + `,{"role":"user","content":"new request"},` + oa,
		"reasoning boundary": a + `,{"type":"reasoning","content":[]},` + oa,
		"assistant boundary": a + `,{"type":"message","role":"assistant","content":"next generation"},` + oa,
		"unknown boundary":   a + `,{"type":"future_item"},` + oa,
		"dependent call":     a + `,` + b + `,` + oa + `,` + d + `,{"type":"function_call","call_id":"c"},` + ob + `,{"type":"function_call_output","call_id":"c","output":"ok"}`,
	} {
		t.Run(name, func(t *testing.T) {
			input := []byte(`{ "input": [` + items + `] }`)
			require.True(t, json.Valid(input))
			got, err := RewriteRequest(input, true)
			require.NoError(t, err)
			require.Equal(t, input, got)
		})
	}
}
