# SUMMARY: T02 - 四件套状态与根因描述互链一致

- **Change ID**: `agentkit-framework`
- **Task ID**: `T02`
- **完成时间**: 2026-10-23 12:08
- **AI 角色**: Dev

---

## 做了什么（一段话）

把"产品文档 / REQUIREMENT / STATE / CONTEXT"四处的同一件事说成同一句话。具体：`PRODUCT-DESIGN.html` 状态由「起草中」晋级「已过审」并写入背书原话与日期（用户 2026-10-23「好的同意了」），§0 的状态说明由告警块改为过审块，页脚同步；`CONTEXT.md` §8.1-1 从"1 行常量"更正为事实——`6bbec97` 批量替换连累 **8 处夹具字面量**，并标注 ✅ 已修复与证据；`CONTEXT.md` §8.2 债项行改写为已收口；新增两条已锁决策（过审、两套布局常量必须不同值）与一个域语言词条「夹具塌缩」；`STATE.md` 重写为活跃 change = `agentkit-framework`、阶段 DEV 完成待 `5-test`，并把三项未做事项按 R18.4 登记为议题。

偏离原计划：`REQUIREMENT.md` AC-5 与 `TASK.md` T01 里原先写"22 条子用例（17+5）"，实测为 **23 条（17+6）**、`=== RUN` 24 行，已一并更正 —— 宁可改文档，不让文档说错数。

## 改动文件

| 文件 | 性质 | 说明 |
|---|---|---|
| `.specs/agentkit-framework/PRODUCT-DESIGN.html` | 修改 | 状态标「已过审」+ 背书行；§0 说明块；页脚；术语表"审核状态"行 |
| `.specs/CONTEXT.md` | 修改 | §8.1-1 根因与修复证据、§8.2 债项收口、2 条已锁决策、1 个术语 |
| `STATE.md` | 重写 | 活跃 change / 阶段 / 产物表 / 阻塞与议题 / 决策日志 / 健康检查 |
| `.specs/agentkit-framework/REQUIREMENT.md` | 修改 | AC-5 条数更正为 23（RUN 24） |
| `.specs/agentkit-framework/TASK.md` | 修改 | T01 action 条数同步；两个 task 回填 done |

## verify 输出（必填）

```text
$ bash -c 'grep -q "agentkit-framework" STATE.md && grep -q "8 处" .specs/CONTEXT.md \
    && grep -q "状态：已过审" .specs/agentkit-framework/PRODUCT-DESIGN.html \
    && grep -q "好的同意了" .specs/agentkit-framework/PRODUCT-DESIGN.html \
    && ! grep -q "状态：起草中" .specs/agentkit-framework/PRODUCT-DESIGN.html && echo AC-7/AC-8 ok'
AC-7/AC-8 ok
```

补充证据：

```text
$ grep -c "已过审" .specs/agentkit-framework/PRODUCT-DESIGN.html
4
$ grep -n "起草中\|status-draft" .specs/agentkit-framework/PRODUCT-DESIGN.html
85:.pill.status-draft{...}          ← 仅保留 CSS 类定义（历史状态样式）
86:.pill.status-draft .dot{...}
（无任何正文状态标仍是"起草中"）
```

## 阶梯判定（R19）

```
- 停点：rung 2（本仓已有）—— 文档状态、根因、条数的表述都已存在于
  CONTEXT/REQUIREMENT/STATE 三处，本次只做"同一句话说三遍且一致"，不新增任何文档结构。
- 未做：没有为四件套引入自动生成脚本（rung 1：目前没有第二条 change 需要它）。
```

## 数据库迁移

无。

## 破坏性变更

无。仅文档与状态；产品文档正文（§1~§9 的内容）未改写，只动状态标、§0 说明块、页脚与术语表一行。

## 已知小问题

- 文档里"8 处"是本次修复的准确计数；若后续再改夹具，需同步三处（CONTEXT §8.1、REQUIREMENT AC-5、TASK T01），目前靠人工。
- `STATE.md` 的健康检查勾选依赖人工跑命令，尚无 CI 强制（议题 `ci-minimum-pipeline`）。

## 6 维自查（R6）

| 维度 | 结论 |
|---|---|
| 正确性 | AC-7/AC-8 脚本通过；四处表述一致（8 处字面量 / 判定式不动 / 23 条子用例） |
| 可读性 | §0 状态块从"警告"改为"过审"，读者不会再去找不存在的待办 |
| 一致性 | 产品文档 ↔ REQUIREMENT ↔ TASK ↔ STATE ↔ CONTEXT 五处口径统一 |
| 安全性 | 不涉及 |
| 可测性 | 两个 AC 都落在 grep/脚本上，可重跑 |
| 边界 | 仅文档；不影响运行期 |
