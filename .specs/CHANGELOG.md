# CHANGELOG

> 每个归档 change 一行。格式：日期 · change-id · 一句话摘要 · 提交 · 新增 LESSONS。

| 日期 | Change | 摘要 | 提交 | 新增 LESSONS |
|---|---|---|---|---|
| 2026-10-23 | `agentkit-framework` | 产品文档（九段 + 7 原型）过审并作为需求基线；AC-1~AC-8 全部挂可执行验证；根守卫夹具 8 处布局字面量恢复（判定式零改动），`go test ./...` 由常驻红转为 18/18 全绿；文档四视口无横向溢出、无障碍补强（reduced-motion / focus-visible） | `e719a69` `7039b7f` `2049508` + 归档提交 | `L-001` |
| 2026-10-23 | `ci-minimum-pipeline` | 最小 CI 落地：`go test -race` + 门禁三件套（钉表/可搬运性/文档断言）+ 覆盖率门槛（core ≥90%）+ nightly `govulncheck`；补齐 `audit`/`preset`/`web` 三个缺口包到 91.6/90.6/91.5；顺带修出真缺陷 3 处（nil panic、缺失的 `--freeze`、钉表不校验哈希） | `3aec3b3` `6ad94b3` | `L-002` `L-003` |
