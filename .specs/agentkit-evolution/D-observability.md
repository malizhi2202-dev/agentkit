# D-observability · 可观测性与成本计量

> 审核: **通过**（第 1 轮 · 2026-10-23） · change: `agentkit-evolution` · 依据: TOPICS F1 / F2 / F7

## ① 头脑风暴（技法：假设反转 + 类比迁移 + SCAMPER）

被反转的既有假设："trace 由接入方生成就够了"（`audit.Entry.TraceID` 允许调用方填，`audit/audit.go:78`）。反转后：**框架不产生关联标识，接入方就无法把一个请求的模型调用、工具执行、审计条目串成一条链**。

| # | 候选方向 | 回应事实 | 影响 | 可行性 |
|---|---|---|---|---|
| 1 | 框架生成 `TraceID` 并贯穿 model/tools/audit | F7 | 高 | 高 |
| 2 | 注入式 `Trace` 接口（对齐既有 `Logger`/`Clock`/`Masker` 注入面） | F1 | 高 | 高 |
| 3 | `model` 层解析响应 `usage` → 每请求 token 计量，盖章进 `audit.Entry.Fields` | F2 | 高 | 高 |
| 4 | 阶段耗时分解（语义选择 / 模型往返 / 工具并行 / 审计） | F1 | 高 | 中 |
| 5 | 结构化日志字段规范（补进既有 `Logger` 注入面） | F1 | 高 | 高 |
| 6 | OTel 适配器（独立模块，不污染核心依赖） | F1 | 中 | 中 |
| 7 | Prometheus `/metrics` 导出 | F1 | 中 | 中 |
| 8 | 工具级 P95 / 失败率聚合 | F1 | 中 | 中 |
| 9 | 分布式上下文传播（透传 `traceparent`） | F7 | 高 | 中 |
| 10 | 采样策略 | F1 | 低 | 高 |
| 11 | token 预算熔断（超预算拒绝/降级） | F2 | 中 | 低（需产品确认） |

## ② 竞品与其他产品做法

| 产品 | 做法 | 可借鉴 | 不可取 |
|---|---|---|---|
| [tRPC-Agent-Go](https://github.com/trpc-group/trpc-agent-go) | OpenTelemetry tracing+metrics 为一等公民，附 Langfuse 集成示例 | 用 OTel 标准语义，不自造字段 | 引入 OTel SDK 依赖 |
| [Eino](https://github.com/cloudwego/eino) | callback aspects：在 OnStart/OnEnd/OnError 等固定点注入 logging/tracing/metrics | "固定注入点"比"到处埋点"可控 | 组件级回调面较大 |
| [LangGraph](https://github.com/langchain-ai/langgraph) | LangSmith 追踪执行路径、状态迁移、运行时指标 | 阶段边界要显式 | 绑定商业后端 |
| [OTel GenAI semconv](https://github.com/open-telemetry/semantic-conventions-genai) | 为 GenAI 客户端与 MCP 定义 span/metric/event 语义 | 字段名跟随标准（`gen_ai.*`） | — |

**关键判断**：竞品都把"可观测性"做成框架内建；agentkit 若继续缺席，接入方只能在业务层自己拼日志——而它恰好是"零依赖、可整目录搬走"的框架，**自造轮子成本应由框架承担一次**。

## ③ 结论与建议

**推荐（按落地顺序）**
1. **框架生成 TraceID 并贯穿**（候选 1）—— 最小改动、最大收益：`kitctx.RequestContext` 已有承载位，`audit.Entry.TraceID` 改由框架填。
2. **注入式 `Trace` 接口**（候选 2）—— 与既有注入面同型，零第三方依赖；接入方接自己的后端。
3. **usage 计量**（候选 3）—— 解析响应 `usage`，盖章进审计字段；这是"成本可见"的唯一前提。

**否决记录**
- 候选 11（预算熔断）：先有计量再谈熔断，且策略属产品决策，本阶段不做。
- 候选 6/7（OTel/Prometheus 适配器）：留作**可选插件**，不进核心（保护依赖口径 R?/`go.mod` 只有 errgroup）。

**批判（第一性原理）**：trace 的本质是"关联标识 + 阶段边界"两件事，不需要引入 SDK 才能成立；把这两件事做对，接不接 OTel 都只是适配层问题。

## ④ GD 议题门（R16.7 · 3 人评审团）

| 角色 | 票 | 理由 |
|---|---|---|
| R-高级产品经理 | 同意 | "上线后是黑盒"是接入方最真实的痛点，成本计量直接对应账单 |
| R-研发负责人 | 有保留 | 同意方向；但注入式 `Trace` 会扩大公开 API 面，必须**接口最小化**（不超过 3 个方法），且不得引入第三方依赖 |
| R-资深用户评测员 | 同意 | 有 TraceID 就能把一次请求的模型/工具/审计串起来，排障效率是数量级差异 |

**裁决**：2 同意 + 1 有保留（非反对）→ 通过，提交 ⑤。

## ⑤ 人工审核门（R16.3 法定硬停点）

- 结论：推荐候选 1 + 2 + 3（TraceID 贯穿 / 注入式 Trace 接口 / usage 计量）
- 影响面：`kitctx`、`audit`、`model` 三个包；涉代码改动，**本阶段不执行**
- **状态：通过** —— 用户原话「通过（① 推荐 TraceID 贯穿 + 注入式 Trace 接口 + usage 计量）」（2026-10-23，DSH 选项提交）
- 晋级：已汇入 `ROADMAP.md`
