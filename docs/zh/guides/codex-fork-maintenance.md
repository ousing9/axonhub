# Codex 自维护补丁与主线升级

本文是本 fork 的 Codex 补丁维护入口。升级 AxonHub 时，用它确认哪些本地能力需要保留、对应代码在哪里、如何验收，以及何时可以删除兼容补丁。通用接入方式见 [Codex 集成指南](codex-integration.md)；配置定义以 [config.example.yml](../../../config.example.yml) 为准。

## 基线与补丁清单

截至 2026-10-31，维护分支为 `codex/v2-portable-agents`，基于上游 `v1.0.0-beta11`（`b8bb3421`）。自维护代码发布到 CNB，远端分工如下：

| Remote | 地址 | 用途 |
| --- | --- | --- |
| `origin` | `https://cnb.cool/ousing9/axonhub` | 自维护代码的主要推送仓库 |
| `upstream` | `https://github.com/looplj/axonhub.git` | 官方主线，获取版本与合并更新 |
| `github` | `git@github.com:ousing9/axonhub.git` | 保留原个人 GitHub fork；按需显式推送 |

本地设置 `remote.pushDefault=origin`，维护分支跟踪 `origin/codex/v2-portable-agents`。日常推送使用 `git push`，获取官方更新使用 `git fetch upstream --tags`；向 CNB 推送不会自动同步 GitHub。Git remote 配置由同一仓库的 worktree 共用。

新克隆可使用：

```bash
git clone --branch codex/v2-portable-agents https://cnb.cool/ousing9/axonhub
cd axonhub
git remote add upstream https://github.com/looplj/axonhub.git
git remote add github git@github.com:ousing9/axonhub.git
git config remote.pushDefault origin
```

以下是基线之后的四个功能提交，按依赖顺序排列；它们不代表上游已合入的功能。后续文档提交另计。

| 提交 | 自维护能力与原因 | 升级时重点检查 |
| --- | --- | --- |
| `d798dc63` | 官方 ChatGPT Codex 渠道走原生 Images API，保留生成与编辑能力 | Codex 渠道鉴权、图像端点选择、编辑请求转换及透传条件 |
| `dc8e9d56` | DeepSeek 无法消费 Codex V2 的加密任务参数；增加明文任务适配，继续使用 Codex 原生子 agent 生命周期 | 工具目录、`agent_message` 输入、调用名称恢复、`encrypted_function_args` 标记、配置注入 |
| `06c35032` | DeepSeek 工具历史被路径指导消息打断时会误报缺少工具结果；同时修复透传覆盖 developer→system 配置的问题 | 历史组装与协议转换的顺序、完整工具批次配对、最终出站角色替换 |
| `3e82c921` | DeepSeek 流式消息先标记 final，结束时可能改成 commentary，导致 Codex 前文先折叠后展开 | `output_item.added` 的临时阶段与 `output_item.done` 的最终阶段 |

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

这是显式开启的适配层，空配置不改变默认行为：

```yaml
codex_agent_compatibility:
  native_channel_ids: []  # 填入需要注入工具的 GPT Responses 透传渠道 ID
  portable_models: ["deepseek-flash"]
```

`native_channel_ids` 的值来自当前实例，不能照抄另一套数据库的 ID。配置在启动时加载。当前支持的 portable 模型为 `deepseek-flash`；添加其他模型前应单独验证协议行为。

GPT 生产任务一侧保留 `collaboration`，另加 `axonhub_v2.dispatch_task`、`deliver_message`、`continue_task` 三个工具，只用于 DeepSeek。返回时映射回原生 collaboration 调用，并用 `encrypted_function_args: []` 明确表示明文参数。该约定已按 Codex 0.166 验证，Codex 升级时需要复查。它不改变授权、子 agent 生命周期或 GPT 的加密任务行为；既有不透明的加密 DeepSeek 任务也不会被解密。

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

`RestoreResponse` 仅将 portable 模式下 `response.output_item.added` 中 assistant message 的 `final_answer` 改为 `commentary`；保留全部文字增量，并保留完成事件和非流式响应的最终阶段。不增加整段缓冲，也不修改 GPT 阶段。缺失或未知阶段不作推断。

体验上的取舍是：DeepSeek 真正的最终回答在文字输出完毕时才触发折叠。要恢复与 GPT 一样的开始即折叠，需要上游在开始事件可靠地区分过程与最终消息。不能用“所有消息永久标成 commentary”替代当前修复，否则最终回答也失去正常折叠能力。

## 跟随上游升级

1. 记录当前源码提交、镜像、配置及验收结果；保留可回退的分支或标签。`git fetch upstream --tags` 后选择目标版本，阅读其迁移与发布说明，不把当前补丁数量当作目标版本仍缺失功能的证据。
2. 在升级分支上比较目标版本与上述四项补丁。逐项判断上游是否已有等价行为；仍需要的补丁按表中顺序移植。处理冲突时重点核对历史组装、中间件先后、透传和流式完成事件，不能仅以编译通过认定语义一致。
3. 对已被上游覆盖的补丁，在移除本地实现后运行同一套行为验收。工具历史整理与角色替换、阶段修复各自独立判断，不因某一项修复就整包删除。
4. 更新本文的基线、提交映射、行为差异和部署记录。同步中英文版本；保留回归用例，以便后续发现主线回退。
5. 完成离线回归、隔离实例真实验收，再构建和部署。上游升级可能涉及前端、依赖和数据库；应按目标版本构建完整镜像。此次仅换后端二进制、复用旧镜像前端的方式只适用于相同主线基线且前端未变的补丁发布。

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
| 未开启适配、普通 GPT | 工具目录、加密参数和流式阶段维持原行为 |
| DeepSeek 主 agent | 多轮工具调用、真实结果写入及读回正常 |
| GPT 父 agent → DeepSeek 子 agent | 首次任务、同一子 agent 消息及 follow-up 均可消费；GPT 子 agent 正常 |
| 工具历史中间插入 developer/system | 完整批次可继续；真正缺少结果仍失败，不伪造输出 |
| 角色替换开/关、透传与协议转换 | 按配置作用域生效；不扩大到其他渠道 |
| DeepSeek 工具前说明 | added=commentary、done=commentary，文字增量正常，前文持续可见 |
| DeepSeek 最终回答 | added=commentary、done=final_answer，完成后可折叠 |
| GPT 最终回答 | 原有 added=final_answer、done=final_answer |
| 流异常、中断及非流式响应 | 错误保持可见；不伪造成功/最终答案，不丢失完成阶段 |
| Codex 原生图像 | 生成、编辑及路由正常；mask 保护与第三方中继边界保持 |

真实验收使用合成任务并显式开启，会产生 API 费用。现有 `TestLivePortableToolHistory`、`TestLivePortableMainAgent`、`TestLiveNativeCodex` 读取 `AXONHUB_AGENT_LIVE=1`、`AXONHUB_AGENT_TEST_URL`（网关根 URL，不含 `/v1`）、`OPENAI_API_KEY`；原生子 agent 测试还需要 `AXONHUB_AGENT_TEST_CATALOG`，可用 `AXONHUB_AGENT_TEST_PARENT` 指定父模型。使用前阅读对应 `*live_test.go`。

这些测试中的 relay 会自行应用部分适配，尤其 `TestLiveNativeCodex` 会注入工具；应连接关闭该适配的隔离实例，避免与生产适配重复注入。relay 测试不能替代部署后直接通过网关的验收。桌面折叠还需观察实际 UI，完成后的聊天记录无法证明流式过程没有闪烁。

## 部署记录与回滚

运行目录以部署主机的 AxonHub Compose 目录为准，当前为 `~/axonhub`。源码仓库保留实现、配置样例与合成测试；运行目录的 `deployments/` 保存镜像信息、Compose 备份、二进制、验收摘要和回滚脚本。凭据、原始会话和完整运行日志留在受保护的运行目录，不提交进源码。

当前发布记录：

| 项目 | 值 |
| --- | --- |
| 后端功能提交 | `3e82c921` |
| 当前镜像 | `axonhub-local:v1.0.0-beta11-native-images-codex-v2-phase-3e82c921` |
| 前一镜像 | `axonhub-local:v1.0.0-beta11-native-images-codex-v2-history-06c35032` |
| 发布目录 | `deployments/20261031-deepseek-phase-3e82c921/` |
| 验收 | 离线相关包与完整 orchestrator 测试通过；直连网关 DeepSeek 两轮流式/GPT 对照、真实 DeepSeek 主 agent 通过；桌面视觉复验待用户确认 |

每次发布使用新镜像标签，备份现有 Compose，记录源码版本和二进制哈希。服务健康后执行直接网关验收，再更新此表。当前发布目录的 `rollback.py` 会先核对部署后 Compose 哈希，匹配才恢复上一份 Compose 并重建 `axonhub` 服务：

```bash
python3 ~/axonhub/deployments/20261031-deepseek-phase-3e82c921/rollback.py
```

若 Compose 后续有变更，脚本会停止，需人工比较后再回退，避免覆盖新配置。本次阶段补丁没有数据库迁移；未来主线升级应另行核对数据库备份与迁移回退条件，切换旧镜像本身不等于回滚数据库。
