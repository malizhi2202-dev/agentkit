# LESSONS — 跨任务失败知识库

> 项目级常驻文件，位置：`.specs/LESSONS.md`
> **每个新任务开工前必扫**（R1.8）；任务完成后按本文件末尾的「提名条件」决定是否新增。

---

## 标签索引

- `arch` 架构决策类
- `lib` 第三方库选型 / 陷阱
- `tool` 构建 / 测试 / 工具链
- `data` 数据建模 / 迁移
- `perf` 性能
- `a11y` 可访问性
- `sec` 安全
- `ux` 交互 / 视觉
- `ops` 部署 / 运维
- `proc` 流程 / 协作

---

## 使用方式

### 写代码前（DEV 阶段，R1.8）

1. AI 用任务的 `files` + `action` 关键词 grep 本文件
2. 命中的条目必须在执行计划里显式声明：
   - 「已查阅 L-NNN，本次方案与之的差异是 X」 或
   - 「已查阅 L-NNN，本次确认仍适用，因此不重试该方案」
3. 若计划方案与某条完全相同 → 触发 R1.6，不允许直接重试

### 任务完成后（INTEGRATION ARCHIVE 步骤）

AI 扫本次 change 的所有 `*-SUMMARY.md` 与遗留 `*-PROGRESS.md`，按下面「提名条件」筛选并追加新条目。

---

## 条目

### L-002 · `tool` · "钉表"只比路径不比哈希 = 假门禁

- **现象**：`GATE-PINS.md5` 存了每个 `.go` 的 md5，但校验脚本只比对**路径集合**（未钉 / 幽灵），
  从不比对哈希。结果是任意修改 `.go` 都不会让门禁变红。
- **举证**：加哈希校验后实测 **14 个文件**与钉定不一致（13 个历史漂移 + 1 个本次改动），
  此前一直无人发现。脚本自己的报错文案还让人 `--freeze`，而该 flag 根本不存在。
- **教训**：任何"基线/快照/钉定"机制，必须有一条**负例**（改一个字节 → 必须红）才算接通；
  否则它只是文档。设计门禁时先写负例，再写正例。
- **已修复**：`tools/agentkit_code_pins.py` 增加哈希校验与 `--freeze`（change `ci-minimum-pipeline`）。

### L-003 · `tool` · 本机默认 `go` 是 1.13.8，脚本里只设 `GO=` 不够

- **现象**：`tools/ci_local.sh` 里设了 `GO=/usr/local/go/bin/go`，但 `portability_check.py`
  通过子进程调 `go` 走的是 `PATH`，仍然命中 1.13.8 → 报 `undefined: any`。
- **教训**：为子进程准备工具链时，改 `PATH` 而不是只改变量；并把这条写进脚本注释，
  避免下一个人再踩。

### L-001 · [tool, proc] 批量路径替换会把"两套布局"的测试夹具塌缩成同值

- **首发**: `agentkit-framework` · `T01` · 2026-10-23
- **上次复核**: 2026-10-23
- **适用栈**: Go 1.x 测试（任何用常量区分"仓库布局 / 抽取布局"的守卫测试）
- **状态**: active
- **关键词**: repoSubtree extractedSubtree 布局 批量替换 sed 夹具 guard 常驻红 guardInsideFramework pkg/agentkit

**问题场景**

删包 / 去路径 / 独立布局这类提交，常有人在全仓跑
`sed -i 's#/pkg/agentkit#/agentkit#g'` 一类的批量替换。守卫测试里同时存在
`repoSubtree`（框架在 `<module>/pkg/agentkit`）与 `extractedSubtree`（框架在 `<module>/agentkit`）
两个常量时，替换会把两者变成同值 —— 于是"抽取后仓库布局路径已不再是框架子树"这条
跨布局断言自相矛盾（`want false, got true`），根包 `go test ./...` 常驻红。

**当时尝试的方案**

`6bbec97` 就是这么做的：把守卫里的 `/pkg/agentkit` 全部改成 `/agentkit`，包括 8 处夹具字面量
（两个常量 + 4 条相似前缀 + 尾斜杠 + `..` + 抽取布局的对照 path）。判定函数本身没动。

**为什么不行**

- 失败信号极不指向根因：只报 `guardInsideFramework("example.com/hostmod/agentkit","example.com/hostmod/agentkit/model") = true，want false`，
  看上去像"判定函数写错了"，实际是夹具常量被替换；
- 排查成本高：要 `git log -p -- guard_test.go` 考古到替换前才能确定原意（本次约 30 分钟）；
- 一旦常驻红，`go test ./...` 的真红会被这条已知红掩盖。

**当前推荐做法**

1. 批量替换后立刻跑根包测试，并用 `git log -p` 核对被替换过的夹具字面量；
2. 用 `repoSubtree != extractedSubtree` 的**运行期自检**（`guard_test.go` T05 已加）替代"靠注释提醒"；
3. 判定函数与期望值一律不动 —— 只恢复字面量（`T01`，`e719a69`）。

---

## 提名条件（写新条目前逐条核对）

- 调试 / 试错耗时 > 30 分钟 → 提名
- 错因不局限于本任务、其它任务也会撞 → 提名
- 6 个月内有合理概率被再次尝试 → 提名
- 否则不入库（避免污染）
