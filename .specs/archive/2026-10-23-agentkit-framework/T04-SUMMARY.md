# SUMMARY: T04 - 无障碍补强（reduced-motion + focus-visible）

- **Change ID**: `agentkit-framework`
- **Task ID**: `T04`（由 `6-review` 第三轮发现触发）
- **完成时间**: 2026-10-23 13:05
- **AI 角色**: Dev

## 做了什么

第三轮 UI/无障碍快检命中两项真实缺陷：① 有 `html{scroll-behavior:smooth}` 但无 `prefers-reduced-motion` 降级；② 全篇无 `:focus-visible`，键盘焦点回落浏览器默认蓝。各补一条 CSS：`@media (prefers-reduced-motion: reduce){html{scroll-behavior:auto}}`、`:focus-visible{outline:2px solid var(--brand);outline-offset:2px}`。未动版面、token、断点。

## 改动文件

| 文件 | 性质 | 说明 |
|---|---|---|
| `PRODUCT-DESIGN.html` | 修改 | +4 行（2 条规则 + 2 行注释） |

## verify 输出

```text
$ grep -q "prefers-reduced-motion" PRODUCT-DESIGN.html && grep -q "focus-visible" PRODUCT-DESIGN.html && echo T04-ok
T04-ok

$ （CDP 四视口复测）
width=360  overflow=False  navActive=1
width=768  overflow=False  navActive=1
width=1024 overflow=False  navActive=1
width=1440 overflow=False  navActive=1
```

对比度实测（hex 兜底色，9 组）：14.85 / 16.36 / 8.81 / 5.00 / 4.77 / 5.96 / 6.49 / 6.39 / 5.96 —— 全部 ≥ WCAG AA 4.5。

## 阶梯判定（R19）

```
- 停点：rung 2（本仓已有 CSS 体系）—— 两条标准媒体查询 + 一条伪类，无需 JS。
```

## 数据库迁移 / 破坏性变更

无 / 无（仅新增规则，不改既有选择器行为）。

## 已知小问题

- 未做 Firefox/Safari 实测；`prefers-reduced-motion` 与 `:focus-visible` 均为基线特性，风险低。

## 6 维自查

| 维度 | 结论 |
|---|---|
| 正确性 | 两条规则命中；四视口无回归 |
| 可读性 | 注释说明"跟随品牌色而非默认蓝"的意图 |
| 一致性 | 焦点环用 `var(--brand)`，与 token 体系一致 |
| 安全性 | 不涉及 |
| 可测性 | grep + CDP 可复跑 |
| 边界 | 未测 320px 窄屏（与 T03 同一残余风险） |
