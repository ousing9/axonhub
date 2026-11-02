# Codex fork patches and upstream upgrades

This is the maintenance entry point for this fork's Codex patches. Use it during AxonHub upgrades to identify local capabilities, locate their implementation, verify behavior, and decide when compatibility code can be retired. See the [Codex integration guide](codex-integration.md) for general setup and [config.example.yml](../../../config.example.yml) for configuration definitions.

## Baseline and patch inventory

As of 2026-11-02, the maintenance branch is `codex/v2-portable-agents`, based on upstream `v1.0.0-beta11` (`b8bb3421`). Production was rolled back to the image-only patch `d798dc63` after a GPT cache regression. Cache repair `c18afe61` and mixed agents using ordinary model names passed isolated acceptance and are not deployed. The dedicated-model restriction from `33069930` has been withdrawn. Locally maintained code is published to the personal GitHub fork, with these remote roles:

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

These are the main maintained patches, in dependency order. Distinguish historical commits from current behavior; this inventory does not establish upstream inclusion.

| Commit | Local capability and reason | Upgrade review focus |
| --- | --- | --- |
| `d798dc63` | Preserve native Images API generation and editing on official ChatGPT Codex channels | Authentication, image endpoints, edit conversion, passthrough conditions |
| `dc8e9d56` | Adapt plaintext tasks for DeepSeek, which cannot consume Codex V2 encrypted task arguments, while retaining native subagent lifecycle | Tool catalog, `agent_message` input, restored call names, `encrypted_function_args` marker, configuration wiring |
| `06c35032` | Repair DeepSeek tool histories interrupted by path guidance and prevent passthrough from undoing developer-to-system replacement | History assembly and conversion order, complete tool-batch pairing, final outbound role replacement |
| `3e82c921` | Prevent Codex progress from collapsing and reopening when DeepSeek starts a message as final and finishes it as commentary | Provisional phase in `output_item.added` and authoritative phase in `output_item.done` |
| `c18afe61` | Fix the connection/cache regression introduced by `dc8e9d56` appending transient guidance to GPT history; keep guidance in the stable tool description | Multi-turn WebSocket handshakes, `previous_response_id`, incremental input, and actual cached-token growth |
| `09938648` | Remove the dedicated GPT model alias restriction; support native mixed agents with ordinary model names and add live Process-pipeline comparison | Stable tool descriptions, previous input prefixes, bidirectional DeepSeek messaging and same-child follow-up, native GPT children, cache comparison |

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

Mixed mode keeps existing client GPT model names and Codex's native agent lifecycle. Explicitly select GPT Responses passthrough channels and the DeepSeek model in AxonHub; no dedicated model alias is required:

```yaml
codex_agent_compatibility:
  native_channel_ids: [36]  # Example: replace with this instance's GPT Responses passthrough channel ID
  portable_models: ["deepseek-flash"]
```

`native_channel_ids` controls the fixed GPT task-tool extension and is rechecked on retries and channel failover. Users keep selecting existing models such as `gpt-6-astra`. When the model uses a DeepSeek plaintext tool, the gateway restores the call to Codex's native `spawn_agent`, `send_message`, or `followup_task`. GPT children keep their original encrypted tools. This adds neither an independent broker nor a second agent lifecycle.

The fixed tool definitions and descriptions add input tokens compared with disabled adaptation. Guidance is never appended as a hidden message at the end of each request. Acceptance must check connection reuse, `previous_response_id`, cached tokens, and real parent/child round trips, not just successful tool selection.

`portable_models` exactly matches the original client model before model mapping. Task-message conversion, tool-history normalization, passthrough role repair, and streaming phase repair apply only to the selected DeepSeek requests. Running DeepSeek as the main agent alone can leave `native_channel_ids` empty; GPT → DeepSeek delegation still needs the GPT plaintext tool extension.

Channel IDs are instance-specific. Configuration loads at startup; empty lists disable adaptation. The supported portable model is `deepseek-flash`; validate another model before adding it. The experimental `native_models` setting has been withdrawn and should be removed from candidate configurations. Production remains on the rollback version; the configuration above has not been deployed.

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

The repaired path returned 5,376, 6,912, and 8,576 cached tokens in rounds two through four. A separate live tool-selection probe chose `axonhub_v2.dispatch_task` from the tool description; no child was executed. These observations demonstrate restored connection continuity and better caching in the sample, not a fixed production hit-rate guarantee. New input, channel/account switching, model changes, and tool-catalog changes still affect caching. The full live subagent lifecycle is covered by the pipeline acceptance below.

The dedicated-model restriction in `33069930` has been withdrawn. Mixed delegation uses existing model names and the channel allowlist, retaining the stable-description repair from `c18afe61`.

`TestLiveAgentCompatibilityIsolated` exercises the actual `Process` pipeline, an in-memory database, isolated Codex configuration, and fixed official GPT/DeepSeek upstreams. It does not contact a running gateway or read production databases/personal Codex configuration; the caller explicitly supplies credentials and a model catalog. It checks preservation of prior input across consecutive outbound GPT requests, the initial DeepSeek task, separate messaging, same-child follow-up, a child-generated random nonce round trip, native GPT delegation, and GPT WebSocket handshakes/cache usage.

Actual CLI connection behavior needs a separate qualification: Codex 0.166.0 changes `metadata` and `internal_chat_message_metadata_passthrough` when replaying output; some reasoning items also change `content`. AxonHub's strict prefix comparison therefore still reconnects. A pure GPT control with native adaptation disabled reproduced the same differences. This change does not modify general GPT transport or prefix comparison. The one-connection result above applies to controlled, stable-prefix tests, not every real CLI message shape. Live acceptance records handshakes and compares cached tokens for the parent GPT after its first round; the five-percentage-point regression threshold is a test guard, not a production hit-rate promise.

Full-pipeline samples on 2026-11-02 used Codex 0.166.0 and a `gpt-6-astra` parent. The rates below use the old recorded counters after the parent's first request. That sampler could not distinguish absent counters from explicit zero; see the audit below:

| Comparison | Pure GPT baseline | GPT + DeepSeek | Result |
| --- | --- | --- | --- |
| First full comparison | 98.50% | 83.28% | Functionality passed; the mixed group's request 8 was recorded as zero and failed the cache gate |
| Independent run with request diagnostics | 98.33% | 98.38% | Functionality/cache gate passed; earlier outbound input and other top-level fields stayed stable |
| Final source run | 85.70% | 98.21% | Functionality, real request prefixes, and cache gate passed; request 6 of the pure GPT baseline was also recorded as zero |

A per-request audit confirmed both zeros occurred in later requests of the same parent thread. Each CLI trace contains one `thread.started` and one `turn.started`. Statistics include only `gpt-6-astra`; children use `gpt-6-luna` or `deepseek-flash`, and the parent's first request is excluded.

| Sample | Parent request number | Input / recorded cached tokens | Adjacent cached-token counts |
| --- | --- | --- | --- |
| First mixed group | 8 | 16,787 / 0 | Request 7: 16,128 |
| Final pure GPT baseline | 6 | 15,863 / 0 | Request 5: 15,360; request 7: 15,616 |

These are consecutive model calls within one user task, not new conversations or children's first requests. However, the old sampler called `gjson.Int()` directly: missing/null counters also became zero. Archived reports do not retain field presence, so they cannot establish that upstream explicitly returned zero. The supported conclusion is a zero in later-request usage records, with actual cache misses and missing usage still undistinguished—not a confirmed complete upstream cache miss.

The test now preserves response IDs and the original `input_tokens_details`, records missing/non-numeric counters as null plus `usage_error`, and fails acceptance instead of treating them as zero. Offline tests cover explicit zero, positive values, omission, null, and incorrect types; paid live acceptance has not been rerun with this recording correction. Both old groups exhibit the issue, so it cannot simply be attributed to DeepSeek adaptation. Successful later samples do not erase the first failure. The diagnostic run had 9 parent requests/9 handshakes in the baseline and 10/10 in the mixed group, showing that reconnection does not automatically mean a cache miss. Retain real-prefix checks, cache comparisons, and upstream anomaly records during upgrades.

Full pipeline reports are in the `acceptance*` directories under `~/.local/state/axonhub-tests/20261102-native-mixed/`. The baseline disables native adaptation and uses GPT children; the mixed group enables stable tools and replaces the first child with DeepSeek. Both execute the same task/message flow. A `context canceled` error is classified as a client interruption only when the downstream HTTP context is also canceled. Other errors and upstream failure events still fail acceptance. Continued execution after messaging, same-target follow-up, and nonce round trips must all succeed.

Sanitized acceptance and drivers are saved under `~/.local/state/axonhub-tests/20261102-gpt-cache-c18afe61/`; temporary credentials for both test batches were deleted. Track source repairs separately from deployment state: the running service remains on the rollback version.

## Following upstream releases

1. Record the current source commit, image, configuration, and acceptance results; retain a rollback branch or tag. After `git fetch upstream --tags`, choose a target version and read its release/migration notes. The current patch inventory does not prove the target version still lacks these capabilities.
2. Compare the target version with each patch on an upgrade branch. Determine whether upstream has equivalent behavior; port remaining patches in inventory order. Review history assembly, middleware ordering, passthrough, and terminal stream events when resolving conflicts. Compilation alone does not establish semantic compatibility.
3. For patches covered upstream, remove the local implementation and run the same behavioral acceptance cases. Evaluate history normalization, role replacement, and phase repair separately rather than dropping the whole adapter when one issue is fixed.
4. Update this document's baseline, commit mapping, behavior differences, and deployment record in both languages. Retain regression cases to detect future upstream regressions.
5. Complete offline regression and isolated live acceptance before building and deploying. Upstream upgrades may change frontend assets, dependencies, and the database; build a complete image for the target version. Replacing only the backend binary while inheriting old frontend assets is suitable only for a patch release on the same upstream baseline with no frontend changes.

The upstream `unstable` revision inspected on 2026-11-02 was `aa3a265ddc1a7bb0787d56020a1aa77d8ec0e036`. Tool-output ordering fix [#2633](https://github.com/looplj/axonhub/pull/2633) applies to Responses-to-Chat conversion and does not replace encrypted-task or `agent_message` handling here. That revision also retains strict WebSocket input-prefix comparison. Recheck these findings against future revisions.

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
| Compatibility disabled or DeepSeek-only; ordinary GPT | Existing tool catalog, encrypted arguments, and stream phases remain unchanged |
| GPT native adaptation enabled across controlled rounds with stable history prefixes | One WebSocket, correct previous_response_id and incremental input; cached tokens grow with reusable history; genuine history edits reconnect |
| Existing GPT model name with mixed adaptation enabled | Fixed tools are added without hidden history messages; native encrypted responses and final phases stay intact |
| Unselected GPT channel or retry to another channel | Native adaptation stays disabled; requests gain no passthrough requirement and error responses are not parsed or rewritten |
| DeepSeek main agent | Multiple tool rounds, actual result writes, and read-back verification succeed |
| GPT parent → DeepSeek child | Initial task, same-child messaging, and follow-up work; GPT children still work |
| Developer/system guidance between calls | Complete batches continue; genuinely missing results still fail without fabricated output |
| Role replacement on/off; passthrough/conversion | Explicit configuration and scope are honored without extending to other channels |
| DeepSeek tool preamble | added=commentary, done=commentary, normal text deltas, prior progress stays visible |
| DeepSeek final answer | added=commentary, done=final_answer, collapse available on completion |
| GPT final answer | Existing added=final_answer, done=final_answer behavior |
| Stream errors, interruptions, non-streaming responses | Errors remain visible; no fabricated success/final answer or discarded completion phase |
| Native Codex images | Generation, editing, routing, mask protection, and third-party relay boundaries remain correct |

The full pipeline test skips by default. It requires a local `codex` CLI, a catalog containing all three models, and an external JSON credential file with mode `0600`. Its shape is `{"OAuth":{"access_token":"…","expires_at":"…"},"DeepSeekKey":"…","Catalog":"/absolute/path/models.json"}`. OAuth may include an account's required `id_token`; the test uses static credentials and does not refresh or persist tokens. Keep credentials and reports outside the source checkout.

```bash
AXONHUB_AGENT_ISOLATED_LIVE=1 \
AXONHUB_AGENT_ISOLATED_CONFIG=/secure/path/credentials.json \
AXONHUB_AGENT_TEST_REPORT_DIR=/secure/path/reports \
go test ./internal/server/orchestrator -run '^TestLiveAgentCompatibilityIsolated$' -count=1 -v -timeout=20m
```

Reports are separated into `baseline/` and `mixed/`. `pipeline-report.json` records tool types, targets, markers, nonce-return booleans, usage, and connection counts; `native-cli.jsonl` is a raw synthetic-task trace and must not be committed. Delete temporary credentials afterward. This test uses actual Process middleware with an in-memory test database; limitations of the older relay tests follow below.

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
| Candidate repair | `c18afe61` cache repair + `09938648` mixed agents with ordinary model names; offline regression and live pipeline comparison passed, not deployed |

Use a new image tag for each release, back up Compose, and record the source version and binary hash. Run direct gateway acceptance after the service becomes healthy, then update this table. Each deployment's `rollback.py` verifies its post-deployment Compose hash before restoring that directory's previous Compose file and recreating the `axonhub` service. Check the restoration direction before execution: the November 2 rollback directory preserves the withdrawn compatibility version, so its script would restore that version rather than the current healthy baseline.

If Compose has changed since deployment, the script stops so those changes can be compared rather than overwritten. These compatibility patches have no database migration. For future upstream upgrades, evaluate database backups and migration rollback separately; switching to an older image does not roll back the database.
