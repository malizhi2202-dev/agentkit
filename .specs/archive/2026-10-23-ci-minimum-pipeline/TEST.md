# TEST — ci-minimum-pipeline

## 1 · 测试矩阵

| AC | 类型 | 验证方式 | 结果 |
|---|---|---|---|
| AC-1 | 静态断言 | `grep -c` 命中 ci.yml 的 3 个执行步骤 | ✅ 4 处命中 |
| AC-2 | 负例 | 构造 `web` 0% 的 coverprofile → 门槛脚本必须拒绝 | ✅ exit=1，`LOW web 0.0% (0/10)` |
| AC-3 | 正例 | 真实覆盖率 → 门槛脚本必须通过 | ✅ exit=0，全部包 ≥90%（web 91.5 / preset 90.6 / audit 91.6） |
| AC-4 | 正负例 | 文档断言：干净 → 0；注入「待审」→ 1 | ✅ 0 / exit=1（1 处命中） |
| AC-5 | 静态断言 | nightly 含 govulncheck | ✅ 4 处命中 |
| AC-6 | 端到端 | `bash tools/ci_local.sh` 一条命令复跑 | ✅ exit=0，末行「全部门禁通过」 |
| AC-7 | 依赖面 | `git diff origin/main -- go.mod go.sum` | ✅ 0 行改动 |
| AC-8 | 回归 | `go test ./...` + 钉表 | ✅ 18 包全绿 · 缺口=0 · 589 个测试 |

## 2 · 新增测试（覆盖率补齐）

| 文件 | 覆盖目标 | 断言的行为 |
|---|---|---|
| `audit/recall_test.go` | `Recalling.Len/Reset/Entries`、`Nop`、`SinkFunc`、`RequireMasked` | 回看计数正确；Nop 恒成功；未盖章条目被拒；**Close 透传到内层 sink**（否则缓冲 sink 永不 flush） |
| `web/extra_test.go` | `ErrKind.Error`、`(*Error).Unwrap`、`WriteJSON` 双入口、`Routes/routeFor`、`ServeHTTP` 404、`Serve`/`ListenAndServe` | 响应信封 `{code,data,msg}`；静态路由契约；优雅停止返回 `ErrServerClosed` |
| `web/scalar_test.go` | `scalarString` 全分支、`formatNum`、`recorder` 写头语义、`splitKey` | 整值浮点不带 `.0`；不支持类型必须报错（防静默入参）；重复写头被忽略 |
| `preset/describe_test.go` | `Audit.Describe`、`Server.Describe` | **nil 接收者不 panic**，且 `sinks`/`addr` 字段准确 |

## 3 · 测试期间发现并修复的缺陷

| # | 缺陷 | 影响 | 修复 |
|---|---|---|---|
| D1 | `preset.Audit.Describe()` / `Server.Describe()` 对 `a.ready` / `sv.ready` 裸取，nil 接收者 panic（`Name()` 和 `fan` 却做了守卫，守卫半途而废） | 管理面在插件未装配时调用即崩 | 两处改为先判空再取字段 |
| D2 | `agentkit_code_pins.py` 报错文案让人 `--freeze`，但**脚本不认这个参数** | 补钉只能手写 md5，钉表可靠读书写 | 实现 `--freeze` |
| D3 | 钉表**只比路径不比哈希** | 改任何 `.go` 都不会红 ⇒ "唯一事实源"失效。实测已有 **14 个文件**与钉定哈希不一致（`preset/preset.go` 为本次改动，其余 13 个为历史漂移） | 加哈希校验并计入缺口；`--freeze` 重建基线 |

> D3 是本轮最重要的发现：门禁三件套里的"钉表"此前**装饰性大于判据性**。

## 4 · 覆盖率变化

| 包 | 之前 | 之后 |
|---|---|---|
| audit | 88.4% | **91.6%** |
| preset | 88.1% | **90.6%** |
| web | 85.4% | **91.5%** |
| 全仓 | 92.1% | 提升（新增 4 个测试文件） |
