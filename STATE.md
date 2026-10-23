# STATE — agentkit 跨会话项目状态

> 仓库根 `STATE.md`。AI 每次开始新会话先读 `.specs/CONTEXT.md`（项目图谱）+ 本文件。

---

## 当前位置

- **活跃 Change**：`agentkit-framework`（产品基线验收）
- **当前阶段**：`5-test` 完成（测试门 4/4 通过）→ 下一阶段 `6-review`
- **当前 Task**：T01 ✅ / T02 ✅ / T03 ✅（`TASK.md` 均 `status="done"`，三份 SUMMARY 已落盘）
- **中断任务**：无

### 本次会话产物

| 工件 | 路径 | 状态 |
|---|---|---|
| 项目图谱（入场扫描） | `.specs/CONTEXT.md` | 已落盘 · 2026-10-23 · §8.1-1 已更正为已修复 |
| 产品文档 + 界面原型 | `.specs/agentkit-framework/PRODUCT-DESIGN.html` | **已过审**（用户原话「好的同意了」，2026-10-23） |
| 变更提案 | `.specs/agentkit-framework/CHANGE.md` | active |
| 需求与验收准则 | `.specs/agentkit-framework/REQUIREMENT.md` | 已落盘 · 需求门 4/4 通过 |
| 任务清单 | `.specs/agentkit-framework/TASK.md` | 已落盘 · Task 门 4/4 通过 · T01/T02 已执行 |
| 钉表 | `GATE-PINS.md5` | 已刷新 `guard_test.go` 哈希 |
| 开发小结 | `.specs/agentkit-framework/T01-SUMMARY.md` · `T02-SUMMARY.md` · `T03-SUMMARY.md` | 已落盘（含真实 verify 输出） |
| 测试报告 | `.specs/agentkit-framework/TEST.md` | 已落盘 · 5 轮声明齐 · 测试门 4/4 通过 |

### 会话开始建议

- 想继续推 → 跑 `5-test`（从 AC-1~AC-8 派生用例），再 `6-review`
- 想改代码 → 先读 `@.specs/agentkit-framework/REQUIREMENT.md`，新需求开新 change（`R3.2`）
- 想知道项目全貌 → `.specs/CONTEXT.md`（§3 项目图谱、§8 禁动清单与已知问题）

## 阻塞与待决策

| 项 | 类型 | 详情 | 待谁 | 自 |
|---|---|---|---|---|
| 本次改动未提交 | 把关 | 已跟踪改动 2 个文件（`guard_test.go`、`GATE-PINS.md5`），新增未跟踪 `.specs/`（含 `PRODUCT-DESIGN.html` 与 6 个 change 工件）与 `STATE.md`；等人工过 `git diff`/`git add` 后再提交（`R17.4` ④） | 用户 | 2026-10-23 |

### 已留档议题（R18.4 · 交 D-discovery 消化）

| 议题（=change-id） | 一句话 | 来源 |
|---|---|---|
| `ci-minimum-pipeline` | 最小 CI：`go test -race ./...` + 两个 python 门禁脚本 | `CHANGE.md` 范围排除、`CONTEXT.md` §8.1-3 |
| `doc-path-realign` | 文档中 `.specs/adr/…`、`pkg/agentkit/…` 悬空指针统一为仓库相对路径 | `CONTEXT.md` §8.1-2 |
| `e2e-perf-baseline` | 端到端 P95 / QPS 回环压测（当前只有单点基准） | `docs/architecture.md:99`、`CONTEXT.md` §8.2 |
| `coverage-web-preset-audit` | `web` 85.4% / `preset` 88.1% / `audit` 88.4% 低于 core 90% 门槛 | `TEST.md` §1.3、§1.6 |
| `test-name-scenario` | 守卫子用例名由路径字面量改为场景名（T1 🟢） | `TEST.md` §1.5 |
| `govulncheck-baseline` | 离线环境缺 `govulncheck` / `gosec`，依赖 CVE 结论空缺 | `TEST.md` §3.1、§3.3 |

## 决策日志（最近 10 条，倒序）

- `[2026-10-23]` 守卫两套布局子树常量必须不同值；夹具字面量禁止批量替换（`6bbec97` 教训） — `@.specs/CONTEXT.md` 已锁决策
- `[2026-10-23]` PRODUCT-DESIGN 过审：状态「起草中」→「已过审」，用户原话背书 — `@.specs/agentkit-framework/PRODUCT-DESIGN.html#0`
- `[2026-10-23]` 根守卫夹具红定性为**测试夹具**问题（8 处字面量），判定函数一行不改 — `@.specs/agentkit-framework/REQUIREMENT.md#AC-5`
- `[2026-10-23]` 本 change 跳 2-design：无新架构决策 — `@.specs/agentkit-framework/CHANGE.md`
- `[2026-10-23]` 入场扫描采用「既有 README + docs 三篇合并 + 代码交叉验证」口径 — `@.specs/CONTEXT.md#0`
- `[2026-10-23]` 5-test 第 4 轮量测发现 360px 溢出 377>360，根因唯一（长路径 code），补一条 CSS 修复（T03）并在四视口复测 — `@.specs/agentkit-framework/TEST.md#4.1`

## 已归档 Changes

| 日期 | Change | 摘要 | PR |
|---|---|---|---|
| — | — | 尚无归档 change | — |

---

## 横向命令状态

```yaml
ai_context_doc: .specs/CONTEXT.md
last_intel_scan: 2026-10-23

last_architect_at:
last_evolve_at:
last_evolve_promoted: []

last_health_at:
last_health_score:
```

---

## 健康检查（AI 每次会话开始前自检）

- [x] `.specs/` 目录存在
- [x] `.specs/CONTEXT.md` 已生成（含项目图谱与证据行号）
- [x] 当前活跃 change 的工件齐全（CHANGE / REQUIREMENT / TASK）
- [x] 没有未归档但已合并的 change
- [x] `go test ./...` 18/18 包全绿（2026-10-23 实测，exit 0）
- [x] `python3 tools/agentkit_code_pins.py` 缺口 0，`guard_test.go` 哈希已刷新
- [x] `5-test` 五轮声明齐全，四视口实测无横向溢出（360/768/1024/1440）
