# REVIEW: agentkit 产品基线验收

- **Change ID**: `agentkit-framework`
- **关联**: `@.specs/agentkit-framework/REQUIREMENT.md`、`TASK.md`、`TEST.md`
- **审查范围**: `6bbec97..HEAD` 三个提交（`e719a69` 代码 / `7039b7f` 文档与 change 工件 / `2049508` 状态）
- **项目类型**: Go 库 + 单文件产品文档（跳过 2a-ui-design ⇒ 无 UI-DESIGN.md 基线）

---

## 第一轮 · Spec 合规审查

| AC | 实现证据 | 测试证据 | 判定 |
|---|---|---|---|
| AC-1 文档断网可开、九段、零外链 | `PRODUCT-DESIGN.html`（1897 行） | 脚本断言 + CDP 四视口量测（`TEST.md` §1.1 / §4.1） | ✅ |
| AC-2 四档等号基线 | 未改判定式（`git diff` 空） | `TestGuard_NoDirectLLMCall` / `TestGuard_HTTPDirectionClassification` | ✅ |
| AC-3 空角色 fail-closed | 未改授权路径 | `TestRegistry_RoleUndeterminedFailClosed` 等 | ✅ |
| AC-4 五句起服务 e2e | 未改装配路径 | `TestPreset_EndToEnd_*` ×5 | ✅ |
| AC-5 根包全绿 | `e719a69`：8 处字面量 + T05 自检 | 23 子用例 / 24 RUN 行 / `go test ./...` 18 包绿 | ✅ |
| AC-6 钉表 | `GATE-PINS.md5` 哈希随内容刷新（两次：T01、T05） | 缺口 0 + md5 对照 | ✅ |
| AC-7 过审状态与背书 | 头部/页脚/§0 三处一致 | grep 断言 | ✅ |
| AC-8 四件套互链 | CONTEXT §8.1 更正为 8 处字面量；STATE 活跃 change 与阶段一致 | AC-8 脚本 | ✅ |

**范围检查**：

- 无 `out of scope` 内容被引入（未做前端、未加依赖、未引 CI、未动判定式）。
- 无范围蔓延的功能：T03/T04/T05 都是**对本次在范围内交付物的缺陷修复**（T03/T04 修产品文档自身缺陷，T05 补本 change 代码的回归自检），不新增对外能力。
- 无投机抽象 / 扩展点 / 兼容层：diff 中新增代码仅 3 行断言 + 4 行 CSS（两条规则 + 两条注释）。
- 未触动 DESIGN 之外架构：本 change 无 DESIGN，未改任何包结构或依赖方向。

---

## 第二轮 · 代码质量审查（6 维衰退风险）

### 2.0 TEST.md 五轮金字塔完整性（先查）

- [x] 5 轮状态全部明确（✅/⚠️/❌ 各有理由）
- [x] 跳过/部分跳过均有理由（性能无预算、安全工具缺失、可观测不适用库）
- [x] 第 1 轮每条 AC 有覆盖（8/8）
- [x] 第 2 轮标注"无预算 → 不做预算判定"，并未伪造达标
- [x] 第 3 轮依赖/秘钥/SAST/OWASP 各有处理记录（含 ❌ 与 ⚠️ 的诚实标注）
- [x] 第 4 轮四视口 + 工具链声明
- [x] 第 5 轮逐项映射到库内可观测面
- **结论**：无 🔴，不退回 5-test。

### 2.1 六维诊断

```
### 🟡 R2 · Change Propagation：两个布局常量可被一次批量替换再次塌缩
**Symptom**：`guard_test.go:897-898` —— `repoSubtree` 与 `extractedSubtree` 是两个相邻的字面量，
6bbec97 已经证明"批量替换会同时改到两者"是真实发生过的变更路径；塌缩后失败信息是
`want false got true`，不指向常量本身。
**Source**：Fowler · Refactoring · Divergent Change / Shotgun Surgery（一处改动波及不相关断言）
**Consequence**：同类改动再次发生时，下一个人要重新做一次 `git log -p` 考古才能定位，估 30+ 分钟。
**Remedy**：在用例开头加常量自检（常量相等即 `t.Fatalf` 并打印值）。**已修（T05）** ——
比"写进注释"强，因为注释不执行。
```

```
### 🟢 R3 · Knowledge Duplication：「两套布局」规则在 3 处被表述（可接受）
**Symptom**：`guard_test.go:886-895`（注释）／`.specs/CONTEXT.md` 已锁决策／`TEST.md` §4.1
**Source**：Pragmatic Programmer · DRY（概念重复）
**Consequence**：改规则时三处要同步；但三者角色不同（可执行断言 / 决策留痕 / 测试报告），
不属于"同一业务规则被实现两遍"。
**Remedy**：不再新增第四处；已在 TASK T02 把三处口径列为必须一致项。不修。
```

```
### 🟢 R1 / R4 / R5 / R6：无命中
R1 认知过载：新增代码 3 行断言 + 4 行 CSS，读一遍即懂，无隐式状态。
R4 偶然复杂：没有为"两套布局"引入参数化矩阵或配置项 —— 两个常量 + 一条断言已是 rung 3 停点。
R5 依赖混乱：diff 无新增 import，依赖方向未变（根包仍只依赖标准库 + errgroup）。
R6 领域扭曲：常量名 `repoSubtree` / `extractedSubtree` 与文档里"仓库布局 / 抽取布局"逐字对应。
```

### 2.2 架构依赖检查（未触发）

触发条件逐项核对：无新增/重命名 package、无危险 import 合并、无新中间件、跨模块改动 = 1 个测试文件。
→ **未触发**，不画依赖图（本 change 的依赖图见 `CONTEXT.md` §3.2，未变）。

### R19 过度工程专项

- 重造标准库 / 平台原生 / 本仓已有：无。
- 未举 rung 2-5 不覆盖证据就引入新依赖：无（依赖数仍为 1）。
- 死接缝 / 柔性预留：无。
- 新增 `ponytail:` 标记：0 条。
→ **无 R19 发现**。

---

## 第三轮 · UI 视觉审查（命中条件：diff 含 `.html`）

> 说明：本次 `.html` 是**文档制品**而非产品界面，且跳过 `2a-ui-design`（无 UI-DESIGN.md 基线）。
> 因此 3.1 无 frontmatter 可比对，改以"文档自身的 `:root` token 集"为基线；3.3 以本文档声明的调性为准。

### 3.1 Design Tokens 一致性

- ✅ 主体颜色全部走 `var(--…)`（`:root` 定义 hex 兜底 + `@supports` oklch 覆盖）
- 🟡 **偏离 1**：暗色 rail 面板（`.app-rail` 起 `PRODUCT-DESIGN.html:187-199`）有约 10 处硬编码 hex（`#152238` / `#c8d3e4` / `#6f83a3` …），未进 token
- 🟡 **偏离 2**：SVG 拓扑图/时序图的 `fill`/`stroke` 有 6 处硬编码 hex（`:586-592` 等）
- 🟢 判定：两者都是**图示与小众面板**，不参与正文可读性；本 change 未新增任何硬编码色（新增 CSS 只用 `var(--brand)`）。登记议题 `doc-theme-tokens`，不阻塞。

### 3.2 Anti-Pattern 扫描（逐项）

| 类别 | 结果 | 证据 |
|---|---|---|
| 字体 | ✅ 无 Inter/Roboto/Arial | 系统字体栈（`-apple-system, …`） |
| 颜色 | ✅ 无纯黑；`#fff` 仅用于深色底 | `color:#fff` ×5 均在 `var(--brand)` / 深色 rail / 渐变上，实测白字对 brand 对比度 **5.96** ✅ |
| 阴影 | ✅ 平面为主 | 卡片 `box-shadow` 极轻，无大投影 |
| 边框 | 🟢 命中 3 类彩色左条（3–4px）：`.note`/`.notice`（`:146`、`:281`）/`.lead`（`:96`）/ 1 处内联（`:438`）；`.nav-item` 激活条 2px（`:65`） | **判定不命中**：`.note` 等是文档提示块（admonition）惯例，不是"卡片状态条"；`.nav-item` 是导航激活态。规则意图是禁止 SaaS 卡片左彩色状态条，此处为文档语义块。已记录，不改 |
| 动效 | 🟡 原缺 `prefers-reduced-motion` | **已修（T04）**：`scroll-behavior:auto` 降级 |
| 布局 | ✅ 无卡片嵌套、无 hero-metric 模板 | — |
| 文案 | ✅ 无 hedging、无 lorem ipsum、按钮动词具体 | 全文中文实义文案 |
| 组件 | ✅ 无 placeholder 冒充 label（无表单控件） | — |

### 3.3 视觉北极星一致性

**问**：只看截图，能看出这是"工程克制、证据优先"的框架文档吗？
**答**：✅ 能 —— 全篇以 `文件:行` 证据表、判据表格、可复跑命令为主，无营销大图/渐变标题党；调性由信息密度而非装饰承担。

### 3.4 无障碍快检

| 项 | 结果 |
|---|---|
| 颜色对比 ≥ WCAG 2.1 AA | ✅ **实测 9 组全部 ≥ 4.5**：`--text/--bg` 14.85、`--text/--surface` 16.36、`--text-2` 8.81、`--text-3` 5.00、`--text-3` on `surface-2` 4.77、`--brand` 5.96、`--ok` 6.49、`--warn` 6.39、白字 on brand 5.96 |
| 键盘可达 | ✅ 目录为 `<a href="#…">`，Tab 顺序 = 文档顺序（无 `tabindex` 干预）；本 change 无按钮组件 |
| 焦点环可见 | 🟡 原缺失（回落浏览器默认蓝）→ **已修（T04）**：`:focus-visible` 品牌色 2px |
| `prefers-reduced-motion` | 🟡 原缺失 → **已修（T04）** |
| 表单 label 关联 | ✅ 不适用（无表单） |
| 图片 alt | ✅ 不适用（无 `<img>`；SVG 均带 `role="img"` + `aria-label`，共 4 处） |

---

## 第四轮 · 补充审查

- **4.1 技术债评估**：**未命中** —— 非里程碑/季度版本，且 `.specs/CONTEXT.md` 技术债段于 2026-10-23 刚更新。
- **4.2 跨模型 spot-check**：**未命中** —— 未涉及安全实现、并发实现、无 >80 行函数、覆盖率无下降（18 包覆盖率与改动前一致）。

---

## 严重度汇总

| 级别 | 数量 | 明细 | 处理 |
|---|---|---|---|
| 🔴 Critical | **0** | — | — |
| 🟡 Major | 4 | R2 常量可再塌缩（T05 已修）；reduced-motion 缺失（T04 已修）；focus-visible 缺失（T04 已修）；暗色 rail / SVG 硬编码色（议题） | 3 修 1 登记 |
| 🟢 Minor | 2 | R3 三处口径重复（接受）；彩色左条 3px（判定不命中，记录） | 接受 |

**R2.5 合规**：所有 Critical（0 条）无待修项；Major 中 3 条已修、1 条登记议题 `doc-theme-tokens` 并经本轮审查接受。

---

## 产出修复任务

已追加到 `TASK.md` 并执行完毕（均 `status="done"`，含 SUMMARY 摘要于 `TEST.md` / 本报告）：

- `T04` 无障碍补强（reduced-motion + focus-visible）
- `T05` 夹具自检（防 6bbec97 复发）

---

## 🛡️ G4 审查门禁（R9 / R13 · 已执行）

🗳️ G4 审查门: REVIEW.md 是否通过？

- 🟫 **资深测试工程师 (M)**: ✅ 三轮主审查都做了（第三轮因 `.html` 命中而做）；TEST.md 五轮完整性逐项核对无 🔴；每条 AC 都能追到实现与测试证据；T03/T04 的修复都有复测数据（四视口 overflow=False）。
- 🟦 **架构师**: ✅ 未参与架构决策的改动（判定式、依赖方向、包结构均未变）；R19 专项为 0 条；未命中架构依赖检查的触发条件，报告也如实写明"未触发"而不是省略。
- 🟩 **领域专家**: ✅ 领域语言一致（"仓库布局 / 抽取布局"↔ `repoSubtree`/`extractedSubtree`）；无范围蔓延（T03–T05 都是本次交付物自身的缺陷）；AC-5 的"八处字面量"表述与 `git log -p` 事实一致。
- 🔴 **安全审计师**: ✅ 无 Critical；等号基线/空角色/脱敏/出域未被任何改动触碰（AC-2/3/4 复跑通过）；安全侧缺口（govulncheck 未装）已在 TEST.md 显式登记而非掩盖。

**结果: 4/4 全票 → ✅ 自动进入 `7-integration`**
