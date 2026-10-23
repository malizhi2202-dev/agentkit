# Alignment: agentkit

> S-align 的产物模板。落 `.specs/ALIGNMENT.md`，每轮覆盖更新，历史轮次折叠进文末附录。
> 本文件是五层对齐的唯一留痕：覆盖了哪些提交、发现哪些漂移、合并了什么、哪些已对齐。

## 概要

- **本轮时间**：2026-10-23 13:10
- **基线范围**：`6bbec97..HEAD`（3 条提交，`agentkit-framework` change 全部落库改动）
- **五层状态**：L1 代码 ✔ · L2 原型 ✔ · L3 活文档 ✔ · L4 决策留痕 ✔ · L5 执行留痕 ✔
- **结论**：整体健康 —— 本 change 无新增漂移；存量 1 项 D6（孤儿指针，`6bbec97` 之前既有）已登记议题，不在本轮合并。

## 1. 覆盖的本地提交

| # | commit | 主题 | 归层 |
|---|---|---|---|
| 1 | `e719a69` | 修根守卫夹具布局字面量：恢复 pkg/agentkit 与 agentkit 两套布局 | L1+L5 |
| 2 | `7039b7f` | agentkit 产品基线：产品文档过审 + CHANGE/REQUIREMENT/TASK/TEST 留痕 | L2+L3+L4+L5 |
| 3 | `2049508` | STATE：关闭「未提交」阻塞行，登记提交号与未推送状态 | L5 |

未提交变更（`git status`）：无（本轮 S-align 在 7-integration 归档提交前执行；归档提交随 7 阶段一并落库，见 §5）。

### 五层逐层核对

| 层 | 本 change 是否触碰 | 核对结论 |
|---|---|---|
| L1 代码 | `guard_test.go`（仅测试夹具 + 3 行自检）、`GATE-PINS.md5`（派生哈希） | ✔ 判定函数零改动；哈希与磁盘一致（缺口 0） |
| L2 原型 | `PRODUCT-DESIGN.html` 的 CSS（断行、动效降级、焦点环） | ✔ 页面清单、tokens `:root`、§1–§9 正文均未变 |
| L3 活文档 | `.specs/CONTEXT.md`（§8.1 更正为已修复 + 2 条决策 + 1 术语）、`STATE.md` | ✔ 与代码现状一致（"8 处字面量、判定式不动"） |
| L4 决策留痕 | CONTEXT 已锁决策新增 2 条：过审背书、两套布局常量必须不同值 | ✔ 有日期与出处行号 |
| L5 执行留痕 | `T01/T02/T03` SUMMARY、`TEST.md`、`REVIEW.md`、本文件、`CHANGELOG.md` | ✔ verify 输出为真实命令 |

## 2. 漂移项

| 编号 | 类型 | 层 | 证据 | 描述 | 状态 |
|---|---|---|---|---|---|
| D6-1 | D6 孤儿产物 | L3 | `README.md:4`、`docs/architecture.md:85` | 指向 `.specs/adr/010-*.md`、`.specs/agent-engine/AGENTKIT-LAYERS.md`、`agentkit/samples/agentkit-demo.go.txt` —— 独立副本中均不存在（宿主仓工件） | 已登记议题 `doc-path-realign`，本轮不合并 |
| D6-2 | D6 孤儿产物 | L3 | `docs/guide.md` 引用 `pkg/agentkit/…` 路径 | 同上，宿主布局口径残留；已在 `CONTEXT.md §0` 登记为口径冲突 | 已登记（同议题） |

- 无 D1（代码变了文档未变）：L1 改动为测试夹具，无对外行为 / 接口 / 配置 / 依赖变化。
- 无 D2（决策矛盾）：`CONTEXT.md` 两条新决策与代码、文档表述一致。
- 无 D3（原型漂移）：`PRODUCT-DESIGN.html` 的页面清单与 tokens 未变，仅 CSS 缺陷修复。
- 无 D4（执行留痕缺口）：五个 task 均有 SUMMARY 与 verify 输出。
- 无 D5（需求失配）：8 条 AC 与实现逐条对齐（见 `REVIEW.md` 第一轮表）。

## 3. 合并动作

### 3a. 可自动合并（待确认后执行）

| # | 对应漂移 | 动作 | 目标文件/章节 |
|---|---|---|---|
| — | 无 | 本轮无自动合并项（D6 涉及"是否保留宿主口径"的判断，需人决策） | — |

### 3b. 需人工裁决

| # | 对应漂移 | 选项 | 建议 |
|---|---|---|---|
| 1 | D6-1 / D6-2 | ① 改为仓库相对路径 + 一句"宿主口径"说明；② 保留原样并在文首加"本文档面向宿主仓"告示 | 建议 ①（独立副本优先），随议题 `doc-path-realign` 批量处理 |

### 3c. 建议删除（孤儿产物）

| # | 文件/段落 | 理由 |
|---|---|---|
| 1 | `docs/architecture.md:85` 的 `agentkit/samples/agentkit-demo.go.txt` 指针 | 该文件在本仓不存在（样例已移入 `samples/`），删除或改为 `samples/agentkit-demo.go.txt` |

## 4. 已对齐记录

| 时间 | 漂移编号 | 对应提交 | 合并动作 | 合并提交 |
|---|---|---|---|---|
| 2026-10-23 13:10 | D6-2（部分） | `7039b7f` | 在 `CONTEXT.md §0` 显式登记"文档引用宿主布局路径、本副本不存在" | `7039b7f` |

## 5. 下一轮基线

`2049508` —— 下次 S-align 从此开始扫描（7-integration 的归档与 CHANGELOG 提交紧随其后，属 L5 归档动作，不产生新漂移）。

---

## 附录 · 历史轮次

<details><summary>无（本轮为首轮 S-align）</summary>

无。

</details>
