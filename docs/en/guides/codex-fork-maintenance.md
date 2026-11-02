# Codex fork patches and upstream upgrades

This is the maintenance entry point for this fork's Codex patches. Use it during AxonHub upgrades to identify local capabilities, locate their implementation, verify behavior, and decide when compatibility code can be retired. See the [Codex integration guide](codex-integration.md) for general setup and [config.example.yml](../../../config.example.yml) for configuration definitions.

## Baseline and patch inventory

As of 2026-11-02, the maintenance branch is `codex/v2-portable-agents`, based on upstream `v1.0.0-beta11` (`b8bb3421`). Production was rolled back to the image-only patch `d798dc63` after a GPT cache regression. Cache repair `c18afe61` passed isolated acceptance and is not deployed. Locally maintained code is published to the personal GitHub fork, with these remote roles:

| Remote | URL | Purpose |
| --- | --- | --- |
| `origin` | `git@github.com:ousing9/axonhub.git` | Primary push destination for locally maintained code |
| `upstream` | `https://github.com/looplj/axonhub.git` | Official releases and upstream updates |

The local repository sets `remote.pushDefault=origin`, and the maintenance branch tracks `origin/codex/v2-portable-agents`. Use `git push` for normal publication and `git fetch upstream --tags` for official updates. Git remote configuration is shared by worktrees of the same repository.

For a fresh clone:

```bash
git clone --branch codex/v2-portable-agents git@github.com:ousing9/axonhub.git
cd axonhub
git remote add upstream https://github.com/looplj/axonhub.git
git config remote.pushDefault origin
```

These five functional commits follow the baseline, in dependency order. They describe local patches, not features confirmed to be merged upstream. Later documentation commits are separate.

| Commit | Local capability and reason | Upgrade review focus |
| --- | --- | --- |
| `d798dc63` | Preserve native Images API generation and editing on official ChatGPT Codex channels | Authentication, image endpoints, edit conversion, passthrough conditions |
| `dc8e9d56` | Adapt plaintext tasks for DeepSeek, which cannot consume Codex V2 encrypted task arguments, while retaining native subagent lifecycle | Tool catalog, `agent_message` input, restored call names, `encrypted_function_args` marker, configuration wiring |
| `06c35032` | Repair DeepSeek tool histories interrupted by path guidance and prevent passthrough from undoing developer-to-system replacement | History assembly and conversion order, complete tool-batch pairing, final outbound role replacement |
| `3e82c921` | Prevent Codex progress from collapsing and reopening when DeepSeek starts a message as final and finishes it as commentary | Provisional phase in `output_item.added` and authoritative phase in `output_item.done` |
| `c18afe61` | Fix the connection/cache regression introduced by `dc8e9d56` appending transient guidance to GPT history; keep guidance in the stable tool description | Multi-turn WebSocket handshakes, `previous_response_id`, incremental input, and actual cached-token growth |

Inspect the complete local delta with:

```bash
git log --reverse --oneline v1.0.0-beta11..HEAD
git diff --stat v1.0.0-beta11..HEAD
git show <commit>
```

## Patch boundaries

### Official Codex images

The implementation is in `llm/transformer/openai/codex/images.go`, its neighboring `outbound.go`, and orchestrator image passthrough tests. Official Codex channels reuse OAuth and use native `/images/generations` and `/images/edits`; edit requests are converted to the Codex format. Third-party compatible relays retain the existing conversion path. Native editing ignores masks, so the local implementation rejects masked requests. Retain that protection until the upstream endpoint actually supports the requested behavior.

### DeepSeek V2 task adaptation

The adapter is opt-in; empty configuration leaves default behavior unchanged:

```yaml
codex_agent_compatibility:
  native_channel_ids: []  # IDs of GPT Responses passthrough channels needing tool injection
  portable_models: ["deepseek-flash"]
```

Channel IDs are instance-specific; do not copy them from another database. Configuration is loaded at startup. The currently supported portable model is `deepseek-flash`; validate protocol behavior before adding another model.

On the GPT task-producing side, native `collaboration` tools remain available. Three additional DeepSeek-only tools, `axonhub_v2.dispatch_task`, `deliver_message`, and `continue_task`, are mapped back to native collaboration calls on response. `encrypted_function_args: []` explicitly marks plaintext arguments. This convention was validated with Codex 0.166 and needs review when Codex changes. The adapter does not alter authorization, subagent lifecycle, or encrypted GPT task behavior, and cannot decrypt existing opaque DeepSeek tasks.

Routing guidance belongs in the stable `axonhub_v2` namespace description. Do not append client-invisible messages to history on every request. Functional tool acceptance does not establish history-prefix or cache continuity; verify these separately.

Portable input adaptation runs after Responses history assembly and before protocol conversion so the parser does not discard `agent_message`. Portable response adaptation runs after conversion back to the client protocol; native GPT tool adaptation runs on the outbound response side. Preserve those ordering relationships during upgrades.

Main implementation entry points:

- `llm/transformer/openai/agentcompat/`: request and event compatibility transforms.
- `internal/server/orchestrator/agent_compatibility.go`: model/channel scoping, passthrough role repair, stream wrapper.
- `internal/server/orchestrator/orchestrator.go`: middleware and history processing order.
- `conf/conf.go` and `internal/server/api/openai.go`: configuration and API wiring.

### Tool history and role replacement

DeepSeek's native Responses API returned `No tool output found` for complete histories shaped like:

```text
call A → developer/system guidance → call B → output A → output B
```

`tool_history.go` rearranges only complete, unambiguous batches into:

```text
call A → call B → output A → output B → original guidance
```

Calls, results, and guidance content are preserved. Missing or duplicate IDs, mismatched kinds, user messages, or other generation boundaries prevent normalization; the adapter never invents tool results. Tests cover function/custom calls, images, and multipart outputs.

`replaceDeveloperRoleWithSystem` is a separate channel option. Raw request passthrough previously overwrote the ordinary conversion-stage replacement. Portable Responses requests now honor that explicit option on the final outbound body as well. Role replacement alone does not repair interrupted tool batches.

### Streaming message phases

On 2026-10-31, a direct request to DeepSeek's official API, bypassing AxonHub, reproduced a tool preamble starting as `final_answer` and finishing as `commentary`. AxonHub previously passed those phases through. Codex collapsed earlier progress on the start event and reopened it on completion.

In portable mode only, `RestoreResponse` changes `final_answer` to `commentary` on assistant messages in `response.output_item.added`. Text deltas, completion phases, and non-streaming responses remain intact. There is no whole-message buffering and no GPT phase rewrite. Missing or unknown phases are not inferred.

The tradeoff is that a real DeepSeek final answer triggers collapse after its text finishes. Matching GPT's collapse-at-start behavior requires the upstream API to distinguish commentary from final answers reliably at message start. Permanently marking every message as commentary would also prevent proper final-answer collapsing and is not an equivalent repair.

### GPT cache regression and repair

`dc8e9d56` appended transient developer guidance to each GPT request. The next client history did not contain that message, so the previous upstream input plus output was no longer a prefix of the new request. `responses/websocket_executor.go` consequently dropped `previous_response_id`, reconnected, and sent full context. This affected GPT channels with native adaptation enabled even when no DeepSeek tool was actually called. This investigation did not identify a problem with DeepSeek's own cache.

The November 2 investigation found prefix breaks at this guidance item and new handshakes in all 15 sampled adjacent production request pairs. Channel switching after quota exhaustion added cold starts. Earlier functional acceptance missed the regression because it did not test connection and cache continuity.

`c18afe61` moves the complete guidance into the stable tool description and removes the appended message. The new `responses/agent_compatibility_test.go` connects the adapter to the real WebSocket executor. Both top-level tools and additional_tools catalogs reuse one connection across four rounds, sending only new tool results. Cases include ordinary tools, DeepSeek plaintext call-name round trips, and encrypted GPT arguments. Editing earlier history still reconnects correctly. Running the test against the old implementation fails reliably on the second round.

Isolated acceptance called the official upstream directly with `gpt-6-astra` and separate sessions, without passing through production AxonHub. Each group had four rounds with added synthetic data:

| Mode | Handshakes across four rounds | Final-round input / cached tokens | Final-round hit rate |
| --- | --- | --- | --- |
| No adaptation | 1 | 9,518 / 7,680 | 80.7% |
| Old adaptation | 4 | 10,501 / 6,912 | 65.8% |
| Repaired adaptation | 1 | 10,423 / 8,576 | 82.3% |

The repaired path returned 5,376, 6,912, and 8,576 cached tokens in rounds two through four. A separate live tool-selection probe chose `axonhub_v2.dispatch_task` from the tool description; no child was executed. These observations demonstrate restored connection continuity and better caching in the sample, not a fixed production hit-rate guarantee. New input, channel/account switching, model changes, and tool-catalog changes still affect caching. The full live subagent lifecycle has not been rerun after this repair and remains a pre-deployment acceptance item.

Sanitized acceptance and drivers are saved under `~/.local/state/axonhub-tests/20261102-gpt-cache-c18afe61/`; temporary credentials were deleted. Track source repairs separately from deployment state: the running service remains on the rollback version.

## Following upstream releases

1. Record the current source commit, image, configuration, and acceptance results; retain a rollback branch or tag. After `git fetch upstream --tags`, choose a target version and read its release/migration notes. The current patch inventory does not prove the target version still lacks these capabilities.
2. Compare the target version with each patch on an upgrade branch. Determine whether upstream has equivalent behavior; port remaining patches in inventory order. Review history assembly, middleware ordering, passthrough, and terminal stream events when resolving conflicts. Compilation alone does not establish semantic compatibility.
3. For patches covered upstream, remove the local implementation and run the same behavioral acceptance cases. Evaluate history normalization, role replacement, and phase repair separately rather than dropping the whole adapter when one issue is fixed.
4. Update this document's baseline, commit mapping, behavior differences, and deployment record in both languages. Retain regression cases to detect future upstream regressions.
5. Complete offline regression and isolated live acceptance before building and deploying. Upstream upgrades may change frontend assets, dependencies, and the database; build a complete image for the target version. Replacing only the backend binary while inheriting old frontend assets is suitable only for a patch release on the same upstream baseline with no frontend changes.

Retire a patch when the target version passes its original failure case, not merely because a PR title looks similar. Converted tool-output handling and native Responses passthrough history handling can fail on different paths. References: [DeepSeek Responses documentation](https://api-docs.deepseek.com/guides/responses_api/), [tool-history compatibility report](https://github.com/deepseek-ai/awesome-deepseek-integration/issues/740), and [OpenCode #57372](https://github.com/anomalyco/opencode/issues/57372). The latter reports missing phases; distinguish that from the inconsistent start/end phases observed in this fork.

## Acceptance checklist

Run offline regression from the repository root. `llm/` is a separate Go module:

```bash
go test ./conf ./internal/server/orchestrator -count=1
cd llm
go test ./transformer/openai/agentcompat ./transformer/openai/responses ./transformer/openai/codex -count=1
```

| Scenario | Expected behavior |
| --- | --- |
| Compatibility disabled; ordinary GPT | Existing tool catalog, encrypted arguments, and stream phases remain unchanged |
| GPT native adaptation enabled across multiple tool rounds | One WebSocket, correct previous_response_id and incremental input; cached tokens grow with reusable history; genuine history edits reconnect |
| DeepSeek main agent | Multiple tool rounds, actual result writes, and read-back verification succeed |
| GPT parent → DeepSeek child | Initial task, same-child messaging, and follow-up work; GPT children still work |
| Developer/system guidance between calls | Complete batches continue; genuinely missing results still fail without fabricated output |
| Role replacement on/off; passthrough/conversion | Explicit configuration and scope are honored without extending to other channels |
| DeepSeek tool preamble | added=commentary, done=commentary, normal text deltas, prior progress stays visible |
| DeepSeek final answer | added=commentary, done=final_answer, collapse available on completion |
| GPT final answer | Existing added=final_answer, done=final_answer behavior |
| Stream errors, interruptions, non-streaming responses | Errors remain visible; no fabricated success/final answer or discarded completion phase |
| Native Codex images | Generation, editing, routing, mask protection, and third-party relay boundaries remain correct |

Live acceptance uses synthetic tasks, requires explicit opt-in, and incurs API charges. Existing `TestLivePortableToolHistory`, `TestLivePortableMainAgent`, and `TestLiveNativeCodex` read `AXONHUB_AGENT_LIVE=1`, `AXONHUB_AGENT_TEST_URL` (gateway root URL without `/v1`), and `OPENAI_API_KEY`. The native child-agent test additionally needs `AXONHUB_AGENT_TEST_CATALOG`; `AXONHUB_AGENT_TEST_PARENT` selects its parent model. Read the corresponding `*live_test.go` before running it.

These tests' relays apply some adaptation themselves, particularly tool injection in `TestLiveNativeCodex`. Use an isolated instance with that adaptation disabled to avoid duplicate injection against production middleware. Relay tests do not replace post-deployment acceptance directly through the gateway. Verify collapsing in the desktop UI too: a completed chat record cannot establish that streaming never flickered.

## Deployment records and rollback

Use the deployment host's AxonHub Compose directory, currently `~/axonhub`. Source control holds implementation, sample configuration, and synthetic tests. Its runtime `deployments/` directory holds image metadata, Compose backups, binaries, acceptance summaries, and rollback scripts. Credentials, raw conversations, and complete runtime logs stay in the protected runtime directory, outside source control.

Current release record:

| Item | Value |
| --- | --- |
| Running source commit | `d798dc63`, image fix only |
| Current image | `axonhub-local:v1.0.0-beta11-native-images` |
| Withdrawn image | `axonhub-local:v1.0.0-beta11-native-images-codex-v2-phase-3e82c921` |
| Rollback record directory | `deployments/20261102-150907-rollback-native-images/` |
| Candidate repair | `c18afe61`, not deployed; relevant offline packages, full orchestrator, and direct official upstream cache comparison passed |

Use a new image tag for each release, back up Compose, and record the source version and binary hash. Run direct gateway acceptance after the service becomes healthy, then update this table. Each deployment's `rollback.py` verifies its post-deployment Compose hash before restoring that directory's previous Compose file and recreating the `axonhub` service. Check the restoration direction before execution: the November 2 rollback directory preserves the withdrawn compatibility version, so its script would restore that version rather than the current healthy baseline.

If Compose has changed since deployment, the script stops so those changes can be compared rather than overwritten. These compatibility patches have no database migration. For future upstream upgrades, evaluate database backups and migration rollback separately; switching to an older image does not roll back the database.
