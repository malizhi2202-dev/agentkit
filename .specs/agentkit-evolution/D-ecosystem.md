# D-ecosystem · 生态接入（MCP / 流式 / 工具 schema）

> 审核: **通过**（第 1 轮 · 2026-10-23） · change: `agentkit-evolution` · 依据: TOPICS F3 / F4 / F6

## ① 头脑风暴（技法：类比迁移 + 逆向）

**逆向提问**："怎么能让 agentkit 在生态上彻底掉队？"——答：① 只支持一种私有模型协议；② 外部工具接不进来；③ 响应只能等全量返回。这三条恰好对应现状（F3/F4）。

| # | 候选方向 | 回应事实 | 影响 | 可行性 |
|---|---|---|---|---|
| 1 | MCP 客户端适配：外部工具导入 `Registry`，**导入层强制 D1-D4 分类** | F4 | 高 | 中 |
| 2 | 流式响应：`model` 层 SSE 解析 + `web` 层 `Flusher` | F3 | 高 | 中 |
| 3 | OpenAI 兼容协议适配层（当前 `model/protocol.go` 自研） | — | 高 | 中 |
| 4 | 从 Go 结构体自动生成工具 schema（对齐 tRPC `function.WithName` 风格） | F6 | 高 | 高 |
| 5 | 结构化输出（JSON Schema 约束响应） | — | 高 | 中 |
| 6 | 多模型路由（按成本/能力选择） | — | 中 | 中 |
| 7 | prompt caching（供应商侧 `cache_control`） | F2 | 中 | 中 |
| 8 | 按错误类型细化的重试/熔断 | F6 | 中 | 高 |
| 9 | MCP 服务端（把 agentkit 工具暴露给别人） | F4 | 中 | 低 |
| 10 | 插件市场 / 注册中心 | — | 低 | 低 |

## ② 竞品与其他产品做法

| 产品 | 做法 | 可借鉴 | 不可取 |
|---|---|---|---|
| [MCP](https://github.com/modelcontextprotocol/modelcontextprotocol) | 工具/资源/提示的标准化协议，schema 版本化（当前 `2026-07-28`） | 只做客户端导入即可吃到全生态 | 自己发明协议 |
| [tRPC-Agent-Go](https://github.com/trpc-group/trpc-agent-go) | `mcptool` 直接包 MCP 连接；SSE/AG-UI 流式；Go 1.21+ | MCP 接入是"标配"不是"特色" | 绑定其 tRPC 生态 |
| [Eino](https://github.com/cloudwego/eino) | 流式贯穿编排（框架自动 concat/merge）；组件生态 `eino-ext` 覆盖 OpenAI/Claude/Gemini/Ark/Ollama | 流式应由框架统一处理 | 组件抽象较重 |
| [ADK for Go](https://github.com/google/adk-go) | 丰富工具生态，多 agent 组合 | 工具注册要简单 | 偏 Gemini/GCP |

**关键判断**：MCP 已是事实标准，**"支持 MCP"本身不构成差异化**；agentkit 的机会是**"外部工具进来先过治理门"**——竞品普遍没有工具级权限分类，agentkit 有 `tools.Registry.LookupAuthorized` 与 D1-D4。

## ③ 结论与建议

**推荐**
1. **工具 schema 自动生成**（候选 4）—— 零依赖、纯本仓能力、直接降低接入方写样板的心智成本，先做这个。
2. **MCP 客户端导入 + 治理前置**（候选 1）—— 外部工具在导入时**必须显式声明方向分类**，否则按 D4 拒绝注册；这是 agentkit 独有的卖点。
3. **流式响应**（候选 2）—— 分两步：`model` 层解析 SSE，`web` 层加 `Flusher` 支持；不要一次做全链路流式编排。

**否决记录**
- 候选 9（MCP 服务端）：方向相反且工作量翻倍，YAGNI。
- 候选 10（插件市场）：无需求实证。
- 候选 3（OpenAI 兼容层）：若 MCP + 现有协议够用，先不做；留观察。

**批判（预演失败）**：若 MCP 工具未分类就被 `Registry` 接收，D4"未归类即违规"会直接拒绝注册，接入方会觉得"框架在跟我作对"。**必须同时提供分类模板/默认建议**，否则治理会变成摩擦。

## ④ GD 议题门

| 角色 | 票 | 理由 |
|---|---|---|
| R-高级产品经理 | 同意 | 生态接入决定"能不能被采纳"，schema 生成是最快见效的一项 |
| R-研发负责人 | 有保留 | MCP 导入必须与 D1-D4 一起设计，否则等于开后门；流式要评估对现有 `Renderer`/`web.Config` 的侵入 |
| R-资深用户评测员 | 同意 | "从结构体生成工具"能省掉最容易写错的一层样板 |

**裁决**：2 同意 + 1 有保留 → 通过，提交 ⑤。

## ⑤ 人工审核门

- 结论：推荐候选 4（schema 生成）→ 1（MCP 导入 + 治理前置）→ 2（流式）
- 影响面：`tools`、`model`、`web`；涉代码改动，本阶段不执行
- **状态：通过** —— 用户原话「通过（推荐 schema 生成优先，MCP 导入必须过治理门）」（2026-10-23，DSH 选项提交）
- 晋级：已汇入 `ROADMAP.md`
