# D-agent-shell · 能力提取器与宿主壳：开源选型（用户点 6 · 第 2 轮修订）

> 审核: **通过**（第 2 轮 · 2026-10-23 · G1 已澄清后重做） · change: `agentkit-evolution`
> 依据: 用户原话第 6 点 + 补充澄清「有几个是号称逆向任何软件和项目开源的项目 也有一个号称可以给任何软件加agent能力的壳的开源项目」

## 0 · G1 澄清结果（原表述歧义已解）

用户说的不是某一类，而是**两类不同角色的项目**，我把它们分开：

| 类别 | 作用 | 代表项目（GitHub 实查） |
|---|---|---|
| **A 能力提取器**（逆向/理解软件） | 把软件的行为、接口、二进制结构"读出来" | `morluto/rea`、`NabilRFG12/Reverse-Engineer-anything`、`amruth-sn/kong`、`miolamio/claude-reverse-engineer` |
| **B 能力宿主 / 套壳**（让软件被 agent 操作） | 把软件包成 agent 可驱动的环境或技能 | `AgentSkillOS/SkillAnything`、`cmu-l3/gym-anything`、`Leoooooli/ACI` |

> 星数为 GitHub 搜索快照；`morluto/rea` 的数值异常偏高且 API 随后限流，**未复核**，不作为判据。

## ① 头脑风暴（技法：角色分解 + 逆向）

反转假设："套壳就是选一个最强项目接进来。"——反转后：**"套壳"其实是两个可分离的职责**（提取能力 / 宿主运行），把它们混成一个选型问题，就必然选错。

| # | 候选 | 回应 | 影响 | 可行性 |
|---|---|---|---|---|
| 1 | 把 A 类（逆向器）当外部 MCP 工具接入，不自己造 | 点 5+6 | 高 | 高 |
| 2 | 把 B 类产物形态（Skills / Environment）作为导出参考 | 点 2+6 | 高 | 高 |
| 3 | 自研逆向器（二进制/行为分析） | 点 5 | 低 | 低（法律+工程量） |
| 4 | 自研控制台壳 | 点 4 | 中 | 低 |
| 5 | 学 gym-anything 的"环境 + 任务"建模，扫描产物出 environment spec | 点 5 | 高 | 中 |
| 6 | 输出平台专属 Skills（Claude Code/OpenClaw 格式） | 点 2 | 中 | 高 |
| 7 | 输出 MCP + A2A（跨平台标准） | 点 2 | 高 | 中 |
| 8 | 以 ACI 式"结构化 agent 接口"作为 Capability 蓝本 | 点 2+3 | 中 | 中 |
| 9 | 直接逆向闭源产品 | — | 低 | 不可行（法律/维护） |
| 10 | 自建注册中心 | 点 4 | 低 | 低 |

## ② 竞品实查（README 级核实）

### A 类：能力提取器

| 项目 | 事实 | 可借鉴 | 风险 |
|---|---|---|---|
| [morluto/rea](https://github.com/morluto/rea) | 「Reverse Engineer Anything」：**一个 MCP** 覆盖二进制、应用行为、运行时；MIT；`npx rea-agents`；有多语言 README 与独立站点 | 它自己是 MCP ⇒ **可直接作为工具被 agentkit 导入**；"从行为反推接口"正是点 5 想要的 | 逆向天然有法律与伦理边界；分析闭源/第三方软件需授权 |
| [amruth-sn/kong](https://github.com/amruth-sn/kong) | 「world's first agentic reverse engineer」 | 同类竞品，验证赛道存在 | 同上 |
| NabilRFG12/Reverse-Engineer-anything | 与 rea 同描述，疑似镜像/衍生 | — | 来源不明，不采用 |

**结论**：A 类**不自己造**。agentkit 的正确姿势是**做 MCP 客户端把它接进来**（与第 1 轮 `ecosystem` 的"导出治理"互补）。

### B 类：能力宿主

| 项目 | 事实 | 可借鉴 | 不可取 |
|---|---|---|---|
| [SkillAnything](https://github.com/AgentSkillOS/SkillAnything) | 473★ MIT Python；七阶段流水线，**为任意软件生成 AI Agent Skills**，面向 Claude Code / OpenClaw / Codex；自称 "meta-skill" | "任意软件 → 结构化能力描述"的完整流水线先例 | 产物是**平台专属 Skills**，跨平台性差 |
| [gym-anything](https://github.com/cmu-l3/gym-anything) | 297★ MIT CMU；**把任意软件变成 agent 环境**（浏览器/IDE/病历系统/CAD），标准 environment API + 任务定义，有 arXiv 论文 | **"环境 + 任务"建模**：软件不只是工具集，是可被操作的环境 | 定位是**评测/训练环境**，不是对外服务 |
| [ACI](https://github.com/Leoooooli/ACI) | 7★ Apache-2.0；「任意应用 → 结构化 agent 可操作接口」的标准提案 | 与我们的 `Capability` 概念同构 | 太早期，无生态 |

**关键判断**：B 类三家的共同点是**"把软件变成 agent 可理解的结构化描述"**；差异在产物形态：Skills（平台专属）/ Environment（评测）/ Interface（标准提案）。**没有一个直接产出 MCP + A2A 的跨平台能力描述**——这正是 agentkit 的空位。

## ③ 结论与建议（G1 澄清后修订）

**推荐**
1. **A 类当工具，不当底座**：agentkit 以 MCP 客户端身份接入 `rea` 这类逆向器，用于点 5 的"老服务理解"；**不实现自己的逆向引擎**（法律与工程量双重不可行）。
2. **B 类当形态参考，不当依赖**：吸收 `SkillAnything` 的"任意软件 → 能力描述"流水线思路与 `gym-anything` 的"环境 + 任务"建模，但**产物统一为 Capability → MCP/A2A**，不产出平台专属 Skills。
3. **壳（宿主）仍走 OpenHands/Agent Server + ACP 路线**：B 类不是壳，是"能力提取 + 结构化描述"，两者不要混为一谈。
4. 点 5 的扫描产物 = **环境规格（environment spec）+ 能力清单 + 分类建议**，人工确认后进编目。

**否决**：候选 3（自研逆向器）、候选 4（自研控制台）、候选 9（逆向闭源）、候选 10（自建注册中心）。

**批判（预演失败）**：若把 `rea` 类工具直接暴露给生产 agent 调用，等于给了一个"能读任意二进制与运行时"的超能力工具。**必须纳入 D1-D4 分类**（大概率是出域/高风险类），并默认不导出、不自动启用。

## ④ GD 议题门

| 角色 | 票 | 理由 |
|---|---|---|
| R-高级产品经理 | 同意 | 明确"借力而非造轮子"，把 scarce 资源放在编目与治理上 |
| R-研发负责人 | 同意 | A 类作为 MCP 工具接入成本低；自研逆向属明确的非目标 |
| R-资深用户评测员 | 有保留 | 逆向类工具的权限与合规提示必须在接入时强制出现，不能默认开 |

**裁决**：2 同意 + 1 有保留 → 通过，提交 ⑤。

## ⑤ 人工审核门

- 结论：A 类（rea 等）作为 MCP 工具接入、不造逆向引擎；B 类（SkillAnything / gym-anything / ACI）作为形态参考，产物统一为 Capability → MCP/A2A；壳仍走 OpenHands 路线
- 影响面：`tools`（MCP 客户端）、`Capability` 编目；本阶段不执行代码
- **状态：通过** —— 用户原话「通过（修订版：逆向器当工具、Skills/环境类当形态参考、壳走 OpenHands 路线）」（2026-10-23，DSH 选项提交）
- 晋级：已汇入 `ROADMAP.md` §7
