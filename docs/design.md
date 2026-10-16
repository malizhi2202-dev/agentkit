# agentkit 设计文档（随包文档 2/3）

> 回答"为什么是这个形状"。每条都带**可机械验证的落点**（测试名 / 门禁判据 / ADR），
> 没有落点的"设计理念"不写进来。

## 1. 七条不变量（invariants）

| # | 不变量 | 落点 |
|---|---|---|
| I-1 | **一切能力皆可插拔**：能力 = `Plugin{Name, Declare, Apply}`（+`Enabled`/`Deps`/`Describe`），关掉一个能力不需要改代码 | ADR-011；`kit` 两阶段与回滚测试 |
| I-2 | **授权判定只有一条谓词**：`tools.Registry.LookupAuthorized(name, role)`；治理层只消费不判定 | 全仓 `grep` 实现数 = 1；`scene`/`playbook`/`semantic` 无角色字符串比较 |
| I-3 | **空角色 ≠ 默认角色**：未判定的角色不放行任何东西（含 `RequiredRoles` 为空er 的工具） | `tools` 表驱动 + `scene_test` / `bind_test`（各自钉住"空角色 ⇒ 零可见"） |
| I-4 | **框架不猜默认值**：不猜默认角色、不猜上下文窗口、不补 `StepTimeout` 默认；缺失就拒或就不动作 | `kitctx.Limits` 契约注释、`session.Window<=0 ⇒ 不压缩`、`authz` 装配期"宁缺不猜" |
| I-5 | **出域唯一 / 入域唯一**，且基线是**等号**：出域集合必须恰等于 `[model]` | ADR-012；`TestGuard_HTTPDirectionClassification` + 门禁 `agentkit-purity` |
| I-6 | **降级绝不当成功**：撞步数上限 ⇒ `StepsLimited` + 冻结文案；模型不可用 ⇒ `Degraded` + 冻结文案，**不返回 error 也不返回空成功** | `agent/loop.go`；`engine_test` 两条端到端用例分别钉住两种降级 |
| I-7 | **轨迹必须可回放**：缺 `TraceID` 直接拒绝进循环（不产生无法回放的执行） | `agent` 的 `trace_id.required`；`engine` 不绕过这道门（探针验证过） |

## 2. 关键设计决策与代价

### 2.1 同一声明产出三件东西（接口 = 工具 = 权限点）

一个能力只写一次：`preset.Served(route, sample, perm, handler)` 同时登记 HTTP 路由、agent 可见工具、权限点；参数 JSON Schema 由 Go 结构体反射（`tools.SchemaFor`），所需权限由 Schema 反推（`RequiredFromSchema`）。

- **收益**：外部接口清单、模型可见工具清单、授权面三者不可能漂移；"把业务接口开放给 agent"零额外接线。
- **代价**：HTTP 面与工具面耦合在同一声明上，想"只对 agent 开放、不开 HTTP"必须显式走 `preset.Direct`。
- **补的门**：`web.Config.Authorize` 未挂时，`Build` 后输出告警（`warnUngatedPerms`）而非静默放行 —— 声明了权限点却没人判，是最危险的假安全感。

### 2.2 两阶段注册（Declare → Apply）

登记与接线分开。Declare 期只往 `Declarer` 里塞东西（工具/路由/权限点），Apply 期才拿 `*Kit` 接线。

- 为什么：装配期需要**先看清全局**（拓扑排序、缺依赖早失败），接线才需要能力句柄。
- 踩过的真实坑：`Kit.OnStop` **只能在 Apply 期注册**。所以 `engine` 自己必须是插件（在 `Apply` 里挂停止钩子）；在 `New` 里事后挂钩会被 `kit` 拒。探针 E4 把这条钉住（摘掉登记 ⇒ 红）。
- 代价：`ErrLateDeclare`（装配后再登记 = 编程错误，直接红），早期版本会静默吞。

### 2.3 值域当数据注入，校验规则留在包内

`authz` 的"合法角色集合"与"合法 enforcement 开关"是宿主注入的**数据**，包内不写死调用方的产品角色；`Scope` / `Enforced` 之类字段**无默认值**（零值即非法）。

- 为什么：写死角色 = 框架长出血缘，换个程序就得改框架；无默认 = 漏配会被当场拒，而不是"看起来能跑"。
- 实测证据：写 `session` 测试时漏填 `Scope`、`Enforced` 两次，都被 `NewPerms` 当场拒 —— 闭集在挡我自己。

### 2.4 记忆结构只有一份

四类分组、条目、冲突、压缩文案唯一归 `agent`；随请求携带的句柄唯一归 `kitctx.MemoryHandle`。`session` 只是**装配点**（固定顺序 `Clear→Deletes→Edits→NewItems→Compress`），不复制任何结构。

- 为什么：`kitctx` 的注释早就写明"刻意不复制其结构，避免同一结构两处定义而漂移"。
- 这也是 `session` 曾被误记为"已取消"的原因 —— 混淆了"取消服务端会话存储"（对的）与"不需要无状态装配层"（错的）。 已按事实改写。

### 2.5 探活/传输/时钟/日志全是注入点

`modelcfg.Prober` 由调用方注入（框架不认识任何厂商协议，测试期零真实出网）；`model.HTTPDoer` + `EndpointPolicy` 注入；`kitctx.Clock/Logger` 注入。

- **限度（登记不藏）**：`GOPROXY=off` 这类设置只在依赖真缺失时才显形。

### 2.6 刻意拒绝的抽象

| 被拒的抽象 | 为什么不做 |
|---|---|
| `internal/registry[T]` 泛型基座（P7c） | `authz` 与 `tools` 在**四条轴**上真实分歧：锁（无锁快照 vs RWMutex）、序（值索引 vs 声明序）、`TrimSpace` 位置、nil 接收者语义。硬合并会把行为差异抹平成隐性 bug。重开条件：出现第 5 张表且四轴一致（评估写在 `authz/doc.go`） |
| 第三方 DI / 反射插件框架 | 违背 ADR-010 的依赖口径（只 stdlib + `errgroup`）与"拿走就能用" |
| 独立子 module | 同仓 + 守卫 + 陌生 module 取走实验已达到同样效果，且不需要维护第二套版本 |
| Scene 里再写一份角色清单 / `ModelConfigID` 字段 | 没人读就不加（P7a/P8 判定）；场景表不抄角色、不抄工具清单 |

## 3. 失败语义表（一眼看懂"错了会怎样"）

| 情形 | 行为 | 判据 |
|---|---|---|
| 装配后重复登记 | `ErrLateDeclare`，Build 失败 | `kit` |
| 某插件 Apply 失败 | 已 Apply 者**逆序回滚**；`currentPackageId` 式语义：失败不污染成功态 | `kit` 回滚测试 |
| 场景/剧本里写了不存在的工具 | 裁掉并记 `Drop{Reason: unknown_tool}` / `Problem{Kind: unknown_tool}`（**配置漂移可诊断**） | `scene` / `playbook` |
| 角色无权限 | 裁掉并记 `not_authorized`；空角色 ⇒ 整批裁掉 | 同上 |
| 剧本预演阶段发现问题 | **零写入**（一次工具都不调）；失败则逆序补偿 | `playbook_test` |
| 补偿动作本身失败 | 记 `CompensationProblems` + 告警，继续补偿其余（best-effort，绝不卡死） | 同上 |
| 句柄载荷坏 | 明确报错，**不静默当新会话**（否则用户以为记忆被清空） | `session` |
| 装配失败后仍调能力 | 各自 `Err*NotAssembled` / `Err*Stopped` 分档（未装配 ≠ 已停止） | `preset` |

## 4. 测试纪律（这个包怎么被证明）

1. **值域闭集 + 未知值必拒**：每包都有 `*Kinds()` / `Valid()` 的闭集测试，断言**恰等于**期望集合（不是"包含"）。
2. **变异探针**：每片交付前跑"故意改坏源码 ⇒ 测试必须红"的探针（本轮 `E1–E4`、`S1–S3`、`Q1–Q4`）。探针必须 (a) 能编译、(b) 自证已落地、(c) 打印真实原因、(d) **报告 `^=== RUN` 计数**（打空的 `-run` 过滤器会产生假绿）。见 `.specs/LESSONS.md` L-097 / L-104 / L-106 / L-107。
3. **陌生 module 取走实验是判据，不是手工活**：`tools/portability_check.py`（临时 module + `GOPROXY=off` + build/vet/test/`-race` + 端到端样例十段逐条命中），已进 `gate_report.py` 的判据性检查。
5. **钉定 = 代码全量**：`agentkit-code-pins` 断言钉定集与 `agentkit/**/*.go` 双向相等（漏钉与幽灵条目都红）。这是"代码是唯一事实源"的机械表达 —— 文档不在钉定面内，因为它是代码的下游；文档与代码冲突时改文档。
4. **判据自身要能红**：断言写"每一项必须出现"而不是"数够 N 个"；步骤清单要求一致；有 `--selftest` 验字符串级判据不误伤注释里的禁令原文。

## 5. 与宿主的关系

框架对宿主零依赖；宿主可以只 import 其中几个包（实测调用方宿主引用最多的是 `security`、`kitctx`、`agent`、`model`）。
*补充（属调用方一侧的账，不在本包追踪）*：本包只保证「零宿主依赖」与「可整目录取走」；调用方是否换用本包、换用几片，记在调用方自己的计划与验收文档里。
