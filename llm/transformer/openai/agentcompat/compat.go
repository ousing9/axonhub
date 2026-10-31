// Package agentcompat adapts Codex V2 task delivery without changing its agent lifecycle.
// Callers must explicitly opt in; old encrypted tasks are never decrypted here.
package agentcompat

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
)

const Namespace = "axonhub_v2"

var aliases = map[string]string{
	"spawn_agent":   "dispatch_task",
	"send_message":  "deliver_message",
	"followup_task": "continue_task",
}

func decode(body []byte) (map[string]any, error) {
	var value map[string]any
	d := json.NewDecoder(bytes.NewReader(body))
	d.UseNumber()
	if err := d.Decode(&value); err != nil {
		return nil, err
	}
	if value == nil {
		return nil, errors.New("expected a Responses object")
	}
	return value, nil
}

func object(v any) map[string]any { m, _ := v.(map[string]any); return m }
func text(v any) string           { s, _ := v.(string); return s }

// RewriteRequest prepares new plaintext collaboration calls. Portable mode also
// adapts agent_message input to public Responses messages. Native mode preserves
// existing native agent inputs, including opaque encrypted history.
func RewriteRequest(body []byte, portable bool) ([]byte, error) {
	v, err := decode(body)
	if err != nil {
		return nil, err
	}
	changed := false
	var owners []map[string]any
	items, _ := v["input"].([]any)
	if _, ok := v["tools"].([]any); ok {
		owners = append(owners, v)
	} else if len(items) > 0 {
		item := object(items[0])
		if text(item["type"]) == "additional_tools" && text(item["role"]) == "developer" {
			if _, ok := item["tools"].([]any); ok {
				owners = append(owners, item)
			}
		}
	}
	for _, owner := range owners {
		ts := owner["tools"].([]any)
		if err := checkCatalog(ts, 0); err != nil {
			return nil, err
		}
		if portable {
			continue
		}
		for _, raw := range ts {
			t := object(raw)
			if text(t["type"]) != "namespace" || text(t["name"]) != "collaboration" {
				continue
			}
			children, _ := t["tools"].([]any)
			var functions []any
			for _, child := range children {
				fn := object(child)
				name := text(fn["name"])
				alias, ok := aliases[name]
				if !ok {
					continue
				}
				encoded, err := json.Marshal(fn)
				if err != nil {
					return nil, err
				}
				copy, err := decode(encoded)
				if err != nil {
					return nil, err
				}
				copy["name"] = alias
				copy["description"] = map[string]string{
					"spawn_agent":   "Spawn a deepseek-flash native Codex V2 subagent with plaintext task delivery. Codex owns its thread, tools and lifecycle. Other models use collaboration.spawn_agent.",
					"send_message":  "Send a plaintext message to an existing deepseek-flash native agent. Does not wake an idle agent; use continue_task for new work.",
					"followup_task": "Send a plaintext follow-up task to an existing deepseek-flash native agent; wake it if idle.",
				}[name]
				properties := object(object(copy["parameters"])["properties"])
				delete(object(properties["message"]), "encrypted")
				if name == "spawn_agent" {
					properties["model"] = map[string]any{"type": "string", "enum": []string{"deepseek-flash"}}
					parameters := object(copy["parameters"])
					required, _ := parameters["required"].([]any)
					if !slices.Contains(required, any("model")) {
						parameters["required"] = append(required, "model")
					}
				}
				functions = append(functions, copy)
			}
			if len(functions) != 3 {
				continue
			}
			owner["tools"] = append(ts, map[string]any{"type": "namespace", "name": Namespace,
				"description": "Plaintext transport for DeepSeek native Codex V2 agents. Use dispatch_task for deepseek-flash, deliver_message and continue_task for those agents. All tools are mapped back to native Codex collaboration; other agents use the original collaboration namespace.", "tools": functions})
			changed = true
		}
	}
	if changed && !portable {
		guidance := map[string]any{"type": "message", "role": "developer", "content": []any{map[string]any{"type": "input_text", "text": "For deepseek-flash ONLY, use axonhub_v2.dispatch_task to spawn a native Codex subagent, axonhub_v2.deliver_message to message it, and axonhub_v2.continue_task for follow-up tasks. These are deterministic transport adapters to the native collaboration tools, not external agents. Use original collaboration tools for GPT agents and for list/wait/interrupt. Preserve all existing permission and delegation requirements. Do not call collaboration.spawn_agent/send_message/followup_task for DeepSeek because its API cannot read encrypted task arguments."}}}
		v["input"] = append(items, guidance)
	}
	// Match the history to the upstream names without touching arguments, output,
	// prompt text, reasoning, image contents, or arbitrary nested application JSON.
	for _, raw := range items {
		item := object(raw)
		if !portable && text(item["type"]) == "function_call" {
			var args map[string]any
			if json.Unmarshal([]byte(text(item["arguments"])), &args) == nil {
				message := text(args["message"])
				if message != "" && !strings.HasPrefix(message, "gAAAA") {
					changed = renameCall(item, false) || changed
				}
			}
		}
		if portable && text(item["type"]) == "agent_message" {
			parts, ok := item["content"].([]any)
			if !ok || len(parts) == 0 {
				return nil, errors.New("agent task has no content")
			}
			for _, rawPart := range parts {
				part := object(rawPart)
				if text(part["type"]) != "input_text" {
					return nil, errors.New("portable V2 requires plaintext agent tasks; encrypted or unknown content is unsupported")
				}
				if strings.HasPrefix(strings.TrimSpace(text(part["text"])), "gAAAA") {
					return nil, errors.New("opaque agent task cannot be delivered as plaintext")
				}
			}
			item["type"], item["role"] = "message", "user"
			delete(item, "author")
			delete(item, "recipient")
			delete(item, "internal_chat_message_metadata_passthrough")
			changed = true
		}
	}
	if portable {
		if ordered, ok := alignToolBatches(items); ok {
			v["input"] = ordered
			changed = true
		}
	}
	if !changed {
		return body, nil
	}
	return json.Marshal(v)
}

func checkCatalog(tools []any, depth int) error {
	if depth > 16 {
		return errors.New("tool namespace nesting exceeds compatibility limit")
	}
	for _, raw := range tools {
		t := object(raw)
		n := text(t["name"])
		if n == Namespace || strings.HasPrefix(n, Namespace+".") || strings.HasPrefix(n, Namespace+"__") {
			return errors.New("tool namespace conflicts with Codex agent compatibility")
		}
		if children, ok := t["tools"].([]any); ok {
			if err := checkCatalog(children, depth+1); err != nil {
				return err
			}
		}
	}
	return nil
}

func renameCall(item map[string]any, restore bool) bool {
	from, to := "collaboration", Namespace
	if restore {
		from, to = Namespace, "collaboration"
	}
	n, ns := text(item["name"]), text(item["namespace"])
	if ns == from {
	} else if strings.HasPrefix(n, from+".") {
		n = strings.TrimPrefix(n, from+".")
	} else if strings.HasPrefix(n, from+"__") {
		n = strings.TrimPrefix(n, from+"__")
	} else {
		return false
	}
	if restore {
		found := false
		for original, alias := range aliases {
			if n == alias {
				n = original
				found = true
				break
			}
		}
		if !found {
			return false
		}
	} else if alias, ok := aliases[n]; ok {
		n = alias
	} else {
		return false
	}
	item["namespace"] = to
	item["name"] = n
	return true
}

// RestoreResponse handles a bounded JSON response or one complete SSE/WS event.
// It also runs on incremental responses with no tool catalog in that request.
func RestoreResponse(body []byte, portable bool) ([]byte, error) {
	if bytes.Equal(bytes.TrimSpace(body), []byte("[DONE]")) {
		return body, nil
	}
	v, err := decode(body)
	if err != nil {
		return nil, err
	}
	changed, err := restoreEnvelope(v, 0, portable)
	if err != nil {
		return nil, err
	}
	if !changed {
		return body, nil
	}
	return json.Marshal(v)
}

func restoreEnvelope(v map[string]any, depth int, portable bool) (bool, error) {
	if depth > 8 {
		return false, errors.New("response nesting exceeds compatibility limit")
	}
	changed := false
	isFunction := text(v["type"]) == "function_call"
	restored := isFunction && renameCall(v, true)
	if isFunction && (restored || portable && text(v["namespace"]) == "collaboration") {
		if _, ok := aliases[text(v["name"])]; ok {
			if args, ok := v["encrypted_function_args"].([]any); ok && len(args) != 0 {
				return false, errors.New("upstream encrypted a plaintext collaboration call")
			}
			if s := text(v["arguments"]); s != "" {
				var args map[string]any
				if err := json.Unmarshal([]byte(s), &args); err != nil {
					return false, fmt.Errorf("invalid plaintext collaboration arguments: %w", err)
				}
				if strings.HasPrefix(strings.TrimSpace(text(args["message"])), "gAAAA") {
					return false, errors.New("upstream returned an opaque collaboration task")
				}
				if restored && text(v["name"]) == "spawn_agent" && text(args["model"]) != "deepseek-flash" {
					return false, errors.New("DeepSeek task adapter requires model deepseek-flash")
				}
			}
			// Codex 0.166 recognizes this explicit marker as a plaintext direct call.
			v["encrypted_function_args"] = []string{}
		}
		changed = true
	}
	for _, key := range []string{"item", "response"} {
		if child := object(v[key]); child != nil {
			c, err := restoreEnvelope(child, depth+1, portable)
			if err != nil {
				return false, fmt.Errorf("restore %s: %w", key, err)
			}
			changed = c || changed
		}
	}
	if output, ok := v["output"].([]any); ok {
		for _, raw := range output {
			if item := object(raw); item != nil {
				c, err := restoreEnvelope(item, depth+1, portable)
				if err != nil {
					return false, err
				}
				changed = c || changed
			}
		}
	}
	return changed, nil
}
