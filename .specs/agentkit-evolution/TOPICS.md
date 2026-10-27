# TOPICS — agentkit 演进议题申报（D-discovery 第 1 轮）

> change-id: `agentkit-evolution`（R16.5 议题即 change）
> 轮次：第 1 轮 · 拆解时间 2026-10-23 · 拆解人：AI（D-discovery）
> 用户诉求原话：「让我们头脑风暴下 1 还有那些可以改进的 2 目前网络上其他竞品实现的咋样 包括GitHub开源的」

---

## 1 · 问题定义

agentkit 已完成一轮「产品基线验收」change（`.specs/archive/2026-10-23-agentkit-framework/`）：18 包、341 个测试、92.1% 语句覆盖率、8 道门禁全绿、portability 判据通过。**框架自身是健康的**。

问题不在"能不能跑"，而在**"作为一个要给接入方长期用的框架，它的能力面与工程化面相对竞品的位置在哪"**。本议题要回答两问：
1. 还有哪些值得改进（按事实依据拆解，不凭感觉）；
2. 同类开源实现（尤其 Go 生态）现在做到什么程度，哪些是"必须有"、哪些是"agentkit 可以不走"。

## 2 · 事实底座（第 0 步只读扫描产出）

| # | 事实 | 证据 |
|---|---|---|
| F1 | 无任何可观测性埋点：`opentelemetry` / `prometheus` / `Span` / `Tracer` 在生产代码 0 命中 | `grep -rniE` 全仓生产 `.go` |
| F2 | 无 token / 成本计量：`model/` 不解析响应 `usage` 字段 | `grep -rniE "usage\|prompt_tokens" model/` 零命中 |
| F3 | 无流式：`web/` 无 `Flusher` / `text/event-stream` | `grep -rniE "flusher\|text/event-stream" web/` 零命中 |
| F4 | 无 MCP：`MCP` 生产代码 0 命中 | `grep -rni "MCP"` 全仓 |
| F5 | 无持久化/断点恢复：`resume` / `Checkpoint` 仅有 `kit.snapshot()`（注册表快照，非执行状态） | `kit/kit.go:389` |
| F6 | 有重试（`MaxAttempts` / `RetryBackoff`）与并行+步超时（`tools/runner.go:110`），**这两项不缺** | `model/client.go:54-65`、`tools/runner.go:8-50` |
| F7 | 审计有 `TraceID` 字段**但由调用方填**，框架不产生 trace | `audit/audit.go:76-88` |
| F8 | 无 CI：无 `.github/workflows`、无 `.gitlab-ci.yml` | `ls -d .github/workflows` 未命中 |
| F9 | 3 个包覆盖率低于自定 core 90% 门槛：`web` 85.4 / `preset` 88.1 / `audit` 88.4 | `go tool cover -func` 实测 |
| F10 | 端到端性能未测（只有单点基准），`govulncheck`/`gosec` 未装 | `docs/architecture.md:99`、归档 `TEST.md` §3.1 |
| F11 | 产品文档与代码一致、无半成品：`TODO`/`FIXME` 生产代码 0 命中 | `grep` 全仓 |
| F12 | 文档仍有宿主布局悬空指针（`pkg/agentkit/…`、`.specs/adr/…`） | `.specs/ALIGNMENT.md` D6-1/D6-2 |

## 3 · 子议题状态表

| 子议题 | 一句话 | 依据 | 轮次 | 审核标 |
|---|---|---|---|---|
| `observability` | 没有可观测性埋点与成本计量，接入方上线后是黑盒 | F1 / F2 / F7 | 第 1 轮 | **通过**（2026-10-23） |
| `ecosystem` | 无 MCP、无流式，模型/工具生态接入能力落后于主流水位 | F3 / F4 / F6 | 第 1 轮 | **通过**（2026-10-23） |
| `orchestration` | 单 agent 循环，无图编排、无持久化恢复、无人在环 | F5 / F6 | 第 1 轮 | **通过**（2026-10-23） |
| `delivery-gates` | 门禁能跑但没人自动跑：无 CI、3 包覆盖率缺口、CVE 与端到端性能空白 | F8 / F9 / F10 | 第 1 轮 | **通过**（2026-10-23） |
| `developer-experience` | 接入方上手路径靠读 README，无脚手架 / llms.txt，文档有悬空指针 | F11 / F12 | 第 1 轮 | **通过**（2026-10-23） |

## 4 · 轮次记录

- **第 1 轮**：2026-10-23 拆解完成 → 逐个走 ①-④（GD 门：4 项 2:1、1 项全票）→ ⑤ 人审 **5/5 全通过**（用户 2026-10-23 选项提交）→ 已汇入 `ROADMAP.md`。
- ~~未开第 2 轮~~ → **第 2 轮完成**（2026-10-23，用户六点愿景）：5 个子议题 ①②③④ 走完，⑤ 人审 **5/5 通过**（含 G1 澄清后重做的 `agent-shell`）→ 已汇入 `ROADMAP.md` §7。

## 5 · 竞品基线（第 ② 步共用，附来源）

| 项目 | 语言 | 可观测性 | 协议/生态 | 编排与恢复 | 工程化 |
|---|---|---|---|---|---|
| [Eino](https://github.com/cloudwego/eino)（字节 CloudWeGo） | Go | callback aspects（logging/tracing/metrics 注入点） | 组件生态 eino-ext（OpenAI/Claude/Gemini/Ark/Ollama…），流式为一等公民 | compose 图 + ADK；interrupt/resume for HITL | golangci-lint、coverage badge |
| [tRPC-Agent-Go](https://github.com/trpc-group/trpc-agent-go)（腾讯） | Go | OpenTelemetry tracing+metrics、Langfuse 示例 | MCP 工具、AG-UI、A2A、知识检索 | GraphAgent（对标 LangGraph）、Chain/Parallel/Cycle、session/memory 持久化 | CI + codecov badge |
| [ADK for Go](https://github.com/google/adk-go)（Google） | Go | 依托 GCP/OTel 生态 | 丰富工具生态、多 agent 组合 | 多 agent 系统、部署无关 | nightly CI、`adk.dev/llms.txt` 给 agent 用 |
| [LangGraph](https://github.com/langchain-ai/langgraph)（LangChain） | Python/TS | LangSmith 深度可视化 | 生态整合 | **durable execution** + HITL interrupts + 记忆 | 商业部署平台 |
| [MCP](https://github.com/modelcontextprotocol/modelcontextprotocol) | 协议 | — | 工具/资源/提示的标准化协议（schema 版本 `2026-07-28`） | — | 版本化规范 + JSON Schema |
| [OTel GenAI semconv](https://github.com/open-telemetry/semantic-conventions-genai) | 规范 | GenAI 客户端与 MCP 的 span / metric / event 语义约定 | — | — | 独立仓库维护 |

> agentkit 的差异化不在上表任何一列，而在**治理**：D1-D4 出域/入域/中立/未归类判据、单一授权谓词 `tools.Registry.LookupAuthorized`、fail-closed 默认值。竞品普遍不提供这套"工具与数据的权限分类门禁"。

---

## 6 · 第 2 轮：用户六点愿景拆解（2026-10-23）

**新增输入**（用户原话，逐字）：
> 1 这个框架既能提供http的传统web 接口
> 2 又能提供再开发的时候把这个 接口或者自身的能力封装为mcp或者其他可以被其他agent或者框架调用的 就是一种能力
> 3 语言集成路由层 把本项目的接口 或者能力全部集成 外部也可以通过自然语言调用这些能力
> 4 集成样式agent 主要就是集成3 既能页面对话调用 作为提供的功能 也能机器注册表里注册 也能根据ata协议或者其他协议注册到一个注册中心 被其他人调用
> 5 给定一个老旧的服务就可以快速的自己扫描转化为上述我说的
> 6 接入现在爆火的的可以重新逆向的那个github那几个开源的 其中选一个能力最强的 可以给所有项目 或者运行的软件套一个上述能力的壳
> 这些都要

**与第 1 轮的关系**：第 1 轮解决"框架自身该补什么"（可观测性/生态导入/编排/门禁/DX）；第 2 轮解决"框架要不要变成能力平台"（导出面/编目/扫描/壳）。两轮结论**互补不冲突**：第 1 轮的 `ecosystem` 是**导入**（MCP 客户端），第 2 轮的 `protocol-surfaces` 是**导出**（MCP 服务端）。

### 第 2 轮子议题状态表

| 子议题 | 一句话 | 依据 | 轮次 | 审核标 |
|---|---|---|---|---|
| `capability-model` | HTTP 路由与工具现在两套描述，需要统一 Capability 并支持多投影 | 用户点 2+3；`web/web.go:251`、`tools/registry.go:211` | 第 2 轮 | **通过**（2026-10-23） |
| `protocol-surfaces` | 能力如何导出成 MCP/A2A 并注册到注册中心，且不绕过治理 | 用户点 2+4；`grep MCP` 零命中 | 第 2 轮 | **通过**（2026-10-23） |
| `legacy-onboarding` | 老服务怎么快速变成能力清单（扫描 → 提案 → 确认） | 用户点 5；本仓无扫描器 | 第 2 轮 | **通过**（2026-10-23） |
| `agent-shell` | 选哪个开源 agent 做壳、壳与能力的边界在哪 | 用户点 6（表述待澄清 G1） | 第 2 轮 | **通过**（2026-10-23） |
| `platform-positioning` | 框架 vs 平台：核心是否保持零依赖可搬走 | 六点愿景 + `docs/design.md:6-77` | 第 2 轮 | **通过**（2026-10-23） |

### 第 2 轮新增竞品基线

| 项目 | 关键事实（已联网核实） |
|---|---|
| [A2A](https://github.com/a2aproject/A2A) | JSON-RPC 2.0 + AgentCard 发现 + SSE；**有官方 Go SDK**；Linux Foundation 托管 |
| [MCP Registry](https://github.com/modelcontextprotocol/registry) | Go 实现；**API v0.1 已冻结**（2025-10-24）；`mcp-publisher` 发布；命名空间需 GitHub/DNS 验证 |
| [OpenHands](https://github.com/OpenHands/OpenHands) | Agent Canvas 控制台 + Agent Server REST + 多后端（可接 ACP 兼容 agent） |
| [vLLM Semantic Router](https://github.com/vllm-project/semantic-router) | Go；语义路由做**模型**决策，非能力路由（层级不同，可借鉴形态） |
| [openapi-mcp-server](https://github.com/snaggle-ai/openapi-mcp-server) | OpenAPI 规格 → MCP 检索/暴露的成熟做法 |

### 第 2 轮待澄清

- ~~**G1**~~ **已澄清（2026-10-23）**：用户指两类项目 —— ① 号称逆向任何软件/项目（`morluto/rea` 等）；② 号称给任何软件加 agent 能力的壳（`SkillAnything` / `gym-anything` / `ACI`）。据此把议题重写为「能力提取器 vs 能力宿主」二分，结论见 `D-agent-shell.md` §0/§3。
