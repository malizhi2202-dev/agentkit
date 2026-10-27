# D-developer-experience · 接入方开发者体验

> 审核: **通过**（第 1 轮 · 2026-10-23） · change: `agentkit-evolution` · 依据: TOPICS F11 / F12

## ① 头脑风暴（技法：类比迁移 + 假设反转）

反转假设："DSH 里已经不缺文档了，接入方自己读得懂"——反转后：**读得懂 ≠ 找得到 ≠ 跑得通**。事实是文档里还有宿主布局的悬空指针（F12），`pkg.go.dev` 上也没有可渲染的示例。

| # | 候选方向 | 回应事实 | 影响 | 可行性 |
|---|---|---|---|---|
| 1 | `llms.txt`（给 AI 编码助手的机器可读索引） | F12 | 高 | 高 |
| 2 | 脚手架 `agentkit new` | F12 | 中 | 中 |
| 3 | 5 分钟 quickstart 可执行验证（README 五句真能跑通） | F11 | 高 | 高 |
| 4 | `Example*` 函数（pkg.go.dev 可渲染、可测试） | F11 | 高 | 高 |
| 5 | 错误诊断表扩充（`docs/guide.md` 已有雏形） | F12 | 中 | 高 |
| 6 | 文档路径统一（D6-1/D6-2 悬空指针） | F12 | 高 | 高 |
| 7 | samples 覆盖更多场景（现 1 个 demo） | F11 | 中 | 中 |
| 8 | 从 langchain/eino 迁入指南 | — | 低 | 低 |
| 9 | 视频 / GIF 演示 | — | 低 | 低 |
| 10 | 版本兼容承诺（v0/v1 语义化） | — | 中 | 低 |

## ② 竞品与其他产品做法

| 产品 | 做法 | 可借鉴 | 不可取 |
|---|---|---|---|
| [ADK for Go](https://github.com/google/adk-go) | 提供 `adk.dev/llms.txt` 与 `llms-full.txt`，**明确写给编码 agent 读** | 给 AI 一个入口文件，成本极低 | — |
| [Eino](https://github.com/cloudwego/eino) | 独立 examples 仓 + 完整文档站 + Quick Start | 示例与主仓分离，示例敢写全 | 文档站维护成本高 |
| [tRPC-Agent-Go](https://github.com/trpc-group/trpc-agent-go) | docs 站 + 15 个可运行 examples + 博客 | 每个能力一个可跑示例 | — |
| [MCP](https://github.com/modelcontextprotocol/modelcontextprotocol) | 规范 + 官方文档站 + 版本化 schema | 规范即文档 | — |

**关键判断**：竞品的 DX 投入集中在"**可运行示例 + 机器可读入口**"。agentkit 现在的短板不是文档少（README + 3 篇 docs 已经不错），而是**指针会断**（F12）与**没有给 AI 的入口**。

## ③ 结论与建议

**推荐**
1. **`llms.txt`**（候选 1）—— 一页索引：包清单、五句起服务、注入点表、门禁命令。让任何编码 agent 第一次进仓就能自助。
2. **文档路径统一**（候选 6）—— 把 `pkg/agentkit/…`、`.specs/adr/…` 这类宿主残留统一为"仓库相对路径 + 一句宿主口径说明"。这是唯一会**直接绊倒人**的问题。
3. **`Example*` 函数**（候选 4）—— 与测试同源、可编译、可在 pkg.go.dev 渲染，比 markdown 示例更难腐烂。
4. **quickstart 可执行验证**（候选 3）—— 把 README 的"五句起服务"变成 CI 里真跑一次。

**否决记录**
- 候选 2（脚手架）：CLI 是另一条产品线，YAGNI。
- 候选 8/9（迁移指南、视频）：投入产出比低。

**批判（第一性原理）**：第一分钟摩擦来自"路径能不能解析"和"示例能不能跑"，不是"有没有 CLI"。

## ④ GD 议题门

| 角色 | 票 | 理由 |
|---|---|---|
| R-高级产品经理 | 同意 | 采纳率由第一小时决定；`llms.txt` 对 AI 辅助开发几乎零成本高回报 |
| R-研发负责人 | 同意 | 全是低风险项；`Example*` 还能顺带增加覆盖率 |
| R-资深用户评测员 | 有保留 | "文档路径统一"要一次做完，别只改一半（改一半比不改更乱） |

**裁决**：2 同意 + 1 有保留 → 通过，提交 ⑤。

## ⑤ 人工审核门

- 结论：推荐候选 1 + 6 + 4 + 3
- 影响面：`README.md`、`docs/*`、新增 `llms.txt`、若干 `example_test.go`；本阶段不执行
- **状态：通过** —— 用户原话「通过（llms.txt + 路径统一 + Example* + quickstart 进 CI）」（2026-10-23，DSH 选项提交）
- 晋级：已汇入 `ROADMAP.md`
