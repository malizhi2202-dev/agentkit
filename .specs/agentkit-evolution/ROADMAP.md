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

---

## 7 · 第 2 轮已过审结论（5 项 · 2026-10-23）

| 子议题 | 结论 | 工件 |
|---|---|---|
| `capability-model` | 统一 `Capability` 描述，**必须从既有 handler/tool 自动推导**；一物四投影（HTTP / MCP / A2A / NL 索引）；编目时分类必填 | `D-capability-model.md` |
| `protocol-surfaces` | MCP 服务端 + A2A AgentCard 双导出；**对接官方 MCP Registry，不自建**；导出面单独白名单，出域能力禁止导出，调用落审计 | `D-protocol-surfaces.md` |
| `legacy-onboarding` | OpenAPI 优先 → 代码扫描（Go AST）→ 手工兜底；**扫描只出提案**，人工确认后才进编目；产物 = 环境规格 + 能力清单 + 分类建议 | `D-legacy-onboarding.md` |
| `agent-shell` | **A 类逆向器（rea 等）当 MCP 工具接进来，不自研**；B 类（SkillAnything / gym-anything / ACI）当形态参考，产物统一为 MCP/A2A；壳走 OpenHands / Agent Server + ACP；壳无授权权 | `D-agent-shell.md` |
| `platform-positioning` | **形态 B**：核心 18 包保持零依赖可搬走；新增 `agentkit serve` 侧车承载编目/导出/扫描/注册；UI 不自研 | `D-platform-positioning.md` |

### 7.1 六点愿景 ↔ 结论映射

| 用户点 | 落点 |
|---|---|
| 1 传统 HTTP 接口 | 现状已具备（`web`），列为 Capability 的投影面之一 |
| 2 能力封装为 MCP 供他方调用 | `capability-model` + `protocol-surfaces`（MCP 服务端导出） |
| 3 自然语言集成路由层 | `capability-model`（编目 + 复用 `semantic`） |
| 4 集成样式 agent + 注册中心 | `protocol-surfaces`（A2A AgentCard + MCP Registry）+ `agent-shell`（壳） |
| 5 老服务扫描转化 | `legacy-onboarding`（借 A 类工具 + OpenAPI） |
| 6 开源逆向器 / 套壳 | `agent-shell`（A 类作工具、B 类作形态参考、壳走 ACP 路线） |

## 8 · 第 2 轮建议改动清单（平台化，逐条有落点）

| # | 改动 | 落点 | 依据 |
|---|---|---|---|
| P1 | `Capability` 描述类型 + 从 handler/tool 推导 | 新建 `capability` 包；`web/web.go:251`、`tools/registry.go` | `D-capability-model` |
| P2 | 一物四投影（HTTP / MCP / A2A / NL 索引） | `capability` 包 + 各协议适配 | 同上 |
| P3 | 能力编目：注册即登记，分类必填（缺失按 D4 拒绝） | `tools/registry.go:211` 邻近 | 同上 |
| P4 | NL 路由扩到能力层（复用 `semantic`，不新写检索引擎） | `semantic/bind.go:254` | 同上 |
| P5 | MCP 服务端导出（`tools/list` / `tools/call`）+ 导出白名单 | 新建 `mcp` 包 | `D-protocol-surfaces` |
| P6 | A2A AgentCard 生成（skills 映射自 Capability） | 新建 `a2a` 包（用官方 Go SDK） | 同上 |
| P7 | MCP Registry 发布适配（`mcp-publisher` 风格 + 命名空间验证） | `serve` 子命令 | 同上 |
| P8 | MCP 客户端导入（含 rea 类逆向器，分类默认高风险/不导出） | `tools` 导入层 | `D-agent-shell` |
| P9 | 老服务扫描器：OpenAPI 解析 → 提案 | 新建 `tools/scan` 或子命令 | `D-legacy-onboarding` |
| P10 | 环境规格建模（借 gym-anything 的 environment + task） | `capability` 包扩展 | 同上 |
| P11 | `agentkit serve` 侧车（单二进制承载 P5-P10） | 新建 `cmd/agentkit-serve` | `D-platform-positioning` |
| P12 | 壳对接说明 + 参考实现（ACP / Agent Server 调用示例） | `samples/` + `docs/` | `D-agent-shell` |

## 9 · 平台化落地波次（第 2 轮）

| 波次 | 内容 | 前置 |
|---|---|---|
| **W0** | 第 1 轮 `delivery-gates`（CI + 覆盖率红线） | 无——否则平台化改动无法验证 |
| **W5** | P1 → P3 → P2 → P4（`capability-model`） | W0 |
| **W6** | P5 → P6 → P7（`protocol-surfaces`） | W5（投影依赖 Capability） |
| **W7** | P9 → P10（`legacy-onboarding`） | W5 |
| **W8** | P11 → P12 → P8（`serve` 侧车 + 壳） | W6 |

## 10 · 与架构关的关系（回答 0-change 的 0.4 检测）

- 本轮 `platform-positioning` 的裁决（**形态 B**）就是架构级问题的答案：核心不变 + 侧车承载平台能力。
- 因此**建议先跑 `A-architect` 把这条落成 ADR**（框架 → 平台：形态 B、授权真源单一、导出面白名单），再开 `0-change` 补 `CHANGE.md`。
- 若直接开 change，`CHANGE.md` 必须附《架构层影响声明》，引用本文件 §7/§9。

## 11 · 第 2 轮否决记录

| 方向 | 理由 |
|---|---|
| 自研逆向引擎（二进制/行为分析） | 法律风险 + 工程量；已有 `rea` 类 MCP 可接入 |
| 自研控制台 / 聊天 UI | 复用 OpenHands 类壳，非本框架强项 |
| 逆向闭源产品 | 法律与维护不可行 |
| 自建注册中心 | MCP Registry 已是事实标准 |
| 产物为平台专属 Skills | 跨平台性差；统一出 MCP/A2A |
| 一体化平台（形态 C） | 依赖膨胀、护城河消失 |
| 运行时注入壳（S3） | 风险最高，明确不做 |
