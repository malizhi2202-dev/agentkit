# STATE — agentkit 跨会话项目状态

> 仓库根 `STATE.md`。AI 每次开始新会话先读 `.specs/CONTEXT.md`（项目图谱）+ 本文件。
>
> **远端策略（2026-10-23 起）**：只有 `origin` = `git@github.com:malizhi2202-dev/agentkit.git` 一个远端。
> 不再使用本地镜像副本（原 `/home/malizhi/git-mirror/agentkit.git` 已从 remote 移除），
> 一切 fetch/push 直接从工作区对 `origin` 操作，避免多副本各自演进导致不一致。

---

## 当前位置

- **活跃 Change**：`agentkit-evolution`（D-discovery 第 1 轮 · 5 个子议题）
- **当前阶段**：`D-discovery` **两轮全部完成**（第 1 轮 5 项 + 第 2 轮 5 项，含 agent-shell 第 3 轮修订），结论全部过审并汇入 `ROADMAP.md`；下一步 `A-architect` → `0-change`
- **第 1 轮**：5 子议题已全部过审，结论在 `.specs/agentkit-evolution/ROADMAP.md`
- **上一轮**：`agentkit-framework` 已归档（八阶段闭环，G4 4/4）
- **当前 Task**：T01 ✅ / T02 ✅ / T03 ✅ / T04 ✅ / T05 ✅（全部 `status="done"`）
- **中断任务**：无

### 最近一次 change 产物（已归档）

| 工件 | 路径 | 状态 |
|---|---|---|
| 项目图谱（入场扫描） | `.specs/CONTEXT.md` | 已落盘 · §8.1-1 为已修复态 |
| 产品文档 + 界面原型 | `.specs/archive/2026-10-23-agentkit-framework/PRODUCT-DESIGN.html` | **已过审**（用户原话「好的同意了」，2026-10-23） |
| CHANGE / REQUIREMENT / TASK | 同上目录 | 归档（需求门 4/4、Task 门 4/4） |
| TEST / REVIEW / UAT | 同上目录 | 归档（测试门 4/4、G4 审查门 4/4） |
| 五份 SUMMARY（T01–T05） | 同上目录 | 归档（含真实 verify 输出） |
| 五层对齐 | `.specs/ALIGNMENT.md` | 首轮 · 基线 `2049508` |
| 经验库 | `.specs/LESSONS.md` | `L-001` 新增（批量路径替换塌缩夹具） |
| 变更流水 | `.specs/CHANGELOG.md` | 已记一行 |
| 钉表 | `GATE-PINS.md5` | 已随 T01/T05 两次刷新哈希 |

### 会话开始建议

- 想推新需求 → `@.specs/CONTEXT.md`（图谱 + 禁动清单 + 技术债）→ 开新 change（`R3.2`）
- 想做代码库体检 / 架构梳理 → 走横向命令 M-health / A-architect
- 想对齐文档与代码 → `S-align`（基线见 `.specs/ALIGNMENT.md` §5）
- 想消化遗留议题 → 见下「已留档议题」

## 阻塞与待决策

| 项 | 类型 | 详情 | 待谁 | 自 |
|---|---|---|---|---|
| ~~镜像远端未推送~~ ✅ 已统一远端 | 完成 | 已切换为单一远端 `origin`（GitHub），`d7d2bb9..e178794` 推送完成；本地镜像 `mirror` 远端已移除；推送前全历史密钥扫描 0 命中 | — | 2026-10-23 |

### 已留档议题（R18.4 · 交 D-discovery 消化）

| 议题（=change-id） | 一句话 | 来源 |
|---|---|---|
| `ci-minimum-pipeline` | 最小 CI：`go test -race ./...` + 两个 python 门禁脚本 + 文档断言脚本固化 | `CHANGE.md` 范围排除、`CONTEXT.md` §8.1-3 |
| `doc-path-realign` | 文档中 `.specs/adr/…`、`pkg/agentkit/…`、`agentkit/samples/…` 悬空指针统一 | `CONTEXT.md` §8.1-2、`ALIGNMENT.md` D6-1/D6-2 |
| `e2e-perf-baseline` | 端到端 P95 / QPS 回环压测（当前只有单点基准） | `docs/architecture.md:99`、`CONTEXT.md` §8.2 |
| `coverage-web-preset-audit` | `web` 85.4% / `preset` 88.1% / `audit` 88.4% 低于 core 90% 门槛 | 归档 `TEST.md` §1.3 |
| `test-name-scenario` | 守卫子用例名由路径字面量改为场景名（T1 🟢） | 归档 `TEST.md` §1.5 |
| `govulncheck-baseline` | 离线环境缺 `govulncheck` / `gosec`，依赖 CVE 结论空缺 | 归档 `TEST.md` §3.1 |
| `doc-theme-tokens` | 产品文档暗色 rail 面板与 SVG 图示硬编码 hex 未 token 化 | 归档 `REVIEW.md` §3.1 |

## 决策日志（最近 10 条，倒序）

- `[2026-10-23]` 远端策略改为**单一 origin**（GitHub），移除本地镜像 `mirror`：多副本是"代码不一致"的来源 — 本文件顶部
- `[2026-10-23]` 用户原则确立：**"有现成的直接接过来"** —— 现成件接入清单落 `ROADMAP.md` §10.6（rea/OpenHands/MCP Registry/A2A-SDK/gym-anything/SkillAnything），且一律不进核心依赖
- `[2026-10-23]` D-discovery 第 2 轮（用户六点愿景）：5 子议题全过审 —— Capability 统一建模、MCP/A2A 导出+对接官方 Registry、老服务扫描只出提案、逆向器当工具/壳走 OpenHands 路线、**形态 B（核心不变 + agentkit serve 侧车）** — `.specs/agentkit-evolution/ROADMAP.md` §7-§11
- `[2026-10-23]` D-discovery 第 1 轮：5 子议题（observability / ecosystem / orchestration / delivery-gates / developer-experience）GD 门 4×2:1 + 1×全票，人审 5/5 通过 → 汇入 `ROADMAP.md`；明确不做图引擎/持久化/MCP 服务端/CLI — `.specs/agentkit-evolution/`
- `[2026-10-23]` 第二轮 intel-scan：门禁全绿（18 包 / race / vet / 钉表 0 / portability J-PORT / gofmt 干净）；记录环境坑——默认 `PATH` 的 go 是 1.13.8，须用 `/usr/local/go/bin/go`（1.26.2） — `.specs/CONTEXT.md` §2
- `[2026-10-23]` `agentkit-framework` 归档：G4 4/4 通过后落 `.specs/archive/2026-10-23-agentkit-framework/` — `CHANGELOG.md`
- `[2026-10-23]` 6-review 结论：0 Critical；3 条 Major 已修（T04×2、T05），1 条登记议题 — 归档 `REVIEW.md`
- `[2026-10-23]` 五层对齐首轮：无 D2/D5；存量 D6 孤儿指针留档 `doc-path-realign` — `.specs/ALIGNMENT.md`
- `[2026-10-23]` 提名 LESSONS `L-001`：批量路径替换会塌缩"两套布局"夹具，改用运行期自检防复发 — `.specs/LESSONS.md`
- `[2026-10-23]` 5-test 第 4 轮量测发现 360px 溢出 377>360，根因唯一（长路径 code），T03 一条 CSS 修复 — 归档 `TEST.md` §4.1
- `[2026-10-23]` 守卫两套布局子树常量必须不同值；夹具字面量禁止批量替换 — `CONTEXT.md` 已锁决策
- `[2026-10-23]` PRODUCT-DESIGN 过审：状态「起草中」→「已过审」，用户原话背书 — 归档 `PRODUCT-DESIGN.html`
- `[2026-10-23]` 根守卫夹具红定性为**测试夹具**问题（8 处字面量），判定函数一行不改 — 归档 `REQUIREMENT.md#AC-5`
- `[2026-10-23]` 本 change 跳 2-design：无新架构决策 — 归档 `CHANGE.md`
- `[2026-10-23]` 入场扫描采用「既有 README + docs 三篇合并 + 代码交叉验证」口径 — `.specs/CONTEXT.md` §0

## 已归档 Changes

| 日期 | Change | 摘要 | 提交 |
|---|---|---|---|
| 2026-10-23 | `agentkit-framework` | 产品文档过审 + AC-1~AC-8 验收基线；根守卫夹具 8 处字面量恢复（判定式零改动），`go test ./...` 18/18 全绿；文档四视口无溢出、无障碍补强 | `e719a69` `7039b7f` `2049508` `0f45608` + 归档提交 |

---

## 横向命令状态

```yaml
ai_context_doc: .specs/CONTEXT.md
last_intel_scan: 2026-10-23
intel_scan_rounds: 2

last_architect_at:
last_evolve_at:
last_evolve_promoted: []

last_health_at:
last_health_score:
last_alignment_at: 2026-10-23
last_alignment_baseline: "2049508"
```

---

## 健康检查（AI 每次会话开始前自检）

- [x] `.specs/` 目录存在，结构含 `CONTEXT.md` / `ALIGNMENT.md` / `LESSONS.md` / `CHANGELOG.md` / `archive/`
- [x] `.specs/CONTEXT.md` 已生成（含项目图谱与证据行号）
- [x] 无活跃 change（上一个已归档，工件齐全）
- [x] `go test ./...` 18/18 全绿；`-race` 通过；钉表缺口 0（2026-10-23 实测）
- [x] 五层对齐已跑，基线 `2049508`
- [x] 7 个议题已留档，等待 D-discovery 消化
- [x] 已确认 go 工具链：`/usr/local/go/bin/go`（go1.26.2）；默认 `PATH` 的 `go` 是 1.13.8，会假红（`undefined: any`）
