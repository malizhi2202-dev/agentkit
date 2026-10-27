# ROADMAP — agentkit 演进路线（仅含已过审结论）

> change-id: `agentkit-evolution` · 来源：第 1 轮 D-discovery · 生成 2026-10-23
> 本文件只收**已过 ⑤ 人审门**的子议题（R16.3）；5/5 全部通过。
> 本阶段零代码改动，下列《建议改动清单》**未经执行**。

---

## 1 · 已过审结论（5 项）

| 子议题 | 结论 | 工件 |
|---|---|---|
| `observability` | 框架生成 TraceID 贯穿 + 注入式 `Trace` 接口 + 解析 `usage` 做 token 计量；OTel/Prometheus 只做可选插件；不做预算熔断 | `D-observability.md` |
| `ecosystem` | 工具 schema 自动生成 → MCP 客户端导入（**导入必须过 D1-D4 分类**）→ 流式两步走；不做 MCP 服务端、插件市场 | `D-ecosystem.md` |
| `orchestration` | 终止策略加强 + 工具执行前审批钩子 + 并行合并策略可配；**明确不做**图引擎与断点恢复 | `D-orchestration.md` |
| `delivery-gates` | 最小 CI + 覆盖率红线 + 文档断言 + `govulncheck`；不做发布流水/CHANGELOG 自动化 | `D-delivery-gates.md` |
| `developer-experience` | `llms.txt` + 文档路径统一 + `Example*` + quickstart 进 CI；不做 CLI 脚手架 | `D-developer-experience.md` |

## 2 · 战略定位（本轮最重要的判断）

竞品（Eino / tRPC-Agent-Go / ADK-Go / LangGraph）在**能力面全面领先**，但**没有一个把工具级权限治理做成框架内建**。因此：

- **补三条命脉**：可观测性、生态接入（MCP/流式）、工程化门禁。
- **强化差异化**：把 D1-D4 判据 + 单一授权谓词 + fail-closed 做成"外部工具进来必须过门"的卖点。
- **不追长时运行**（图引擎 / 持久化 / 断点恢复）：那建立在"agent 是长时实体"的假设上，与 agentkit"请求内无状态、可整目录搬走"的定位冲突。

## 3 · 建议改动清单（逐条有依据）

| # | 改动 | 落点 | 依据 |
|---|---|---|---|
| C1 | 新增最小 CI：`go test -race` + 钉表 + portability | 新建 `.github/workflows/ci.yml` | F8 |
| C2 | 覆盖率红线（core 90%），并先补 3 个缺口包 | `web` 85.4 / `preset` 88.1 / `audit` 88.4 | F9 |
| C3 | 文档断言脚本进门禁（状态标/断言行/链接） | 新建 `tools/doc_assert.py` | F12 |
| C4 | `govulncheck` 进 nightly | CI job | F10 |
| C5 | 框架生成 TraceID 并贯穿 model/tools/audit | `kitctx`、`audit/audit.go:78` | F1/F7 |
| C6 | 注入式 `Trace` 接口（≤3 方法，零第三方依赖） | `kitctx` 注入面 | F1 |
| C7 | 解析响应 `usage` → 每请求 token 计量盖章进审计 | `model/protocol.go`、`audit.Entry.Fields` | F2 |
| C8 | 工具 schema 自动生成（从 Go 结构体） | 新建 `tools/schema.go` | F6 |
| C9 | MCP 客户端导入 + 导入层强制分类（否则按 D4 拒绝） | `tools/registry.go:211` 邻近 | F4 |
| C10 | 流式：`model` 层 SSE 解析 + `web` 层 `Flusher` | `model/client.go`、`web/web.go:31` | F3 |
| C11 | 终止策略（最大轮次 / 重复调用检测 / 可选预算） | `tools/runner.go:110` | F6 |
| C12 | 工具执行前审批钩子（可注入 `Approver`，拒绝即 fail-closed） | `tools/runner.go` + `kitctx` | F6 |
| C13 | 并行结果合并策略可配 | `tools/runner.go:142` | F6 |
| C14 | 文档路径统一（宿主残留 → 仓库相对 + 口径说明） | `README.md:4`、`docs/architecture.md:85` | F12 |
| C15 | 新增 `llms.txt`（包清单/五句起服务/注入点表/门禁命令） | 仓库根 | F12 |
| C16 | `Example*` 函数（pkg.go.dev 可渲染 + 可测试） | 各包 `example_test.go` | F11 |
| C17 | quickstart 进 CI（README 五句真跑一次） | CI job + `samples/` | F11 |

## 4 · 落地顺序（波次）

| 波次 | 内容 | 为什么这个顺序 |
|---|---|---|
| **W1 · 兜底** | C1 → C2 → C3 → C4（`delivery-gates`） | 没有 CI，后面所有"改好了"都无法证明；先把跑道铺好 |
| **W1.5 · 低垂果实** | C14 → C15 → C16 → C17（`developer-experience`） | 成本最低、直接减少接入方摩擦，可与 W1 并行 |
| **W2 · 可诊断** | C5 → C6 → C7（`observability`） | 有了 trace 与计量，才能验证后续改动是否真的变好 |
| **W3 · 生态** | C8 → C9 → C10（`ecosystem`） | schema 生成零依赖先落；MCP 必须与治理同批设计；流式最后 |
| **W4 · 编排** | C11 → C12 → C13（`orchestration`） | 三项都在请求内，改动小；审批钩子要复用 W3 的分类结果 |

## 5 · 未采纳方向（否决记录，避免下轮重炒）

| 方向 | 理由 |
|---|---|
| 图工作流引擎 / 断点恢复 / 持久化 | 与"请求内无状态、可整目录搬走"定位冲突；无接入方实证需求（R19 阶梯律） |
| MCP 服务端（对外暴露工具） | 方向相反且工作量翻倍 |
| 插件市场 / 注册中心 | 无需求实证 |
| token 预算熔断 | 先有计量再谈熔断，策略属产品决策 |
| CLI 脚手架 `agentkit new` | 另一条产品线，YAGNI |
| 发布流水 / CHANGELOG 自动化 | 当前无版本发布节奏 |
| OTel / Prometheus 进核心 | 会破坏"仅 errgroup 一个第三方依赖"的锁定口径；只做可选插件 |

## 6 · 与主干的衔接

- 要落地哪个波次 → **同目录补 `CHANGE.md`**（why 引用本文件 §1，what 引用 §3 对应条目）→ 续走 `1-requirement` 起的主干（R16.5）。
- 不建议一个 change 吃下整张表；按波次切开，每波独立过门禁。
