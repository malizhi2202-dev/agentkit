# SUMMARY: T05 - 夹具自检：断言两套布局常量不同值

- **Change ID**: `agentkit-framework`
- **Task ID**: `T05`（由 `6-review` 第二轮 R2 发现触发）
- **完成时间**: 2026-10-23 13:07
- **AI 角色**: Dev

## 做了什么

R2（变更传播）指出：`repoSubtree` 与 `extractedSubtree` 相邻，`6bbec97` 证明一次批量替换会把两者同时改到；塌缩后失败信息只是 `want false got true`，不指向常量。修复：在用例开头加 3 行运行期自检（常量相等即 `t.Fatalf` 并打印值）。只加断言，期望值与子用例条数不变（RUN 行仍 24）。

## 改动文件

| 文件 | 性质 | 说明 |
|---|---|---|
| `guard_test.go` | 修改 | +7 行（3 行断言 + 4 行注释） |
| `GATE-PINS.md5` | 修改 | 哈希随内容刷新 |

## verify 输出

```text
$ go test -count=1 ./...
exit=0（18/18 包）

$ go test -count=1 -v -run TestGuard_AgentkitImportClassification . | grep -c '^=== RUN'
24

$ gofmt -l guard_test.go
（空 = 已格式化）

$ python3 tools/agentkit_code_pins.py
缺口=0
```

## 阶梯判定（R19）

```
- 停点：rung 2（本仓已有断言风格）—— 3 行 if + Fatalf，没有引入表驱动或参数化。
- 未做：没有把两个常量抽成结构体/枚举（rung 4-5），因为问题不是"常量不好管理"，
  而是"两值意外相等"，断言恰好对症。
```

## 数据库迁移 / 破坏性变更

无 / 无（纯测试代码）。

## 已知小问题

- 自检只在 `-run` 命中该测试时生效；若守卫测试被整体跳过则失效（当前无 build tag 跳过逻辑）。

## 6 维自查

| 维度 | 结论 |
|---|---|
| 正确性 | 23 子用例全 PASS；自检未误报 |
| 可读性 | 失败信息直接打印塌缩后的值，指向根因 |
| 一致性 | 与 T01 的注释教训同源，代码化落地 |
| 安全性 | 不涉及 |
| 可测性 | 该自检本身可被"故意写同值"验证 |
| 边界 | 常量相等是唯一新失败面，不涉及并发 |
