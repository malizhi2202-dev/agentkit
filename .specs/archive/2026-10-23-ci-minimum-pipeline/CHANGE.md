# CHANGE — ci-minimum-pipeline

- **change-id**：`ci-minimum-pipeline`（沿用既有议题登记名，见 `STATE.md` 留档议题表）
- **类型**：工程化基础设施（非架构级：不改模块结构、不改 ADR）
- **创建**：2026-10-23 · 来源：`ROADMAP.md` §8 `C1-C4`（W0 波次）

## 1 · 为什么要做

框架本身是健康的（18 包全绿、341 测试、92.1% 语句覆盖），但**门禁全靠人肉触发**：

- 无 `.github/workflows`、无 `.gitlab-ci.yml`（`CONTEXT.md` §8.1-3）
- 3 个包低于自设 core 90% 门槛：`web` 85.4 / `preset` 88.1 / `audit` 88.4
- `govulncheck` / `gosec` 未装，依赖 CVE 结论空缺
- 文档断言（状态标、悬空指针）靠眼睛

**后果**：所有"全绿"结论只在本地那一刻成立；后续平台化（W5-W8）一旦开始，回归无人兜底。

## 2 · 要做什么（范围）

| # | 内容 | 对应 ROADMAP |
|---|---|---|
| 1 | 最小 CI：`go test -race ./...` + 钉表脚本 + portability 脚本 | C1 |
| 2 | 覆盖率门槛脚本 + CI job（core 90%，并补齐 3 个缺口包） | C2 |
| 3 | 文档断言脚本（状态标一致、无「待审」漏改、无宿主布局悬空指针） | C3 |
| 4 | `govulncheck` nightly job | C4 |

## 3 · 范围排除（明确不做）

- 不做发布流水 / tag / release notes（无版本节奏，YAGNI）
- 不做 `gosec`（先解决 CVE 扫描，静态安全规则另开）
- 不做 CHANGELOG 自动化
- 不引入第三方 CI 服务；只用 GitHub Actions（仓库已在 GitHub）

## 4 · 验收口径（高层）

1. 推送任意提交后，CI 自动跑 **测试 + 钉表 + portability** 并阻断失败
2. 覆盖率低于 90% 的 core 包会让 CI 变红（且当前 3 个缺口包已补齐，红是真实回归信号）
3. 文档状态标与 `TOPICS.md`/`ROADMAP.md` 不一致时 CI 变红
4. nightly 产出 `govulncheck` 结果，有漏洞即失败

## 5 · 架构层影响声明

- 不改模块结构、不新增核心依赖（CI 脚本不属 `go.mod` 面）→ **不冲击任何 ADR**
- 与 ADR-001 一致：CI 工具链（`go test` / python 脚本）不进入运行时依赖
- 与 ADR-004 一致：CI 是侧车与平台层落地前的护栏，不改变形态

## 6 · 风险

| 风险 | 缓解 |
|---|---|
| CI 首次全红（缺 test 环境/网络） | 先本地跑通三条命令，再写 workflow；`portability_check` 已实测通过 |
| 覆盖率门槛一上线就红 | 先补 `web`/`preset`/`audit` 到 90%，再启用门槛 |
| Actions 无 Go 1.23 缓存 | 用 `actions/setup-go` + `go-version-file: go.mod` |

## 7 · 执行中追加的范围外修复（已记录理由）

| # | 修复 | 为什么算在内 |
|---|---|---|
| D1 | `preset` 两个 `Describe()` 的 nil 越界 | 补覆盖率测试时直接暴露；2 行修复，不修则测试无法断言"nil 安全" |
| D2 | `agentkit_code_pins.py` 实现缺失的 `--freeze` | CI 门禁要跑钉表，而钉表的维护入口不存在 |
| D3 | 钉表加入哈希校验 | 否则 CI 里的"钉表"挡不住任何改动，等于假门禁 |
