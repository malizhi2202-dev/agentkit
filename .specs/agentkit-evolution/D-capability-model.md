# D-capability-model · 能力统一建模与自然语言路由（用户点 2 + 3）

> 审核: **通过**（第 2 轮 · 2026-10-23） · change: `agentkit-evolution` · 依据: 用户原话第 2、3 点 + `web/web.go:251`(`Route`) + `tools/registry.go:211`

## ① 头脑风暴（技法：SCAMPER + 假设反转）

反转假设："HTTP 路由和工具是两种东西，各管各的。"——反转后：**它们其实是同一个东西的两种投影**（入参 schema、出参、描述、权限分类都一样），现在却没有共享抽象：`web.Route` 与 `tools.Tool` 各写一套。

| # | 候选 | 回应 | 影响 | 可行性 |
|---|---|---|---|---|
| 1 | 统一 `Capability` 描述：name / desc / input schema / 权限分类 / 传输面 | 点 2+3 | 高 | 高 |
| 2 | 多投影：同一 Capability 同时生成 HTTP route、MCP tool、A2A skill、NL 索引 | 点 2+3+4 | 高 | 中 |
| 3 | 能力编目（Catalog）：注册即登记，支持检索 | 点 3 | 高 | 高 |
| 4 | NL 路由：复用 `semantic`，从"选工具"扩到"选能力" | 点 3 | 高 | 中 |
| 5 | 能力分类（D1-D4 方向）作为编目必填项 | 治理 | 高 | 高 |
| 6 | 角色可见面（谁能看到哪些能力） | 点 4 | 中 | 高 |
| 7 | 能力版本与弃用策略 | — | 中 | 中 |
| 8 | 能力别名/同义词，提升 NL 命中 | 点 3 | 中 | 高 |
| 9 | 从既有 handler 自动推导 Capability（不要求手写） | 点 1 | 高 | 中 |
| 10 | 能力依赖图（A 调用 B 时的传播） | — | 低 | 低 |

## ② 竞品与其他产品做法

| 产品 | 做法 | 可借鉴 | 不可取 |
|---|---|---|---|
| [MCP](https://github.com/modelcontextprotocol/modelcontextprotocol) | `tools/list` 返回 name+description+inputSchema | 描述字段极简，够 LLM 用 | 只有工具，没有"能力"层 |
| [A2A](https://github.com/a2aproject/A2A) | `AgentCard` 里声明 skills（id/name/description/tags） | **能力描述 + 发现**是协议级概念 | 偏向 agent 间，不是 HTTP 能力 |
| [Eino](https://github.com/cloudwego/eino) | Tool 抽象 + 图节点统一为 Component | 抽象层次少而稳 | — |
| [tRPC-Agent-Go](https://github.com/trpc-group/trpc-agent-go) | 函数即工具（`function.NewFunctionTool`） | 从函数推导 schema，零手写 | — |
| [vLLM Semantic Router](https://github.com/vllm-project/semantic-router) | 语义路由做**模型**选择，不是能力选择 | 路由是可插拔决策层 | 与本议题不同层 |

**关键判断**：MCP 与 A2A 都把"可被外部发现的能力描述"当成协议一等公民；agentkit 现在两套描述（Route/Tool）且都不完整（无版本、无分类、无别名），**这是点 2/3/4 的共同地基**。

## ③ 结论与建议

**推荐**
1. **统一 `Capability` 描述 + 从既有 handler/tool 自动推导**（候选 1+9）—— 保住"接口即工具"的卖点：接入方不写第二份描述。
2. **能力编目 + 分类必填**（候选 3+5）—— 编目是导出的前提；分类是治理的入口，不允许"先注册后补分类"。
3. **多投影**（候选 2）—— 一个 Capability 生成四种面（HTTP / MCP / A2A / NL 索引），而不是四套实现。
4. **NL 路由扩到能力层**（候选 4）—— 复用既有 `semantic`，不新写检索引擎。

**否决**：候选 10（依赖图）无需求；候选 7（版本化）暂缓到有接入方抱怨时。

**批判（预演失败）**：若 `Capability` 抽象要求接入方**多写一份元数据**，就违背了"接口即工具"的初衷，接入方会退回裸 `net/http`。**必须能从已有签名推导**，手写只作为覆盖手段。

## ④ GD 议题门

| 角色 | 票 | 理由 |
|---|---|---|
| R-高级产品经理 | 同意 | "一份能力、多种暴露"正是用户 6 点的逻辑主干 |
| R-研发负责人 | 有保留 | 统一抽象容易做成"大而全中间层"；必须限定为**描述**，不接管执行 |
| R-资深用户评测员 | 同意 | 不用写第二份描述才可能被采纳 |

**裁决**：2 同意 + 1 有保留 → 通过，提交 ⑤。

## ⑤ 人工审核门

- 结论：推荐候选 1+9 / 3+5 / 2 / 4
- 影响面：`web`、`tools`、`semantic`；涉代码改动，本阶段不执行
- **状态：通过** —— 用户原话「通过（① 统一 Capability + 一物四投影，必须从现有签名自动推导）」（2026-10-23，DSH 选项提交）
- 晋级：已汇入 `ROADMAP.md` §7
