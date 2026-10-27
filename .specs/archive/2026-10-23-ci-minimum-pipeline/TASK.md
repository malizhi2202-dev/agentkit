# TASK — ci-minimum-pipeline

- **波次**：单波次（任务间有弱依赖，按序执行）
- **Task 门**：见 §3

## 1 · 任务清单

| ID | 任务 | 并行 | auto | 产物 | 验证 |
|---|---|---|---|---|---|
| T01 | 覆盖率门槛脚本 `tools/coverage_gate.py` | 是 | true | 新脚本 | AC-2/AC-3 |
| T02 | 补齐 `web`/`preset`/`audit` 三包到 ≥90% | 是 | true | 新增测试 | AC-3 |
| T03 | 文档断言脚本 `tools/doc_assert.py` | 是 | true | 新脚本 | AC-4 |
| T04 | CI 主流水线 `.github/workflows/ci.yml` | 否 | true | workflow | AC-1 |
| T05 | nightly `govulncheck` + 本地一键 `tools/ci_local.sh` | 否 | true | workflow + 脚本 | AC-5/AC-6 |

## 2 · 执行顺序

```
T01 ─┐
T02 ─┼─→ T04 ─→ T05
T03 ─┘
```

T01/T02/T03 无相互依赖可并行；T04 依赖三者脚本就位；T05 最后（复用 T04 的 job 名）。

## 3 · Task 门（4 领域专家 · R9/R13）

| 专家 | 票 | 理由 |
|---|---|---|
| 🟫 高级产品经理 | 同意 | 5 个任务一一对应 4 项范围 + 覆盖率补齐，无范围蔓延 |
| 🟦 资深用户评测员 | 同意 | 每任务都有可执行验证，无"写个 CI"这种模糊交付 |
| 🟩 架构师 | 同意 | 依赖顺序正确；脚本与 workflow 分离，保证本地可复跑 |
| 🔴 安全审计师 | 同意 | nightly 隔离漏洞扫描，不影响主流水线时延 |

**裁决**：4/4 同意 → Task 门通过。
