# agentkit 架构（随包文档 1/3）

> 本文件**不写任何会漂移的数字**。行数、用例数、门禁状态一律用下面的命令现取；
> 文档与代码打架时，**以命令输出为准并回来改文档**（LESSONS L-014 / R3：同一不变量不写两遍）。
>
> ```bash
> go test -race -count=1 ./本包/...                              # 是否全绿
> find 本包 -name '*.go' ! -name '*_test.go' -exec cat {} + | wc -l
> go list -deps ./本包/... | grep "$(awk '/^module /{print $2; exit}' go.mod)" | grep -vc '/本包'   # 出域宿主依赖，应为 0
> python3 本包/tools/portability_check.py                 # 陌生 module 取走实验
> cat .specs/agent-engine/GATE-REPORT.md                                 # 门禁读数
> ```

## 1. 这张图想说什么

```
                     ┌──────────────────────────────────────────┐
   业务代码  ───────▶ │  engine   New(四个必需注入点) / Handle / Stop │   ← 想快速搭建，只碰这一层
                     └───────────────┬──────────────────────────┘
                                     │ 组装（不实现任何判定）
   ┌─────────────────────────────────┴─────────────────────────────────┐
   │  preset   内置插件全家桶：Perms / Tools / Model / Loop / Server /   │
   │           Planner / Scenes / Audit（+ Served：接口即工具）           │
   ├───────────────────────────────────────────────────────────────────┤
   │  kit      装配根：插件契约 + 两阶段注册 + 依赖排序 + 失败回滚          │
   ├──────────────┬──────────────┬─────────────┬───────────┬───────────┤
   │  semantic    │  scene       │  playbook   │  session  │ modelcfg  │  ← 治理层：只消费，不判定
   │  语义→同源计划 │  场景按角色裁  │  剧本预演/补偿 │  无状态装配 │ 三级兜底    │
   ├──────────────┴──────────────┴─────────────┴───────────┴───────────┤
   │  tools（注册表 + 唯一授权谓词）      authz（权限点表 + 角色值域闭集）    │  ← 判定只在这里
   ├─────────────┬─────────────┬────────────┬───────────┬──────────────┤
   │  agent 循环  │  model 出域  │  web 入域   │  security │  audit       │
   │  多轮/降级/轨迹│  唯一 HTTP 出口│  Router/Handler│  脱敏    │  扇出         │
   ├─────────────┴─────────────┴────────────┴───────────┴──────────────┤
   │  kitctx（显式上下文 / 身份 / 预算 / 记忆句柄 / Clock / Logger / Digest）│  ← 一切输入显式携带
   └───────────────────────────────────────────────────────────────────┘
```

**读法**：横向是"能力面"，纵向是"谁允许依赖谁"。三条硬方向：

- 只有 `model` 能出域（发起 HTTP 请求），只有 `web` 能入域（被 HTTP 请求）。其余包出现 `net/http` 即门禁红 —— 这是**等号基线**（集合必须恰等于 `{model}` / `{web}`，多一个少一个都红，ADR-012），由 `guard_test.go` 与 CI 双向核对（基线漂移到宿主仓库同样红）。
- 授权判定**全框架只有一条谓词**：`tools.Registry.LookupAuthorized(name, role)`。`scene` / `playbook` / `semantic` 只是它的消费者，不得自己比较角色字符串。
- 框架对宿主零依赖，且**同仓不分家**（不建独立 go.mod，ADR-010）：搬走能力由"陌生 module 取走实验"担保，而不是由目录名担保。

## 2. 包分层（18 个包，一句话职责）

| 层 | 包 | 一句话 |
|---|---|---|
| 上下文 | `kitctx` | 请求身份、预算、记忆句柄、时钟/日志注入点、`Digest` 单一实现 |
| 判定 | `authz` `tools` | 权限点值域闭集 + 工具注册表；唯一授权谓词在此 |
| 能力 | `security` `audit` `model` `web` | 出域前脱敏、审计扇出、唯一 HTTP 出口、入域框架面 |
| 执行 | `agent` `session` | 多轮循环（降级/轨迹）+ 记忆四类分组与压缩；会话无状态装配 |
| 编排 | `semantic` `scene` `playbook` `modelcfg` | 语义→同源计划；场景/剧本按角色裁剪；模型配置三级顺序 + 注入探活 |
| 装配 | `kit` `preset` `engine` | 插件契约与两阶段注册；内置插件；总装面（四必需注入点 + `Handle`） |
| 接入 | `im` | 平台侧独立进程接入，零业务 HTTP |

## 3. 一次请求的真实路径

```
HTTP/循环入口(web 或 engine.Handle)
  → kitctx 取显式身份（角色/用户/traceid/预算/携带的记忆句柄）
  → semantic 选候选工具（规则/意图）→ tools 表内按角色裁剪 ← 唯一谓词
  → agent 循环：model 出域（先过 security）→ tool_calls → 同表执行 → 回显
  → 步骤轨迹（TraceID + Seq）、降级（StepsLimited / Degraded，各带冻结文案）
  → audit 扇出（投递前再脱敏一次）
```

关键点：**HTTP 层与 agent 层看到的是同一份声明**（`tools.SchemaFor` / `RequiredFromSchema` 反推 params 与权限），所以"接口清单/工具清单/权限点清单"三份不可能漂移；被语义层调用的就是对外服务的那个接口。

## 4. 装配与生命周期

- 两阶段：**Declare**（登记能力：工具、权限点、路由）→ **resolveOrder**（按 `Deps` 拓扑排序，书写顺序不决定接线顺序）→ **Apply**（接线 + 注册 `OnStop`）。
- Apply 失败 ⇒ 按登记**逆序回滚**已 Apply 的插件；装配后重复登记 ⇒ `ErrLateDeclare`。
- `Stop()` ⇒ 钩子 LIFO 执行；停止后各能力各自拒服务（`Err*Stopped`），`engine` 亦在 `Apply` 期挂此钩子。
- ⚠️ 已知边界：装配失败**不回滚宿主持有的对象**（如宿主自己的 `web.Router`），由宿主生命周期负责。

## 5. 文档三件套的分工

| 文件 | 回答什么 |
|---|---|
| `architecture.md`（本文） | 有哪些包、谁允许依赖谁、方向红线、请求路径、生命周期 |
| [`design.md`](design.md) | 为什么是这个形状：不变量、契约、刻意拒绝的抽象、失败语义、值域闭集清单 |
| [`guide.md`](guide.md) | 怎么用：五句起服务、逐步扩到接口即工具/语义/场景/剧本/会话/模型配置、常见报错定位 |

延伸阅读（工程与治理面）：`../README.md`（包表 + 命令）、`.specs/agent-engine/AGENTKIT-LAYERS.md`（分层依据与两仓重复点）、`.specs/agent-engine/AGENTKIT-ACCEPTANCE.md`（三档验收：闭环/留白/债务）、`.specs/adr/0{10,11,12}-*.md`（框架契约 ADR）、`本包/samples/agentkit-demo.go.txt`（可运行样例，十段自校验）。

## 6. 成本与规模边界（当场基准，非估算）

| 被测 | 实测 | 能用来决定什么 |
|---|---|---|
| `session.Assemble` | **≈15–25 µs/条、线性**：10 条 165µs / 100 条 2.49ms / 1000 条 14.3ms（3 次中位）；压缩路径 1.37ms（条目变少，反而更省） | 单请求携带记忆 **≤1000 条**，守住前端 200ms 预算既有前端渲染预算约定；再大先分页，不要继续加载荷 |
| `tools.LookupAuthorized`（唯一谓词） | **≈87 ns/次、0 分配、与表规模无关**（30→300 工具 86→88ns；空角色 84ns 且照样不放行） | 治理税不是瓶颈 ⇒ **别为权限判定再加缓存层**（多一层就多一处漂移） |

```bash
go test -run '^$' -bench Assemble          -benchmem -count=3 ./本包/session/
go test -run '^$' -bench LookupAuthorized  -benchmem -count=3 ./本包/tools/
```

未验证区（诚实登记）：**端到端 P95 与峰值 QPS 未测** —— 上面是单点基准，带模型的回环压测没做，不要拿这两个数冒充端到端延迟。
