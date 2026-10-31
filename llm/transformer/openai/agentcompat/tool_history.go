package agentcompat

// alignToolBatches keeps completed tool batches contiguous for DeepSeek's
// Responses parser. Codex can insert path guidance while dispatching a batch.
// Defer only those system/developer messages until its results are complete;
// preserve all content, call/result order, and instruction order. Do not move
// user turns, reasoning, or unknown items across a call boundary.
// Like CLIProxyAPI's ID-based alignment, ambiguous/incomplete histories are
// left untouched rather than inventing results or guessing their owner.
func alignToolBatches(items []any) ([]any, bool) {
	calls, outputs := map[string]string{}, map[string]bool{}
	for _, raw := range items {
		item := object(raw)
		kind, id := text(item["type"]), text(item["call_id"])
		switch kind {
		case "function_call", "custom_tool_call":
			if id == "" || calls[id] != "" {
				return items, false
			}
			calls[id] = kind + "_output"
		case "function_call_output", "custom_tool_call_output":
			if id == "" || outputs[id] || calls[id] != kind {
				return items, false
			}
			outputs[id] = true
		}
	}
	if len(calls) != len(outputs) {
		return items, false
	}

	ordered := make([]any, 0, len(items))
	changed := false
	for i := 0; i < len(items); {
		kind := text(object(items[i])["type"])
		if kind != "function_call" && kind != "custom_tool_call" {
			ordered = append(ordered, items[i])
			i++
			continue
		}
		pending := map[string]bool{}
		var batch, guidance []any
		seenOutput := false
		for ; i < len(items); i++ {
			item := object(items[i])
			kind, id := text(item["type"]), text(item["call_id"])
			switch kind {
			case "function_call", "custom_tool_call":
				// A new call after a result may be a dependent generation.
				if seenOutput {
					return items, false
				}
				pending[id] = true
				batch = append(batch, items[i])
			case "function_call_output", "custom_tool_call_output":
				if !pending[id] {
					return items, false
				}
				delete(pending, id)
				seenOutput = true
				batch = append(batch, items[i])
			default:
				role := text(item["role"])
				if (kind != "message" && kind != "") || (role != "system" && role != "developer") {
					return items, false
				}
				guidance = append(guidance, items[i])
			}
			if len(pending) == 0 {
				i++
				break
			}
		}
		ordered = append(ordered, batch...)
		ordered = append(ordered, guidance...)
		changed = changed || len(guidance) > 0
	}
	return ordered, changed
}
