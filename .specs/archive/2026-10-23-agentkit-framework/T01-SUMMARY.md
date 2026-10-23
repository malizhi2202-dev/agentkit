# SUMMARY: T01 - 恢复根守卫夹具的 8 处布局字面量并刷新钉表

- **Change ID**: `agentkit-framework`
- **Task ID**: `T01`
- **完成时间**: 2026-10-23 12:05
- **AI 角色**: Dev

---

## 做了什么（一段话）

只改测试夹具，不改判据：把 `TestGuard_AgentkitImportClassification` 里被 `6bbec97` 批量替换连累的 **8 处路径字面量**恢复为原意 —— `repoSubtree` 回到 `<module>/pkg/agentkit`，4 条相似前缀用例、尾斜杠用例、`..` 用例回到 `pkg/agentkit…`，抽取布局最后一条的 path 回到 `host+"/pkg/agentkit/model"`；`extractedSubtree` 保持 `<module>/agentkit`。同时补了一段注释说明"两套布局常量必须不同值"的教训。判定函数 `guardInsideFramework` / `guardUnderModulePrefix` **一行未改**，用例条数不变（23 条子用例 = 17 + 6）。改完用 `md5sum` 刷新 `GATE-PINS.md5` 中 `guard_test.go` 的哈希（其余 81 行未动）。

偏离原计划的地方：原估计是"1 行常量"，实际核对 `git log -p` 后确认是 8 处字面量（`6bbec97` 的 `/pkg/agentkit` → `/agentkit` 批量替换），已在 `CONTEXT.md` §8.1 与本 change 的 AC-5 中更正为事实。

## 改动文件

| 文件 | 性质 | 说明 |
|---|---|---|
| `guard_test.go` | 修改 | 8 处夹具字面量恢复 + 注释补教训；判定函数无改动（+10 / −8 行） |
| `GATE-PINS.md5` | 修改 | `guard_test.go` 哈希同步：`1d88316…` → `f0b14db…`（1 行） |

## verify 输出（必填）

```text
$ go test ./...
ok   github.com/malizhi2202-dev/agentkit        0.578s
ok   github.com/malizhi2202-dev/agentkit/agent  0.103s
ok   github.com/malizhi2202-dev/agentkit/audit  0.009s
ok   github.com/malizhi2202-dev/agentkit/authz  0.028s
ok   github.com/malizhi2202-dev/agentkit/engine 0.020s
ok   github.com/malizhi2202-dev/agentkit/im     0.020s
ok   github.com/malizhi2202-dev/agentkit/kit    0.015s
ok   github.com/malizhi2202-dev/agentkit/kitctx 0.015s
ok   github.com/malizhi2202-dev/agentkit/model  0.119s
ok   github.com/malizhi2202-dev/agentkit/modelcfg 3.037s
ok   github.com/malizhi2202-dev/agentkit/playbook 0.014s
ok   github.com/malizhi2202-dev/agentkit/preset 0.018s
ok   github.com/malizhi2202-dev/agentkit/scene  0.009s
ok   github.com/malizhi2202-dev/agentkit/security 0.042s
ok   github.com/malizhi2202-dev/agentkit/semantic 0.013s
ok   github.com/malizhi2202-dev/agentkit/session 0.017s
ok   github.com/malizhi2202-dev/agentkit/tools  0.713s
ok   github.com/malizhi2202-dev/agentkit/web    0.035s
exit=0

$ python3 tools/agentkit_code_pins.py
agentkit .go：磁盘 82 / 已钉 82
缺口=0

$ test "$(md5sum guard_test.go | awk '{print $1}')" = "$(grep ' guard_test.go$' GATE-PINS.md5 | awk '{print $1}')" && echo AC-6 ok
AC-6 ok

$ go test -count=1 -v -run 'TestGuard_AgentkitImportClassification' . | grep -c '^=== RUN'
24          # 1 父用例 + 23 子用例（仓库布局 17 + 抽取布局 6）
```

修复前后的对照（同一条命令）：

```text
修复前：--- FAIL: TestGuard_AgentkitImportClassification/...#02
        guardInsideFramework("example.com/hostmod/agentkit","example.com/hostmod/agentkit/model") = true，want false
修复后：--- PASS: TestGuard_AgentkitImportClassification (0.00s)  ← 23/23 子用例 PASS
```

## 阶梯判定（R19）

```
- 停点：rung 3（标准库能干 / 最小实现）—— 不需要新抽象、不需要新依赖，
  只是把既有夹具的字面量恢复成它与判定式约定过的形状；没有为"以后可能换布局"加参数化。
- 未做：没有把 8 处字面量抽成常量或表驱动（rung 2 已由 repoSubtree/extractedSubtree 覆盖，
  再抽一层是把夹具写成框架，属 R19.2 禁止项）。
```

## 数据库迁移

无 —— 本次不涉及任何 schema 或持久化。

## 破坏性变更

无。仅 `_test.go` 夹具与钉表哈希；不改任何导出符号、公共 API、运行期行为。

## 已知小问题

- 修复靠历史推断（`git log -p` 还原 `6bbec97` 之前的原意），未与宿主仓库当前版本逐字对照；若宿主侧后续再跑同一次 sed，仍会复发。已在注释里写明"常量必须不同值"，并建议（未做）加一条自检：`repoSubtree != extractedSubtree`。
- 无 CI：这次全绿仍需人工跑命令确认（议题 `ci-minimum-pipeline` 已留档）。

## 6 维自查（R6）

| 维度 | 结论 |
|---|---|
| 正确性 | 23/23 子用例 PASS；相似前缀三条仍判违规（牙齿未被磨掉） |
| 可读性 | 注释补了塌缩原理与提交号；reason 文案回到与路径一致 |
| 一致性 | 夹具与 `guard.test` 之外的口径一致（README/architecture 描述的是仓库布局 `pkg/agentkit`） |
| 安全性 | 未放宽任何判据；未改判定式与期望值 |
| 可测性 | 复用既有断言，未新增测试面 |
| 边界 | 不涉及 nil/并发/时区；纯字面量 |
