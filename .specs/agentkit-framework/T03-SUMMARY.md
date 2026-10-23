# SUMMARY: T03 - 修 360px 视口横向溢出

- **Change ID**: `agentkit-framework`
- **Task ID**: `T03`（由 `5-test` 第 4 轮发现触发，回 `4-dev` 修复）
- **完成时间**: 2026-10-23 12:40
- **AI 角色**: Dev

---

## 做了什么（一段话）

用 Chrome DevTools Protocol 在 360/768/1024/1440 四个视口真实量测文档宽度，360px 下发现 `scrollWidth=377 > clientWidth=360`。逐元素定位（并排除掉被 `overflow-x:auto` 容器正常滚动的表格）后，唯一贡献者是**一个不可断行的长路径行内 `<code>`**（右边界 363，路径含 `.specs/agentkit-framework/…` 这类无空格长串）。修复只加一条 CSS：`.doc-body code{overflow-wrap:anywhere}`，并显式让 `pre code` 保持 `overflow-wrap:normal`（代码块不应被任意断开）。复测四视口全部 `overflow=False`，360px 恢复为 360。

偏离原计划：本条不在原 TASK 里 —— 它是测试阶段量测发现的真实缺陷，按 kit 的"发现 🔴 → 回 4-dev 修"补记为 T03，而不是在测试报告里写一句"已知小问题"。

## 改动文件

| 文件 | 性质 | 说明 |
|---|---|---|
| `.specs/agentkit-framework/PRODUCT-DESIGN.html` | 修改 | CSS 增加 2 行（长 code 断行规则）+ 1 行注释；HTML 结构未动 |

## verify 输出（必填）

```text
修复前（CDP 实测，360px）：
  width=360 scrollWidth=377 clientWidth=360 overflow=True

根因定位（逐元素 + 祖先 overflow-x 过滤）：
  count=1
  {"t":"CODE","c":"","l":53,"r":363,"w":310}   ← 唯一溢出元素

修复后（同一条量测）：
  width=360  scrollWidth=360  clientWidth=360  overflow=False  navActive=1
  width=768  scrollWidth=753  clientWidth=753  overflow=False  navActive=1
  width=1024 scrollWidth=1009 clientWidth=1009 overflow=False  navActive=1
  width=1440 scrollWidth=1425 clientWidth=1425 overflow=False  navActive=1

$ grep -q "overflow-wrap:anywhere" .specs/agentkit-framework/PRODUCT-DESIGN.html && echo T03-css-ok
T03-css-ok
```

## 阶梯判定（R19）

```
- 停点：rung 2（本仓已有 CSS 体系）—— 不需要 JS 断行脚本、不需要 grid 重排、
  不需要把长路径拆成多个元素；一条标准 CSS 属性解决。
- 未做：没有给所有元素加 word-break（会把正常文本也切碎），只作用于 .doc-body code。
```

## 数据库迁移

无。

## 破坏性变更

无。桌面宽度下的观感不变（`overflow-wrap:anywhere` 只在无断点可用时生效）；`pre code` 明确排除。

## 已知小问题

- 量测脚本是一次性的（`/tmp` 下，未入仓）；未来做 CI 时应固化为仓内脚本（议题 `ci-minimum-pipeline` 已含此意）。
- 未做真机浏览器矩阵（Firefox/Safari），仅 Chromium 151 实测；`overflow-wrap` 为基线特性，风险低。

## 6 维自查（R6）

| 维度 | 结论 |
|---|---|
| 正确性 | 四视口实测无溢出；AC-1 断言（九段/零外链/锚点）复测通过 |
| 可读性 | 注释写清"为什么只给内联 code"，避免后人扩大成全局 word-break |
| 一致性 | 与 §6 原型在 720px 折叠规则不冲突（该规则改布局，本条只改断行） |
| 安全性 | 不涉及 |
| 可测性 | 有可复跑的量测方法（CDP 脚本），记录在 TEST.md §4.1 |
| 边界 | 360/768/1024/1440 四点覆盖；未测 320（更窄的老机型），登记为残余风险 |
