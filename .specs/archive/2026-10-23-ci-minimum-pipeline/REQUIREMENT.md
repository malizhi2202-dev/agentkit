# REQUIREMENT — ci-minimum-pipeline

- **change-id**：`ci-minimum-pipeline` · 上游：`CHANGE.md`
- **需求门**：待跑（见 §6）

## 1 · 用户故事

- **US-1**：作为**接入方工程师**，我想在每次提交后自动看到"测试/钉表/可搬运性"三项结论，以便在合并前就知道自己有没有把框架弄坏。
- **US-2**：作为**框架维护者**，我想让覆盖率与文档一致性变成会失败的检查，以便"全绿"是机器结论而不是我的记忆。
- **US-3**：作为**安全负责人**，我想每晚自动扫依赖漏洞，以便不用等出事才发现 `x/sync` 之类的上游问题。

## 2 · 验收准则（v1）

| # | AC（Given/When/Then） | 验证命令 |
|---|---|---|
| AC-1 | Given 仓库根有 `.github/workflows/ci.yml`，When 读取该文件，Then 它包含 `go test -race`、钉表脚本、portability 脚本三个执行步骤 | `grep -c 'go test -race\|agentkit_code_pins.py\|portability_check.py' .github/workflows/ci.yml` ≥ 3 |
| AC-2 | Given 某 core 包覆盖率低于 90%，When 跑覆盖率门槛脚本，Then 退出码非 0 并打印该包 | `python3 tools/coverage_gate.py --root /tmp/cover.out; echo $?`（构造低覆盖输入） |
| AC-3 | Given 当前代码，When 跑覆盖率门槛脚本，Then 退出码 0，且 `web`/`preset`/`audit` 均 ≥ 90% | `go test -coverprofile=/tmp/cover.out ./... && python3 tools/coverage_gate.py /tmp/cover.out; echo $?` |
| AC-4 | Given 文档中存在未更新的「待审」或状态表与文件不一致，When 跑文档断言脚本，Then 退出码非 0 | `python3 tools/doc_assert.py; echo $?`（当前应为 0） |
| AC-5 | Given 仓库根有 nightly workflow，When 读取它，Then 含 `govulncheck` 步骤 | `grep -c govulncheck .github/workflows/nightly.yml` ≥ 1 |
| AC-6 | Given 本地干净环境，When 执行 `bash tools/ci_local.sh`，Then 一条命令复跑全部门禁且退出码 0 | `bash tools/ci_local.sh; echo $?` |
| AC-7 | Given 本次改动，When 检查依赖，Then `go.mod`/`go.sum` 无新增第三方依赖 | `git diff origin/main -- go.mod go.sum \| wc -l` = 0 |
| AC-8 | Given 本次改动，When 跑全量测试与钉表，Then 18 包全绿且钉表缺口 0 | `go test ./... && python3 tools/agentkit_code_pins.py \| tail -1` |

## 3 · 范围切分

- **v1（本次必做）**：AC-1 ~ AC-8
- **v2（下次再说）**：`gosec` 静态安全规则、benchmark 回归门槛（benchstat）、flaky 检测、Go 版本矩阵
- **out（永远不做）**：发布流水 / release notes 自动化、CHANGELOG 自动生成、引入第三方 CI 服务

## 4 · 非功能性需求

| 维度 | 要求 |
|---|---|
| 兼容性 | CI 使用 `go-version-file: go.mod`（1.23.1），不硬编码版本 |
| 安全 | `govulncheck` 结果失败即红；不引入 `GITHUB_TOKEN` 之外的凭据 |
| 可观测性 | 每个 gate 脚本打印"检查了什么、结论是什么"，不可只返回码 |
| 可移植性 | 所有脚本可在本地直接跑（无 Actions 依赖），CI 只是调用它们 |
| 运行时依赖 | **零新增**（脚本用 stdlib python3 + go 工具链） |

## 5 · 域语言（已写入 CONTEXT.md）

- **门禁三件套**：`go test -race` + 钉表（`agentkit_code_pins.py`）+ 可搬运性（`portability_check.py`）
- **覆盖率门槛**：core 包（除 `modelcfg` 外所有包）语句覆盖率 ≥ 90%，低于即红
- **文档断言**：状态标与状态表一致、`ROADMAP` 不含未过审结论、`ARCHITECTURE` ADR 段完整

## 6 · 需求门（4 领域专家 · R9/R13）

| 专家 | 票 | 理由 |
|---|---|---|
| 🟫 高级产品经理 | 同意 | 覆盖 CHANGE 全部 4 项范围；v1/v2/out 切分清晰 |
| 🟦 资深用户评测员 | 同意 | AC 全部可用命令验证，无"要好看"类主观项 |
| 🟩 架构师 | 同意 | 不引入运行时依赖；与 ADR-001/ADR-004 一致；CI 脚本可本地复跑 |
| 🔴 安全审计师 | 有保留 | `govulncheck` 需要漏洞库网络访问，离线环境会失败 —— 已限定为 nightly 且允许在无网时跳过并显式警告 |

**裁决**：3 同意 + 1 有保留（非反对）→ 通过。4/4 参与，无反对。
