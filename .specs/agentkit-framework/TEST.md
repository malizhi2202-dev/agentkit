# TEST: agentkit 产品基线验收

- **Change ID**: `agentkit-framework`
- **关联**: `@.specs/agentkit-framework/REQUIREMENT.md`、`@code-kit/reference/test-pyramid.md`
- **项目类型**: Go 库（无 UI、无服务端进程、无运行时进程）
- **执行环境**: go1.26.2 linux/amd64（`go.mod:3` 声明 `go 1.23.1`）、HeadlessChrome 151.0.7922.34

---

## 0. 本次测试范围声明（5 轮金字塔）

| 轮次 | 状态 | 范围 | 跳过理由 |
|---|---|---|---|
| 第 1 轮 · 功能 | ✅ 必跑 | AC-1 ~ AC-8 全部 8 条 | — |
| 第 2 轮 · 性能 | ⚠️ 部分 | 仅跑既有基准做"无退化"旁证 | `REQUIREMENT.md` 非功能性需求显式写「性能：无 —— 本次不触碰任何运行路径」，无预算可对照，故不做 k6/Lighthouse 类预算判定 |
| 第 3 轮 · 安全 | ⚠️ 部分 | 依赖清单 + 秘钥扫描 + `go vet` + OWASP 逐项映射 | `govulncheck` / `gosec` 未安装（离线环境），已如实记录为缺口，不伪称"扫过" |
| 第 4 轮 · 兼容 | ✅ 必跑 | 文档四视口实测（360/768/1024/1440）+ Go 工具链声明核对 | 跨浏览器矩阵不适用（交付物是本地单文件 HTML，无 JS 框架兼容面） |
| 第 5 轮 · 可观测 | ⚠️ 部分 | 审计证据链 / 脱敏 / trace 三项映射到既有测试 | 库无进程、无 `/health`、无 metrics 端点，告警与链路追踪不适用 |

---

## 第 1 轮 · 功能测试

### 1.1 测试矩阵（AC → 用例）

| AC | 类型 | 用例（文件 · 名称 / 命令） | 状态 |
|---|---|---|---|
| AC-1 文档断网可开、九段齐全 | unit（脚本断言） | `PRODUCT-DESIGN.html` + python 断言（见 §1.3） | ✅ |
| AC-2 方向四档等号基线 | unit | `guard_test.go` · `TestGuard_NoDirectLLMCall`、`TestGuard_HTTPDirectionClassification` | ✅ |
| AC-3 空角色 fail-closed | unit | `tools/registry_test.go` · `TestRegistry_RoleUndeterminedFailClosed`；旁证 `scene/scene_test.go:146`（空角色无可用工具）、`playbook/playbook_test.go:166`（补偿步骤同谓词） | ✅ |
| AC-4 五句起服务端到端 | e2e（进程内） | `preset/e2e_test.go` · `TestPreset_EndToEnd_*`（5 条：装配运行 / 换角色裁工具 / 审计脱敏 / HTTP 起停 / 模型回调） | ✅ |
| AC-5 根包全绿 | unit | `guard_test.go` · `TestGuard_AgentkitImportClassification`（23 条子用例） | ✅ |
| AC-6 钉表双向相等 | 脚本 | `python3 tools/agentkit_code_pins.py` + `md5sum` 对照 | ✅ |
| AC-7 过审状态与背书 | 脚本 | `grep` 断言（已过审 / 背书原话 / 无"起草中"） | ✅ |
| AC-8 四件套互链 | 脚本 | `grep` 断言（STATE / CONTEXT / HTML 三处口径） | ✅ |

**强制项核对**：8/8 条 AC 均有覆盖，无空缺。

### 1.2 UAT 脚本

#### UAT-1 · 产品文档人审门（唯一需要人判断的验收）

- **前置**: `PRODUCT-DESIGN.html` 状态为「起草中」
- **步骤**: 1. 打开文档 → 2. 逐章阅读 §1~§9 → 3. 重点核对 §1.4 边界、§6 原型与 §7 权限矩阵是否一致
- **期望**: 无口径冲突、无未完成段落（无 `TODO` / `待补`）、原型与矩阵自洽
- **实际**: **通过** —— 用户 2026-10-23 以原话「好的同意了」背书，状态晋级「已过审」
- **执行人 / 时间**: 用户 / 2026-10-23

### 1.3 覆盖率

```text
$ go test -count=1 -cover ./...
ok  .                0.641s  coverage: [no statements]     ← 根包只有守卫测试，无生产语句
ok  ./agent          0.099s  coverage: 91.8% of statements
ok  ./audit          0.008s  coverage: 88.4% of statements
ok  ./authz          0.013s  coverage: 97.4% of statements
ok  ./engine         0.019s  coverage: 90.9% of statements
ok  ./im             0.019s  coverage: 90.1% of statements
ok  ./kit            0.015s  coverage: 91.9% of statements
ok  ./kitctx         0.086s  coverage: 98.2% of statements
ok  ./model          0.143s  coverage: 93.4% of statements
ok  ./modelcfg       3.043s  coverage: 97.0% of statements
ok  ./playbook       0.022s  coverage: 92.4% of statements
ok  ./preset         0.031s  coverage: 88.1% of statements
ok  ./scene          0.022s  coverage: 99.0% of statements
ok  ./security       0.053s  coverage: 94.7% of statements
ok  ./semantic       0.029s  coverage: 97.0% of statements
ok  ./session        0.021s  coverage: 94.7% of statements
ok  ./tools          0.718s  coverage: 94.9% of statements
ok  ./web            0.036s  coverage: 85.4% of statements
```

- **当前**：18 个包；13 个 ≥ 90%（核心治理包 `authz` 97.4 / `security` 94.7 / `tools` 94.9 / `scene` 99.0）
- **门槛**：默认 80%，core ≥ 90%
- **不达项**：`web` 85.4%、`preset` 88.1%、`audit` 88.4% 低于 core 90% 门槛（均 ≥ 80% 默认门槛）。**原因**：三者都含"接线 / 失败分支"型代码（HTTP 错误路径、装配回滚分支、多 sink 隔离），本次 change 未触碰其实现，属存量分布问题。已登记「测试质量记事」。
- **注意**：本次改动全部在 `_test.go` 与文档，`go test -cover` 不计测试文件，故覆盖率不因本次改动变化 —— 用"改动前后覆盖率相同"作为无退化证据。

### 1.4 边界 / 错误路径用例

| 类别 | 用例 | 结果 |
|---|---|---|
| 空 / 零值 | `TestRegistry_RoleUndeterminedFailClosed`（空角色）、`TestBlacklist_NilAndScalar`、`TestFence_NilFenceFailsClosed`、`TestIsEmptyPayload_ZeroValueIsNotContainerEmptiness` | ✅ |
| 极大 | `BenchmarkLookupAuthorized_300Tools_v3`（300 工具）+ `TestRegistry_ConcurrentTrimAndLookup`（并发） | ✅ |
| 极小 / 单元素 | `BenchmarkAssemble_10Items`、`TestGuard_AgentkitImportClassification` 单段路径用例 | ✅ |
| Unicode / 多语言 | 产品文档中文 + 全角标点整文件断言（AC-1）；`im` 卡片分段测试 | ✅ |
| 错误路径（显式） | `TestAudit_MaskerFailureBlocksEverySink`、`TestBlacklist_UnverifiableValueFailsClosed`、`TestEgressPolicy_DefaultDenyForUndeclaredSign`、`TestPreset_UnknownPermissionFailsClosed` | ✅ |

### 1.5 测试质量自检（6 维测试衰退风险）

| 编号 | 风险 | 命中文件数 | 严重度分布 |
|---|---|---|---|
| T1 | Test Obscurity 测试晦涩 | 1 | 🔴 0 / 🟡 0 / 🟢 1 |
| T2 | Test Brittleness 测试脆弱 | 0 | 🔴 0 / 🟡 0 / 🟢 0 |
| T3 | Test Duplication 测试重复 | 0 | 🔴 0 / 🟡 0 / 🟢 0 |
| T4 | Mock Abuse Mock 滥用 | 0 | 🔴 0 / 🟡 0 / 🟢 0 |
| T5 | Coverage Illusion 覆盖率幻觉 | 0 | 🔴 0 / 🟡 0 / 🟢 0 |
| T6 | Architecture Mismatch 架构错配 | 0 | 🔴 0 / 🟡 0 / 🟢 0 |

**详细发现**：

```markdown
### 🟢 T1 · Test Obscurity：子用例名是路径字面量，不是场景名
**Symptom**：`guard_test.go:927` 用 `t.Run(tc.subtree+"|"+tc.path, …)` 命名，
子用例名形如 `example.com/hostmod/pkg/agentkit|example.com/hostmod/pkg/agentkitmodel`。
**Source**：xUnit Test Patterns · Test Naming / Test Obscurity
**Consequence**：只看 `-v` 输出无法一眼判断"这条在验证什么"；但 `reason` 字段与失败信息里
都带中文场景说明（`相似前缀：agentkitmodel ≠ agentkit`），实际排障成本可控。
**Remedy**：若后续用例继续增加，改为 `t.Run(n+"·"+tc.reason, …)`。本次 23 条稳定、不改。
```

**处理**：命中 1 项（🟢）→ 记入下文「测试质量记事」，按议题暂缓（`R18.4`）。

### 1.6 测试质量记事（议题 · 暂缓项）

| 文件 / 项 | 维度 | 严重度 | 计划 |
|---|---|---|---|
| `guard_test.go:927` 子用例命名 | T1 | 🟢 | 议题 `test-name-scenario`：子用例改场景名（本次不动，避免 23 条名字与历史日志对不上） |
| `web` 85.4% / `preset` 88.1% / `audit` 88.4% 覆盖率 | 覆盖率 | 🟡 | 议题 `coverage-web-preset-audit`：补接线失败分支用例，目标 core ≥90% |

---

## 第 2 轮 · 性能测试

### 2.1 性能预算（来自 REQUIREMENT.md 非功能性需求）

```yaml
performance: 无   # "无 —— 本次不触碰任何运行路径（只改 _test.go 夹具与文档）"
```

无预算可对照，**不做** Lighthouse / k6 / 压测判定（避免"看起来没问题"式空话）。

### 2.2 实测结果（仅作无退化旁证）

| 指标 | 值 | 说明 |
|---|---|---|
| `BenchmarkAssemble_10Items` | 203,446 ns/op · 20,744 B/op · 141 allocs/op | 装配 10 项 |
| `BenchmarkAssemble_100Items` | 1,443,250 ns/op · 186,978 B/op · 1,052 allocs/op | 装配 100 项 |
| `BenchmarkAssemble_1000Items` | 14,400,708 ns/op · 1,868,388 B/op · 10,079 allocs/op | 装配 1000 项，线性 |
| `BenchmarkLookupAuthorized_30Tools_v3` | 2,657 ns/op · 0 allocs | 授权判定（30 工具） |
| `BenchmarkLookupAuthorized_300Tools_v3` | 27,223 ns/op · 0 allocs | 授权判定（300 工具），零分配 |
| `BenchmarkLookupAuthorized_300Tools_EmptyRole` | 25,410 ns/op · 0 allocs | 空角色路径不比正常路径慢（不靠"提前放宽"换速度） |

**判定**：⚠️ 不适用预算（无预算）；作为"无退化"证据，本次未改任何非测试代码，基准仍可运行且数值与 `docs/architecture.md:99` 记录的形态一致（线性装配、判定零分配）。

---

## 第 3 轮 · 安全测试

### 3.1 依赖漏洞

```bash
$ go list -m all
github.com/malizhi2202-dev/agentkit
golang.org/x/sync v0.8.0
```

- 直接依赖 **1 个**（`errgroup`），由 `TestGuard_AgentkitNoThirdPartyDeps` 常驻钉死白名单
- High / Critical：**未知** —— `govulncheck` 未安装（离线），未伪称已扫
- 处理：登记缺口于「测试质量记事」；`errgroup v0.8.0` 为官方子模块，风险面极小

### 3.2 秘钥扫描

```bash
$ grep -rnE '(sk-[A-Za-z0-9]{16,}|AKIA[0-9A-Z]{16}|BEGIN (RSA|OPENSSH) PRIVATE KEY|ghp_[A-Za-z0-9]{20,})' \
    --include='*.go' --include='*.md' --include='*.html' .
命中行数: 0
```

- 命中：**0**
- 已 rotate：N/A

### 3.3 SAST

- 工具：`go vet ./...`（标准工具链，离线可用）
- 结果：**exit 0，零告警**
- `gosec` 未安装 → 已登记，未伪称执行

### 3.4 OWASP Top 10

| 项 | 状态 | 证据 |
|---|---|---|
| A01 越权 | ✅ 已测 | `authz` 权限点声明/查找测试、`tools.TestRegistry_AdminToolsAbsentFromWireForNonAdmin`、`preset` 换角色裁工具 e2e |
| A02 加密失败 | ❌ 不适用 | 库不做传输/存储加密，出域由 `security.EgressPolicy` 管（见 A10） |
| A03 注入 | ✅ 已测 | `security.TestInject_InstructionIsData`（指令当数据处理）、`TestFence_CannotForgeClosingMarker`（围栏不可伪造）、`TestBlacklist_*` |
| A04 不安全设计 | ✅ 已测 | fail-closed 默认集：空角色不判定即拒（`TestRegistry_RoleUndeterminedFailClosed`）、未知权限整份声明作废（`TestPreset_UnknownPermissionFailsClosed`） |
| A05 配置错误 | ✅ 已测 | `preset` 声明二阶段校验、`ErrLateDeclare` 用例、装配回滚用例 |
| A06 漏洞组件 | ⚠️ 部分 | 依赖面 = `errgroup` 1 个；`govulncheck` 未装，无 CVE 结论 |
| A07 鉴权 | ✅ 已测 | `authz.TestVocab_ScopeClosedSet`、`TestVocab_EnforcementGrammar`、`TestVocab_EnforcementSelfConsistency` |
| A08 数据完整性 | ✅ 已测 | `audit.TestMaskedBeforeSink`（先脱敏再落），`TestAudit_SinkPayloadIsolation`（sink 拿不到原对象） |
| A09 日志监控 | ⚠️ 部分 | 见第 5 轮（库内证据：`audit.TestMaskerFailureBlocksEverySink`） |
| A10 SSRF | ⚠️ 部分 | 出域控管有默认拒绝（`TestEgressPolicy_DefaultDenyForUndeclaredSign`、`TestEgressPolicy_CoveredDomainsFrozenToFour`），但"URL 由模型给出时的 SSRF 语义"属接入方职责，框架只保证出域集合；如实记 ⚠️ |

---

## 第 4 轮 · 兼容性测试

### 4.1 文档四视口实测（CDP 量测，非目测）

用 Chrome DevTools Protocol 在四个宽度下真实量测 `document.documentElement.scrollWidth` 与 `clientWidth`，并负向筛选"真正溢出文档"的元素（排除被 `overflow-x:auto` 容器正常滚动的表格）：

| 视口 | scrollWidth | clientWidth | 横向溢出 | 目录滚动高亮 |
|---|---|---|---|---|
| 360（手机） | 360 | 360 | **无**（修复后） | ✅ 生效 |
| 768（平板） | 753 | 753 | 无 | ✅ |
| 1024（笔记本） | 1009 | 1009 | 无 | ✅ |
| 1440（桌面） | 1425 | 1425 | 无 | ✅ |

**首测发现**：360px 下 `scrollWidth=377 > 360`。根因定位（逐元素量测 + 祖先 `overflow-x` 过滤）为**唯一**一个不可断行的长路径 `<code>`（右边界 363），非表格、非原型面板。

**修复**：`T03` —— 仅加一条 `.doc-body code{overflow-wrap:anywhere}`（`pre code` 保持 `normal`），复测 360px → 360，四视口全部无溢出。

### 4.2 跨版本 / 跨编码

- [x] `go.mod` 声明 `go 1.23.1`，实测工具链 go1.26.2 可编译可测（**未**在 1.23 上实测，本机无该工具链 → 缺口已记）
- [x] 旧数据兼容：本次无 schema / 持久化，N/A
- [x] API 兼容：无公共 API 变更（只改 `_test.go` 与文档），N/A
- [x] UTF-8 / 中文 / 全角标点：文档全文件中文断言通过；Go 源码含中文注释与断言消息，`gofmt` 无异常

### 4.3 数据迁移

不涉及（无 schema、无持久化；`T01`/`T02` 的 SUMMARY「数据库迁移」段均为"无"）。

---

## 第 5 轮 · 可观测性验证

> 库无进程、无 metrics 端点、无 `/health`，故按"库内可观测面"映射，逐项给证据，不硬套 Web 清单。

### 5.1 审计与日志

- [x] 关键路径有留痕：`audit` 包在扇出前脱敏，sink 只能拿到已脱敏副本 — `TestAudit_RawEntryCannotReachSink`、`TestAudit_MaskedBeforeSink`
- [x] trace-id 贯穿：`kitctx` 显式携带，`agent` 步进带 trace — `TestTrace_ReplayByTraceID`、`agent/loop_test.go:343`（每步 `TraceID` 与请求一致）
- [x] 不含 PII / 秘钥：`security` 黑名单字段永不出域 — `TestBlacklist_RemovesNeverSendFields`、`TestEgressPolicy_UncoveredAllowlistsExcludeNeverSendFields`
- [x] 失败不静默：脱敏失败则所有 sink 都不落 — `TestAudit_MaskerFailureBlocksEverySink`（宁可不写，不写脏的）

### 5.2 指标

- [ ] 业务 metric：**不适用**（库不暴露 metrics；接入方自行打点）
- [ ] RED 指标：不适用（无 endpoint）
- 说明：框架把可观测性做成"审计证据 + trace 回放"，不是 Prometheus 指标面。

### 5.3 链路追踪

- [x] 请求内可串：`TraceID` + `Seq` 固化，`TestTrace_RecordStampsTraceIDAndSeq`
- [x] 慢操作可定位：`agent` 每步留痕含步骤名与工具名（`StepsLimited` 时 `Degraded` + 冻结 `Notice`）
- [ ] 跨服务 trace：不适用（库不含 RPC 进程）

### 5.4 告警 + 健康检查

- [ ] 告警：不适用（无进程）
- [ ] `/health`：不适用；`web` 包只提供 HTTP 挂载点，接入方自行提供探针

---

## 新增测试登记

| 用例 / 脚本 | 类型 | 覆盖 AC | 轮次 |
|---|---|---|---|
| `guard_test.go` · `TestGuard_AgentkitImportClassification`（23 子用例，恢复 8 处字面量） | unit | AC-5 | 1 |
| CDP 四视口量测（`/tmp/cdp2.py`，一次性，不入仓） | 兼容 | AC-1 | 4 |
| AC-1 九段/零外链/锚点断言脚本 | unit（脚本） | AC-1 | 1 |
| AC-7/AC-8 grep 断言 | unit（脚本） | AC-7、AC-8 | 1 |

> 说明：AC-1/6/7/8 的校验脚本目前内联在 `REQUIREMENT.md` 与本文档中，未固化成仓内可执行文件 —— 已登记议题 `ci-minimum-pipeline`（随 CI 一起落成脚本）。

## 回归保护

本次变更可能影响的旧功能：

| 受影响面 | 是否仍通过 |
|---|---|
| 根包全部守卫（导入分类、方向基线、第三方依赖、go.mod 契约、身份词表） | ✅ `go test ./...` 18/18 包 |
| 并发安全（`-race` 全量） | ✅ 18/18 包 race 通过 |
| 装配 / 授权 / 审计 / 出域 既有行为 | ✅ 未改任何非测试代码，e2e 全通过 |

---

## 🛡️ 测试门禁（R9 / R13 · 已执行）

🗳️ 测试门: TEST.md 是否通过？

- 🟫 **资深测试工程师**: ✅ 5 轮状态全部显式声明，无静默跳过；8/8 AC 有覆盖；边界用例 5 类齐全；T1~T6 执行且仅 1 项 🟢 命中并已记事；★ 亮点是 360px 溢出被**量测**出来而不是被目测放过，且根因定位到唯一元素。
- 🟦 **研发负责人**: ✅ 测试真正盯着本次风险点（夹具字面量是否恢复原意、钉表是否同步、判定式是否被顺手放宽），用例条数与反例牙齿都做了断言；依赖/工具缺失（govulncheck、1.23 工具链）如实标缺口而非装作扫过。
- 🟩 **高级产品经理**: ✅ AC 覆盖与业务价值对齐（可用性 = 承诺可被机器验证）；UAT-1 是真实人审门且已由用户背书；产品边界（不做前端、不加依赖）未被测试偷偷扩范围。
- 🔴 **资深用户体验官**: ✅ 文档类交付物的"用户体验"被转成可测项（四视口无溢出、目录高亮、断网可开、锚点可达），并在测出 17px 溢出后修复；可访问性/键盘可达未做专项（无误报为已测）。

**结果: 4/4 全票 → ✅ 自动进入 `6-review`**

### 测试阶段新增的修复任务

- **T03**（由本轮第 4 轮发现触发）：`.doc-body code{overflow-wrap:anywhere}` —— 已执行并复测（360px 溢出 377→360），已回填 `TASK.md` 与 `T01/T02` 同一 change 下。
