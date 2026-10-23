# REQUIREMENT: agentkit 产品基线验收

- **Change ID**: `agentkit-framework`
- **关联**: `@.specs/agentkit-framework/CHANGE.md`、`@.specs/CONTEXT.md`、`@.specs/agentkit-framework/PRODUCT-DESIGN.html`（已过审）
- **阶段说明**: 无新架构决策 ⇒ 按 `1-requirement.md`「触发下一步」直接进 `3-task`，不产出 DESIGN.md

---

## 用户故事

- **US-1**：作为**接入方研发**，我想在 5 分钟内判断"这个框架的不变量是否真的被机器守着"，以便决定要不要把它拿走用。
- **US-2**：作为**平台运维**，我想让 `go test ./...` 常驻全绿，以便任何一次回归都能被一眼看见，而不是淹没在"已知红"里。
- **US-3**：作为**产品 / 评审**，我想让产品文档的每条核心承诺都对应一条可重跑的命令，以便过审后不靠口头记忆，也不靠"上次好像是绿的"。

## 验收准则（AC）

### AC-1 · 产品文档断网可开、九段齐全、零外部依赖

- **Given** 仓库里存在 `.specs/agentkit-framework/PRODUCT-DESIGN.html`
- **When** 断网环境用浏览器直接打开它，并执行下述校验脚本
- **Then** 十个章节（`s0`~`s9`）齐全；文件内**不含**任何外部资源引用（`<link>` / `<script src>` / `<img>` / `@import` / `url(http…)`）；目录 11 个锚点全部命中存在的 `id`
- **验证方式**: 
  ```bash
  python3 - <<'PY'
  import re
  s=open('.specs/agentkit-framework/PRODUCT-DESIGN.html',encoding='utf-8').read()
  assert len(re.findall(r'id="s\d"',s))==10, '章节不足九段'
  assert not re.search(r'<link\b|<script[^>]+src=|<img\b|@import|url\(https?:',s), '存在外部资源'
  ids=set(re.findall(r'id="([^"]+)"',s))
  assert all(h in ids for h in re.findall(r'href="#([^"]+)"',s)), '锚点悬空'
  print('AC-1 ok')
  PY
  ```

### AC-2 · 出口/入口方向基线仍是等号（产品承诺 §2.4 / §3.5）

- **Given** 框架代码与守卫测试均未改动判定式
- **When** 单独运行方向与出域判据
- **Then** `D1 出域 == {model}`、`D2 入域 ⊆ {web}`、`D3 中立 ⊆ {model,web}`、`D4 未归类即红` 四项全部通过；出域集合非空（反空转）
- **验证方式**: `go test -count=1 -run 'TestGuard_NoDirectLLMCall|TestGuard_HTTPDirectionClassification' .`

### AC-3 · 空角色 fail-closed（产品承诺 §2.3 / §6.3）

- **Given** 角色未判定（空字符串）
- **When** 用该角色查询工具授权
- **Then** 不返回任何可用工具（`allowed=false`），且**不是**因为"工具不存在"而拒绝（三态可分辨）
- **验证方式**: `go test -count=1 -run 'TestRegistry_RoleUndeterminedFailClosed' ./tools/`

### AC-4 · 五句起服务可端到端跑通（产品承诺 §5.1）

- **Given** 用 `preset` 五件套装配出的引擎
- **When** 跑仓内端到端用例（含真监听起停、HTTP 往返、换角色裁工具、审计已脱敏）
- **Then** 全部通过，且换角色后模型可见工具集合按声明变化
- **验证方式**: `go test -race -count=1 -run 'TestPreset_EndToEnd' ./preset/ ./web/`

### AC-5 · 根包 `go test ./...` 全绿（消除常驻红）

- **Given** 两种布局的夹具常量被一次路径替换连累（`repoSubtree` 与 `extractedSubtree` 都写成 `host+"/agentkit"`；仓库布局的 4 条相似前缀用例、尾斜杠用例、`..` 用例、以及"抽取后仓库路径不再属于子树"用例共 8 处字面量被一起改成 `agentkit/…`）
- **When** 只把这些**字面量恢复为原意**（仓库布局回到 `<module>/pkg/agentkit`，抽取布局仍是 `<module>/agentkit`），判定函数 `guardInsideFramework` 一行不改
- **Then** `go test ./...` 退出码 0；`TestGuard_AgentkitImportClassification` 的**全部子用例**通过，且用例条数不变（23 条子用例 = 仓库布局 17 + 抽取布局 6，连同父用例共 24 行 `=== RUN`）；相似前缀三条仍判违规（反例牙齿未被磨掉）
- **验证方式**: `go test ./... && test "$(go test -count=1 -v -run 'TestGuard_AgentkitImportClassification' . | grep -c '^=== RUN')" -eq 24`

### AC-6 · 钉表与代码全量双向相等（含本次改动的哈希刷新）

- **Given** 本次修改了钉定文件集合内的 `guard_test.go`
- **When** 跑钉表判据并核对哈希
- **Then** 输出 `缺口=0`；`GATE-PINS.md5` 中 `guard_test.go` 的哈希与 `md5sum guard_test.go` 一致
- **验证方式**: `python3 tools/agentkit_code_pins.py && md5sum guard_test.go | awk '{print $1}' | grep -qx "$(grep ' guard_test.go$' GATE-PINS.md5 | awk '{print $1}')" && echo AC-6 ok`

### AC-7 · 产品文档状态为已过审且有背书原话

- **Given** 用户于 2026-10-23 以原话「好的同意了」背书通过人审门
- **When** 检查文档头部与页脚
- **Then** 状态标为 `已过审`，且文件内可检索到「好的同意了」与「2026-10-23」；不再出现「状态：起草中」
- **验证方式**: `grep -q '状态：已过审' .specs/agentkit-framework/PRODUCT-DESIGN.html && grep -q '好的同意了' .specs/agentkit-framework/PRODUCT-DESIGN.html && ! grep -q '状态：起草中' .specs/agentkit-framework/PRODUCT-DESIGN.html && echo AC-7 ok`

### AC-8 · 四件套状态互链一致（产品文档 / REQUIREMENT / STATE / CONTEXT）

- **Given** 本次落下 `CHANGE.md`、`REQUIREMENT.md`、`TASK.md` 与更新后的 `STATE.md`
- **When** 检查互链
- **Then** `PRODUCT-DESIGN.html` 指向 `REQUIREMENT.md` 存在；`STATE.md` 的活跃 change 为 `agentkit-framework` 且当前阶段与实际产出一致；`CONTEXT.md` §8.1 的根因描述与实际修复一致（8 处夹具字面量，而非"1 行常量"）
- **验证方式**: 
  ```bash
  test -f .specs/agentkit-framework/REQUIREMENT.md && grep -q 'agentkit-framework' STATE.md \
    && grep -q '8 处' .specs/CONTEXT.md && grep -q '已过审' .specs/agentkit-framework/PRODUCT-DESIGN.html && echo AC-8 ok
  ```

---

## 范围切分

### v1（本次必做）

- AC-1 ~ AC-8 全部八条。
- 产出 `CHANGE.md` / `REQUIREMENT.md` / `TASK.md`，并更新 `STATE.md`、`CONTEXT.md` §8.1。

### v2（下一轮考虑，不本次 · 即议题，R18.4）

- `ci-minimum-pipeline`：最小 CI（`go test -race ./...` + 两个 python 门禁脚本），让门禁从"能跑"变成"自动跑"。
- `doc-path-realign`：把 `.specs/adr/…`、`pkg/agentkit/…` 的悬空指针统一为仓库相对路径 + 一句宿主口径说明。
- `e2e-perf-baseline`：端到端 P95 / QPS 回环压测（当前只有单点基准，`docs/architecture.md:99` 已诚实登记未测）。

### out（永远不做）

- 不在框架内实现控制台前端（§6 原型是消费方参考实现）。
- 不放宽任何门禁判据来换绿灯（等号基线、空角色 fail-closed、脱敏必证据）。
- 不新增第三方依赖（白名单仍只有 `errgroup`）；不为"以后可能用到"加抽象（R19.2）。

---

## 非功能性需求

- **性能**: 无 —— 本次不触碰任何运行路径（只改 `_test.go` 夹具与文档）。
- **可访问性**: 产品文档为本地单文件，目录锚点键盘可达；无新增交互控件。
- **安全**: **不得降低**。等号基线、空角色 fail-closed、脱敏证据、黑名单不可关闭、公网端点逐主机批准，一条都不动（AC-2/AC-3/AC-4 即其回归）。
- **兼容性**: Go 1.23+（`go.mod:3`）；测试代码只用标准库 `testing`。
- **可观测性**: 无新增埋点；`go test` 输出即证据。

## 依赖与假设

- 依赖：本机 Go 工具链（实测 go1.26.2）、`python3`（钉表脚本）、`md5sum`。
- 假设：
  1. 用户对 `guard_test.go` 的改动已背书（原话「好的同意了」，2026-10-23）；
  2. 恢复字面量为 `pkg/agentkit` 后，判定函数无需改动即可通过 —— 已由 `git log -p -- guard_test.go` 的 `6bbec97` 提交恢复到事实（该提交只做了 `/pkg/agentkit` → `/agentkit` 的字面替换）；
  3. 用例条数不变（23 条子用例）是可核对的：仓库布局 17 条 + 抽取布局 6 条，`=== RUN` 共 24 行（含父用例）。

---

## 🛡️ 需求门禁（R9 / R13 · 已执行）

🗳️ 需求门: REQUIREMENT.md 是否通过？

- 🟫 **高级产品经理**: ✅ 8 条 AC 完整覆盖 CHANGE 的 Why 三项欠账与产品文档核心承诺；v1/v2/out 切分清楚，三项未做事项已登记为议题而非悄悄消失。
- 🟦 **资深用户评测员**: ✅ 每条 AC 都有 Given/When/Then 且落在一句可执行命令上（AC-7 的 `grep` 写法已修正为可返回非零的形态）；描述的是外部可观察结果（测试通过 / 文件可打开 / 哈希一致），不是实现细节。
- 🟩 **架构师**: ✅ 无新架构；跳过 2-design 有明确理据（只改夹具与文档）；NFR 已声明"不触碰运行路径"；风险（禁动清单、钉表同步）已在 CHANGE 登记。
- 🔴 **安全审计师**: ✅ AC-2/AC-3/AC-4 构成安全行为回归；AC-5 明确"只恢复字面量、判定函数一行不改"，且要求反例牙齿（相似前缀三条）仍判违规，避免以"修红"为名放宽判据。

**结果: 4/4 全票 → ✅ 自动进入 `3-task`**
