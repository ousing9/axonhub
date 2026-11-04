# Codex 自维护补丁与主线升级

本文是本 fork 的 Codex 补丁维护入口。升级 AxonHub 时，用它确认哪些本地能力需要保留、对应代码在哪里、如何验收，以及何时可以删除兼容补丁。通用接入方式见 [Codex 集成指南](codex-integration.md)；配置定义以 [config.example.yml](../../../config.example.yml) 为准。

## 基线与补丁清单

截至 2026-11-03，维护分支为 `codex/v2-portable-agents`，基于上游 `v1.0.0-beta11`（`b8bb3421`）。缓存修复和普通模型名混合代理已随 `418d5a89` 部署；仅健康检查不能证明真实代理验收通过。新版 `bb451d04` 已取消 GPT 通道白名单，并统一按 DeepSeek 模型处理，模型来自哪个渠道不影响兼容路径。发布状态见文末。自维护代码发布到个人 GitHub fork，远端分工如下：

| Remote | 地址 | 用途 |
| --- | --- | --- |
| `origin` | `git@github.com:ousing9/axonhub.git` | 自维护代码的主要推送仓库 |
| `upstream` | `https://github.com/looplj/axonhub.git` | 官方主线，获取版本与合并更新 |

本地设置 `remote.pushDefault=origin`，维护分支跟踪 `origin/codex/v2-portable-agents`。日常推送使用 `git push`，获取官方更新使用 `git fetch upstream --tags`。Git remote 配置由同一仓库的 worktree 共用。

新克隆可使用：

```bash
git clone --branch codex/v2-portable-agents git@github.com:ousing9/axonhub.git
cd axonhub
git remote add upstream https://github.com/looplj/axonhub.git
git config remote.pushDefault origin
```

以下是当前维护的主要功能补丁，按依赖顺序排列；历史提交与当前生效行为应分开看。它们不代表上游已合入的功能。

| 提交 | 自维护能力与原因 | 升级时重点检查 |
| --- | --- | --- |
| `d798dc63` | 官方 ChatGPT Codex 渠道走原生 Images API，保留生成与编辑能力 | Codex 渠道鉴权、图像端点选择、编辑请求转换及透传条件 |
| `dc8e9d56` | DeepSeek 无法消费 Codex V2 的加密任务参数；增加明文任务适配，继续使用 Codex 原生子 agent 生命周期 | 工具目录、`agent_message` 输入、调用名称恢复、`encrypted_function_args` 标记、配置注入 |
| `06c35032` | DeepSeek 工具历史被路径指导消息打断时会误报缺少工具结果；同时修复透传覆盖 developer→system 配置的问题 | 历史组装与协议转换的顺序、完整工具批次配对、最终出站角色替换 |
| `3e82c921` | DeepSeek 流式消息先标记 final，结束时可能改成 commentary，导致 Codex 前文先折叠后展开 | `output_item.added` 的临时阶段与 `output_item.done` 的最终阶段 |
| `888a052c` | DeepSeek 新版 Responses 事件缺少阶段；补全前导语与最终回答的跨事件推断，同时保留文字实时输出 | portable 流只暂存消息 `done`，工具调用定为 `commentary`，完成事件定为 `final_answer`；GPT/native 不变 |
| `c18afe61` | 修复 `dc8e9d56` 在 GPT 历史末尾注入临时指导导致的连接复用和缓存回归；将指导保留在固定工具描述中 | 多轮真实 WebSocket 握手数、`previous_response_id` 与增量输入、实际缓存 token 增长 |
| `09938648` | 撤销专用 GPT 模型别名限制，普通模型名支持原生混合代理；增加完整 Process 管线的真实对照验收 | 固定工具描述、既有输入前缀、DeepSeek 双向消息与同一代理 follow-up、GPT 子代理及缓存对照 |
| `e021a859`、`bb451d04` | 移除 GPT 通道白名单；统一从模型配置生成工具与校验，仅在识别到 V2 加密工具能力时扩展 | 任意通道及重试目录一致；两个模型标识走相同路径；非 V2 请求不变 |

可在本地查看完整差异：

```bash
git log --reverse --oneline v1.0.0-beta11..HEAD
git diff --stat v1.0.0-beta11..HEAD
git show <commit>
```

## 各项补丁的边界

### 官方 Codex 图像

代码集中在 `llm/transformer/openai/codex/images.go`、同目录 `outbound.go` 及 orchestrator 的图像透传测试。官方 Codex 渠道复用 OAuth，使用原生 `/images/generations`、`/images/edits`；编辑请求转换为 Codex 所需格式。第三方兼容中继继续走既有转换路径。原生编辑忽略 mask，因此本地明确拒绝带 mask 的请求；升级时应保留此保护，直到上游真实支持该行为。

### DeepSeek V2 任务适配

混合模式保留客户端原来的 GPT 模型名，由 Codex 管理子代理。只需声明需要兼容处理的 DeepSeek 模型：

```yaml
codex_agent_compatibility:
  portable_models: ["deepseek-flash", "deepseek-v4.1-flash"]
```

所有 Responses body 透传通道均可获得 DeepSeek 明文任务工具，不按通道 ID、名称、账号或 provider 类型筛选。新增通道无需登记白名单；重试与切换通道使用相同的模型列表和固定工具目录。只有请求带有 Codex collaboration 工具时才新增工具；普通请求不会凭空得到子代理工具。通道仍须能完整传递 Codex Responses 工具定义和返回标记，未透传或其他协议路径沿用原行为，不因启用此功能新增错误。

原 `native_channel_ids` 是早期限定上线范围的本地选择，并非协议要求，现已移除；请删除对应 YAML 字段或 `AXONHUB_CODEX_AGENT_COMPATIBILITY_NATIVE_CHANNEL_IDS` 环境变量。无需新增 GPT 模型别名。`portable_models` 同时用于 GPT 侧工具的 model 枚举、调用返回校验和 DeepSeek 输入识别，避免配置接受 OpenCode Go、工具仍只接受 `deepseek-flash` 的不一致。它识别需要特殊协议处理的目标模型，不限制哪些 GPT 通道可以发起任务。GPT 子代理保持原生加密工具。

`portable_models` 精确匹配客户端原始模型名，在模型映射之前判断。任务消息转换、工具历史整理、透传角色修复和流式阶段修复仅作用于这些模型的请求。`deepseek-v4.1-flash` 包括 OpenCode Go 的入口；同名模型经其他通道提供时也按相同规则处理。新的模型入口仍应验证其上游协议，但无须修改源码中的模型常量。

配置在启动时加载，空模型列表关闭适配。固定工具定义增加一些输入 token；启用或修改模型列表会改变工具前缀一次。之后同一配置的目录保持稳定，不在每轮历史末尾插入隐藏消息。升级需分别检查多轮历史前缀、连接复用、缓存用量及真实父子代理往返。

GPT 生产任务一侧保留 `collaboration`，另加 `axonhub_v2.dispatch_task`、`deliver_message`、`continue_task` 三个工具，只用于 DeepSeek。返回时映射回原生 collaboration 调用，并用 `encrypted_function_args: []` 明确表示明文参数。该约定已按 Codex 0.166 验证，Codex 升级时需要复查。它不改变授权、子 agent 生命周期或 GPT 的加密任务行为；既有不透明的加密 DeepSeek 任务也不会被解密。

路由指导放在 `axonhub_v2` 的固定 namespace 描述里，不能每轮向历史末尾插入客户端不可见的消息。工具可用性通过测试并不意味着历史前缀和缓存连续性也成立，二者必须分别验收。

Portable 输入在 Responses 历史组装之后、协议转换之前处理，避免 `agent_message` 被解析器丢弃。Portable 响应在转换回客户端协议之后处理；原生 GPT 的工具适配在出站响应侧处理。不要在升级中交换这些位置。

主要代码入口：

- `llm/transformer/openai/agentcompat/`：请求与事件的兼容转换。
- `internal/server/orchestrator/agent_compatibility.go`：模型/渠道作用域、透传角色修复、流式包装。
- `internal/server/orchestrator/orchestrator.go`：中间件与历史处理顺序。
- `conf/conf.go`、`internal/server/api/openai.go`：配置与请求入口接线。

### 工具历史与角色替换

DeepSeek 原生 Responses API 曾对下面这类完整历史报 `No tool output found`：

```text
call A → developer/system 指导 → call B → output A → output B
```

`tool_history.go` 仅在完整、无歧义匹配时调整为：

```text
call A → call B → output A → output B → 原指导
```

调用、输出和指导内容均保留。缺失或重复 ID、类型不匹配、用户消息或其他生成边界会使整理放弃，不猜测补齐工具结果。测试覆盖 function/custom 调用、图片及多段输出。

`replaceDeveloperRoleWithSystem` 是渠道上的独立选项。先前正常转换阶段的替换会被原始请求透传覆盖，因此 portable Responses 在最终出站 body 上再次落实该显式选项。角色替换单独不能解决工具批次被打断的问题。

### 流式消息阶段

2026-10-31 绕过 AxonHub 直连 DeepSeek 官方 API，实测同一工具前说明消息开始时为 `final_answer`，结束时变为 `commentary`。AxonHub 原先直接透传，Codex 因开始事件先折叠前文，又因结束事件重新展开。

portable 模式下仍由 `RestoreResponse` 将 `response.output_item.added` 中显式的 `final_answer` 临时改为 `commentary`。对于 DeepSeek 实际返回的缺失阶段，`agentCompatibilityStream` 立即把 `added` 标为 `commentary` 并继续透传文字增量，只暂存同一消息的 `response.output_item.done`：后面出现工具调用时补为 `commentary`，收到 `response.completed` 时补为 `final_answer`。这样前导语保持展开，最终答案在完成时可以折叠，同时不缓存整段正文。GPT/native 流和非流式响应不经过这段阶段推断。

体验上的取舍是：DeepSeek 真正的最终回答要等 `done`/完成事件到达后才触发折叠，但文字仍然实时显示。要恢复与 GPT 一样的开始即折叠，需要上游在开始事件可靠地区分过程与最终消息。不能用“所有消息永久标成 commentary”替代当前修复，否则最终回答也失去正常折叠能力。

### GPT 缓存回归与修复

`dc8e9d56` 曾在每次 GPT 请求末尾临时追加 developer 指导。客户端下一轮回传的历史不包含它，导致上次上游请求及输出无法成为新请求的前缀。`responses/websocket_executor.go` 因此放弃 `previous_response_id`，重连并发送完整上下文。该缺陷影响启用 native 适配的 GPT 渠道，即使这一轮没有实际调用 DeepSeek 也会发生；DeepSeek 自身的缓存未被此次调查认定有问题。

11 月 2 日调查中，15 组相邻实际请求均在该指导位置断开前缀并重新握手；渠道限额触发的切换还会额外带来冷启动。先前的功能验收没有覆盖连续连接与缓存，是这次回归漏检的原因。

`c18afe61` 将原指导完整移到固定工具描述，取消尾部消息注入。新增 `responses/agent_compatibility_test.go` 将适配器接到真实 WebSocket 执行器：顶层 tools 和 additional_tools 两种目录均连续四轮复用一条连接，只发送新增工具结果；覆盖普通工具、DeepSeek 明文调用名称往返及 GPT 加密参数。修改历史早期内容时仍正确重连。用旧实现运行该测试，在第二轮稳定失败。

隔离验收直连官方上游，使用 `gpt-6-astra` 和独立会话，不经过生产 AxonHub。每组四轮，每轮追加合成数据；结果如下：

| 模式 | 四轮握手数 | 第四轮输入 / 缓存 token | 第四轮命中率 |
| --- | --- | --- | --- |
| 无适配 | 1 | 9,518 / 7,680 | 80.7% |
| 旧适配 | 4 | 10,501 / 6,912 | 65.8% |
| 修复后 | 1 | 10,423 / 8,576 | 82.3% |

修复版第二至四轮缓存 token 为 5,376、6,912、8,576。另一次真实工具选择验证中，模型仅凭工具描述选中了 `axonhub_v2.dispatch_task`，没有执行子 agent。这些结果证明了连接连续性恢复和样本中的缓存改善，不承诺生产固定命中率；每轮新增输入、渠道/账号切换、模型及工具目录变化仍会影响缓存。真实子 agent 生命周期由下述完整管线验收覆盖。

专用模型入口限制 `33069930` 已撤销。普通模型名混合调用现适用于所有 Responses 透传通道，保留 `c18afe61` 的固定描述修复。

新增 `TestLiveAgentCompatibilityIsolated` 通过实际 `Process` 管线、内存数据库、独立 Codex 配置和固定的官方 GPT/DeepSeek 上游验收。它不经过生产网关，也不读取生产数据库或个人 Codex 配置；凭据和模型目录由调用者显式提供。测试检查 GPT 连续出站请求保留既有输入前缀、DeepSeek 首次任务、补发消息、同一代理 follow-up、子代理生成的随机 nonce 回传、原生 GPT 子代理，以及 GPT WebSocket 握手次数和缓存用量。

真实 CLI 的连接行为需要单独说明：Codex 0.166.0 回传历史时会改变 `metadata`、`internal_chat_message_metadata_passthrough`，部分 reasoning 项还改变 `content`。当前 AxonHub 严格比较前缀，因此真实 CLI 仍会重连；关闭全部 native 适配的纯 GPT 对照也复现了同样差异。本次没有修改 GPT 的通用传输或前缀比较。上面的“一条连接”结论适用于受控、前缀稳定的测试，不能推广到真实 CLI 的所有消息形态。真实验收记录握手数，并比较首轮之后 GPT 主代理的缓存 token；5 个百分点的差异是测试回归阈值，不是生产命中率承诺。

2026-11-02 的完整管线样本（Codex 0.166.0，主模型 `gpt-6-astra`）如下。表内比率按旧采样记录中主代理首轮之后各轮的缓存 token 总和 / 输入 token 总和计算；旧采样无法区分字段缺失与明确的 0，见下方核对记录：

| 对照轮次 | 纯 GPT 基线 | GPT + DeepSeek | 结果 |
| --- | --- | --- | --- |
| 首次完整对照 | 98.50% | 83.28% | 功能通过；混合组第 8 次请求记录为 0，缓存门禁失败 |
| 带请求诊断的独立复测 | 98.33% | 98.38% | 功能与缓存门禁通过；出站旧输入及其他顶层字段均稳定 |
| 最终代码复测 | 85.70% | 98.21% | 功能、真实请求前缀及缓存门禁通过；纯 GPT 基线第 6 次请求也记录为 0 |

逐请求复核确认，两处零值都发生在同一个主代理会话的后续请求中，每份 CLI 记录仅有一次 `thread.started` 和一次 `turn.started`。统计只取 `gpt-6-astra`；子代理使用 `gpt-6-luna` 或 `deepseek-flash`，未混入主代理序列，也已排除主代理第 1 次请求。

| 样本 | 主代理请求序号 | 输入 / 记录的缓存 token | 相邻请求的缓存 token |
| --- | --- | --- | --- |
| 首次混合组 | 8 | 16,787 / 0 | 第 7 次：16,128 |
| 最终纯 GPT 基线 | 6 | 15,863 / 0 | 第 5 次：15,360；第 7 次：15,616 |

这里的“请求”是同一次用户任务内的连续模型调用，不是新建会话或子代理首轮。不过，旧测试使用 `gjson.Int()` 直接读取计数，字段缺失或 null 也会变成 0；存档没有保留字段存在性，无法事后证明上游明确返回过 0。准确结论应是“后续请求的用量记录出现零值，明确未命中与用量缺失尚未区分”，不能直接定性为官方缓存完全失效。

测试现已保留 response ID、原始 `input_tokens_details`，并将缺失或非数值计数记为 null 和 `usage_error`，使验收失败而非按 0 计算。离线测试覆盖明确 0、正值、缺失、null 和错误类型；11 月 3 日已使用修正后的记录再次验收，结果见下文。两组旧记录都出现该现象，不能简单归因于 DeepSeek 适配。后续成功样本不抹去首次失败记录。诊断复测中，纯 GPT 主代理 9 轮/9 次握手，混合组 10 轮/10 次握手；重连没有自动等同于缓存未命中。保留真实前缀检查、缓存对照和上游异常记录，升级时继续复核。

完整管线验收报告保存在 `~/.local/state/axonhub-tests/20261102-native-mixed/` 的 `acceptance*` 目录，基线关闭 native 适配并使用 GPT 子代理，混合组启用固定工具并将首个子代理换成 DeepSeek，两组执行相同的任务与消息流程。`context canceled` 只有在下游 HTTP 请求确实取消且错误也是取消时记为客户端中断；其他错误及上游失败事件仍使测试失败。消息打断后的继续执行、同一目标 follow-up 和 nonce 回传均需通过，不能因出现取消就跳过功能检查。

脱敏验收与测试驱动保存在 `~/.local/state/axonhub-tests/20261102-gpt-cache-c18afe61/`；两批测试的临时凭据均已删除。源码修复与生产部署状态应分开记录，当前状态见文末。

2026-11-03，使用统一模型列表的真实 Process 管线对照验收通过。目标模型为 `deepseek-v4.1-flash`，本次隔离测试的上游入口选用 OpenCode Go；该选择只用于覆盖实际协议，不构成运行时渠道条件。检查了首次任务、补发消息、同一子代理 follow-up、随机 nonce 回传、GPT 加密子代理和既有输入前缀。首个完成响应之外，纯 GPT 组为 159,360 / 178,684（89.19%），混合组为 82,432 / 83,225（99.05%）缓存 / 输入 token。OpenAI 上游期间出现过载和 server_error，重试后恢复；这些失败请求不计为缓存 0。

修正后的记录确认，纯 GPT 组第 9 个完成响应明确返回 `cached_tokens: 0`，第 8、10 个响应均为 16,128；混合组只在第 1 个完成响应为 0。前者是关闭兼容的对照组中的后续轮次，后者是新工具目录的首轮。这区分了首轮冷启动与后续零值，但不推断上游缓存失效原因，也不保证固定命中率。记录位于 `~/.local/state/axonhub-tests/20261103-all-channels/`；临时上游凭据已删除。

部署后直接网关验收的首次运行在 300 秒时限结束。已观察到 DeepSeek 创建、补发消息及 follow-up 调用，两个 GPT 子代理的结果文件已出现；DeepSeek 的 follow-up 结果文件尚未生成，因此该次记为失败。该测试的 relay 原有 120 秒总请求时限也可能截断网关重试中的长请求。后续验收由 CLI 整体 8 分钟时限控制，relay 使用请求上下文取消，功能断言保持不变。首次报告 `deployed-opencode/` 名称沿用目标模型的来源称呼；实际测试并未绑定该渠道，均走网关正常路由。

随后 `deployed-v4_1-retry/` 直接网关复测在 198.31 秒通过：主模型 `gpt-6.1-sol`，DeepSeek 目标 `deepseek-v4.1-flash`，另有 `gpt-6-luna`、`gpt-6.1-sol` 两个原生 GPT 子代理。五个结果文件内容、DeepSeek 两次明文回传及两个 GPT 加密创建调用全部核对通过。测试转发层未注入适配，未指定渠道 ID；该结果验证运行中的 `bb451d04`，不覆盖桌面 UI 折叠的视觉验收。后一次成功不抹去首次超时记录，也不能单凭两次时长差异证明超时原因。

## 跟随上游升级

1. 记录当前源码提交、镜像、配置及验收结果；保留可回退的分支或标签。`git fetch upstream --tags` 后选择目标版本，阅读其迁移与发布说明，不把当前补丁数量当作目标版本仍缺失功能的证据。
2. 在升级分支上比较目标版本与上述各项补丁。逐项判断上游是否已有等价行为；仍需要的补丁按表中顺序移植。处理冲突时重点核对历史组装、中间件先后、透传和流式完成事件，不能仅以编译通过认定语义一致。
3. 对已被上游覆盖的补丁，在移除本地实现后运行同一套行为验收。工具历史整理与角色替换、阶段修复各自独立判断，不因某一项修复就整包删除。
4. 更新本文的基线、提交映射、行为差异和部署记录。同步中英文版本；保留回归用例，以便后续发现主线回退。
5. 完成离线回归、隔离实例真实验收，再构建和部署。上游升级可能涉及前端、依赖和数据库；应按目标版本构建完整镜像。此次仅换后端二进制、复用旧镜像前端的方式只适用于相同主线基线且前端未变的补丁发布。

2026-11-02 核查的上游 `unstable` 为 `aa3a265ddc1a7bb0787d56020a1aa77d8ec0e036`。其中工具输出排序修复 [#2633](https://github.com/looplj/axonhub/pull/2633) 作用于 Responses→Chat 转换，尚不能替代本方案的加密任务参数与 `agent_message` 处理。该版本仍使用严格 WebSocket 输入前缀比较。此结论对应该提交，未来应重新核查。

补丁退休依据应是目标版本实际通过原始失败场景，而非 PR 标题相似。工具输出协议转换问题与原生 Responses 透传历史问题可能发生在不同路径。可参考 [DeepSeek Responses 文档](https://api-docs.deepseek.com/guides/responses_api/)、[工具历史兼容报告](https://github.com/deepseek-ai/awesome-deepseek-integration/issues/740)；阶段字段相关的社区报告见 [OpenCode #57372](https://github.com/anomalyco/opencode/issues/57372)，其中“缺少 phase”与本 fork 实测“开始/结束 phase 不一致”需分别判断。

## 验收清单

从仓库根目录执行离线回归，`llm/` 是独立 Go module：

```bash
go test ./conf ./internal/server/orchestrator -count=1
cd llm
go test ./transformer/openai/agentcompat ./transformer/openai/responses ./transformer/openai/codex -count=1
```

| 场景 | 应观察到的结果 |
| --- | --- |
| 空模型列表关闭适配；或无 collaboration 工具的普通请求 | 不注入新工具；加密参数和流式阶段维持原行为 |
| GPT 开启 native 适配，受控且保持前缀的连续多轮工具调用 | 一条 WebSocket、正确的 previous_response_id 与新增输入；缓存 token 随可复用历史增长；修改历史应重连 |
| 原有 GPT 模型名、开启混合适配 | 固定工具扩展生效；不追加隐藏历史；GPT 加密响应及 final 阶段不变 |
| 任意 GPT 通道 ID、名称及重试切换 | Responses 透传路径得到相同目录；非透传路径保持原行为；错误响应不改写 |
| OpenCode Go 的 DeepSeek | model 枚举、spawn 返回校验、输入识别一致；消息与 follow-up 继续走原生生命周期 |
| DeepSeek 主 agent | 多轮工具调用、真实结果写入及读回正常 |
| GPT 父 agent → DeepSeek 子 agent | 首次任务、同一子 agent 消息及 follow-up 均可消费；GPT 子 agent 正常 |
| 工具历史中间插入 developer/system | 完整批次可继续；真正缺少结果仍失败，不伪造输出 |
| 角色替换开/关、透传与协议转换 | 按配置作用域生效；不扩大到其他渠道 |
| DeepSeek 工具前说明 | added=commentary、done=commentary，文字增量正常，前文持续可见 |
| DeepSeek 最终回答 | added=commentary、done=final_answer，完成后可折叠 |
| GPT 最终回答 | 原有 added=final_answer、done=final_answer |
| 流异常、中断及非流式响应 | 错误保持可见；不伪造成功/最终答案，不丢失完成阶段 |
| Codex 原生图像 | 生成、编辑及路由正常；mask 保护与第三方中继边界保持 |

完整管线测试默认跳过，执行时需要本机 `codex` CLI、包含三个模型的目录，以及权限为 `0600` 的外部 JSON 凭据文件。文件结构为 `{"OAuth":{"access_token":"…","expires_at":"…"},"DeepSeekKey":"…","Catalog":"/absolute/path/models.json"}`；OAuth 可包含账号所需的 `id_token`，使用静态凭据，不刷新或保存 token。可选 `DeepSeekModel` 和 `DeepSeekURL` 用于验收 OpenCode Go 等入口，默认分别为 `deepseek-flash`、`https://api.deepseek.com`。凭据文件与报告目录均须位于源码仓库外。

```bash
AXONHUB_AGENT_ISOLATED_LIVE=1 \
AXONHUB_AGENT_ISOLATED_CONFIG=/secure/path/credentials.json \
AXONHUB_AGENT_TEST_REPORT_DIR=/secure/path/reports \
go test ./internal/server/orchestrator -run '^TestLiveAgentCompatibilityIsolated$' -count=1 -v -timeout=20m
```

报告按 `baseline/`、`mixed/` 分目录。`pipeline-report.json` 仅记录工具类型、目标、标记、nonce 是否回传、用量及连接计数；`native-cli.jsonl` 是合成任务的原始运行记录，不得提交。用完删除临时凭据。此测试通过实际 Process 中间件，所有业务数据都在内存测试库中；旧 relay 测试的限制见下文。

真实验收使用合成任务并显式开启，会产生 API 费用。现有 `TestLivePortableToolHistory`、`TestLivePortableMainAgent`、`TestLiveNativeCodex` 读取 `AXONHUB_AGENT_LIVE=1`、`AXONHUB_AGENT_TEST_URL`（网关根 URL，不含 `/v1`）、`OPENAI_API_KEY`；原生子 agent 测试还需要 `AXONHUB_AGENT_TEST_CATALOG`，可用 `AXONHUB_AGENT_TEST_PARENT` 指定父模型。使用前阅读对应 `*live_test.go`。

`TestLiveNativeCodex` 默认 relay 会自行应用适配，只可连接关闭适配的隔离实例。部署验收须设 `AXONHUB_AGENT_TEST_DEPLOYED=1`，此时 relay 只观察网关响应，不注入或恢复工具；`AXONHUB_AGENT_TEST_PORTABLE_MODEL` 可选择 OpenCode Go 模型。测试使用独立 CODEX_HOME，模型目录需包含待测模型。桌面折叠还需观察实际 UI，完成后的聊天记录无法证明流式过程没有闪烁。

## 部署记录与回滚

运行目录以部署主机的 AxonHub Compose 目录为准，当前为 `~/axonhub`。源码仓库保留实现、配置样例与合成测试；运行目录的 `deployments/` 保存镜像信息、Compose 备份、二进制、验收摘要和回滚脚本。凭据、原始会话和完整运行日志留在受保护的运行目录，不提交进源码。

当前发布记录：

| 项目 | 值 |
| --- | --- |
| 当前运行提交 | `bb451d04`，按模型及 V2 请求能力适配，已移除通道白名单 |
| 当前镜像 | `axonhub-local:v1.0.0-beta11-codex-v2-bb451d04` |
| 当前发布目录 | `deployments/20261103-codex-all-channels-bb451d04/` |
| 本次回滚目标 | 上一镜像 `axonhub-local:v1.0.0-beta11-codex-v2-418d5a89` 及其配置 |
| 图片修复回退镜像 | `axonhub-local:v1.0.0-beta11-native-images`（`d798dc63`），仍保留 |
| 验收 | 离线回归、隔离真实功能与缓存对照通过；健康接口版本/提交匹配；正常路由下的直接网关验收通过 |

每次发布使用新镜像标签，备份现有 Compose，记录源码版本和二进制哈希。服务健康后执行直接网关验收，再更新此表。部署目录中的 `rollback.py` 会先核对部署后 Compose 哈希，匹配才恢复该目录保存的上一份 Compose 并重建 `axonhub` 服务。执行前应核实恢复方向：11 月 2 日回退目录保存的是已撤下的兼容版，其脚本会恢复那个版本，不能将它当作恢复当前健康基线的操作。

若 Compose 后续有变更，脚本会停止，需人工比较后再回退，避免覆盖新配置。这些兼容补丁没有数据库迁移；未来主线升级应另行核对数据库备份与迁移回退条件，切换旧镜像本身不等于回滚数据库。

构建 fork 镜像时传入 `--build-arg AXONHUB_BUILD_VERSION=<发布版本>` 和 `--build-arg AXONHUB_BUILD_COMMIT=<完整提交>`，并设置 OCI revision 标签。未传参时沿用上游 VERSION 文件。`418d5a89` 镜像因上游 VERSION 文件陈旧而在健康接口显示 beta9；该显示不代表源码基线，后续发布应检查健康接口中的版本与提交是否匹配发布记录。
