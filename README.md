# agentkit —— 可跨 Go 程序搬运的agent 服务框架

> 「提取出一个公共的框架，其他 go 程序拿走就能用」——本目录是该要求的可执行交付物。
> 零依赖口径的权威依据：`.specs/adr/010-agentkit-internal-imports-and-extraction.md`（ADR-010）。

## 1. 框架用途

把「自然语言实体解析 → 工具调用编排 → 模型出域（脱敏/黑名单/围栏/审计）→ IM 接入」这条链路
做成与宿主解耦的 Go 包：宿主只提供**注入式接口的实现**（日志 / 脱敏 / 时钟 / HTTP 出网 / 实体识别 /
IM 传输），框架不 import 宿主的任何业务代码。

## 2. 包清单（各自职责一句话）

| 包 | 职责（一句话） |
|---|---|
| `kitctx/` | 请求上下文与跨层契约：`RequestContext{UserID,Role,CallerID,RepresentedUser,TraceID,Limits,Masker,Clock,Logger}`、`Memory`、`Trace/Step` 与 `RequestFrom/LoggerFrom/ClockFrom` 取值口。 |
| `model/` | 模型协议与**唯一出域实现**：`ChatRequest/ChatResponse`、`ToolDecl/ToolCall`、端点白名单策略 `EndpointPolicy`、HTTP 客户端 `Client`（`HTTPDoer` 注入）。框架内**唯一**具备出域能力的包：`ADR-012` 的 D1 出域符号等号基线 `== {model}`。 |
| `modelcfg/` | 模型配置三级兜底（P8 落地）：`Config{ID,Base,Model,Timeout}` + `Resolver.Resolve(explicit, project)`，顺序**只有一处**（显式>项目>平台），显式指错**不降级**；`Probe` 靠注入函数探活并逐份套自己的 `Timeout` |
| `security/` | 出域安全件：`Masker`（接口 + `applied` 证据）、永不外发 `Blacklist`（不可关闭）、不可信数据 `Fence`（三条通道闭集）、`Auditor`（接口，只落可交叉核验证据）、`EgressPolicy` 表。 |
| `audit/` | 行为审计**投递面**（P3 落地）：`Entry`（含派生 `Sanitized`/`Masked`）+ `Sink`（`SinkFunc`/`Nop`/`Recalling`/`RequireMasked`）+ `Fanout`（唯一投递路径：先 `security.Masker` 脱敏、再按注册顺序多播，脱敏失败 ⇒ 一个 sink 都不碰）；脱敏凭据是**未导出字段**，外部无从伪造"已脱敏"。 |
| `web/` | 可插拔 HTTP 入口（P4 落地，框架里**唯一**允许服务端 HTTP 符号的包）：静态路由表 + 中间件链（注册序=外层到内层）+ `ErrKind` 闭集（状态码与响应封套**均可注入覆盖**，ADR-009）+ `Server` 生命周期（`Serve`/`ServeAt`/`Shutdown`）+ **`Invoke` 进程内直调**（同一条链：中间件/上限/兜底全生效，不经网络）。`Handler` 返回 error 即走统一渲染；首次使用后路由表封存。 |
| `semantic/` | 前缀指令解析与实体/意图结构：`Entity/Candidate/Intent/Unsupported`、`EntityParser`（注入接口，识别不出 = 空类型 + nil error）。 + **`bind.go`：语义绑定**（意图+实体 → 同源工具的选择与参数装配；只选不执行，判据零重写） |
| `session/` | 会话记忆的**无状态装配点**（ADR-002）：`Assemble(Request{Handle, Clear, Deletes, Edits, NewItems, Used, Window})` 按固定顺序装配并回显句柄；纯函数（同请求重发同结果）、失败**不产生半成品**、未声明窗口**不虚报压缩** |
| `scene/` | 场景表（P7a 落地）：`Scene{ID,Label,Tools}` + `Registry.Resolve(id, role, *tools.Registry)` 按角色裁工具集，被裁工具逐条留原因（`unknown_tool`/`not_authorized` 闭集）；表里刻意不抄角色、不抄工具清单 |
| `playbook/` | 剧本（P7b 落地）：`Step{,Tool,Args,Compensation}` + `Validate/DryRun/Execute/IdemKey`；预演零写入、失败逆序补偿、撤销失败必须留事实；授权只在预演阶段过唯一谓词（不在撤销里判第二遍） |
| `tools/` | 工具注册表 + **`SchemaFor`**（结构体 → JSON-Schema，类型闭集 fail-closed；route↔tool 同源的 schema 支） 工具注册表与执行器：`Tool{Name,Params,Summary,RequiredRoles,Run}`、`Registry`（按角色裁剪）、`Runner`（errgroup 并行 / 步超时 / 未注册拒绝 / panic 隔离）。 |
| `agent/` | 对话循环与结果装配：`Loop`（maxSteps / 超时 / 降级 / 轨迹）、`Memory`（实体/结论/任务/偏好四类）、`Result`（溯源与事实-模型分离）。 |
| `kit/` | **装配根**（`ADR-011`，P1 落地）：`Plugin{Declare,Apply}` 两阶段契约、`Declarer`（Declare 相唯一手柄，**类型上拿不到 `*Kit`**）、`Deps()` 拓扑定序、`Build` 失败即逆序撤销且不留半初始化对象、`Stop` 幂等、`Tools()` 复用 `tools.Registry` 单一谓词、`Plugins()/ApplyOrder()` 内省。**零用户状态**（`ADR-002`）。 |
| `preset/` | **预制插件集**（P2.5 落地，框架的第一个使用者）：`Model`/`Perms`/`Tools`/`Loop` 四个 `kit.Plugin` 实现，把既有包变成"五步装起一个 agent"的积木；产物从调用方手里的插件实例取（无服务定位器）；`Loop.Run` 在 Build 前判 `ErrLoopNotAssembled`、`Stop` 后判 `ErrLoopStopped`。 |
| `im/` | IM 接入能力（**通用、留口**）：事件与会话模型、卡片与长文分段、去重/ACK、身份映射位 `Directory`、收发位 `Transport`、调后端位 `ResponderFunc`、卡片编码位 `CardCodec`（平台线格式归接入方，包内 `StandardCardCodec` 仅为参照）。包内不持凭据、不发 HTTP、不含任何平台的接口路径与字段名——平台形状由接入方实现这三个口子 |
| `engine/` | 最后一步的装配面：`New(Deps{Model,Perms,Tools,Loop,Also})` 四个必需注入点**构造期**判缺、引擎自身作为一个 `kit.Plugin` 在 `Apply` 期挂停止钩子，`Handle(ctx, 问题)` 一句跑完整条链 |
| `authz/` | 权限点**声明与值域**（P2 落地，不含任何授权判定）：`RoleSet`（角色值域由宿主**注入**，框架内零角色字面量，AST 断言钉住）、`Scope`（`platform｜project`）、`Enforcement`（`direct｜funnel:<key>` 文法 + 往返）、`Registry`（有序 + 唯一键 + 空默认角色一律拒绝）。未知权限点 ⇒ **报错而非返回空角色**（空 `RequiredRoles` 在 `tools.allows` 里等于"公开"，返回空就是 fail-open）。 |

## 2.1 最小可用样例（框架不是只能被读，是可被跑）

```bash
go test -race ./本包/preset/ ./本包/web/    # 仓内端到端判据（含真监听起停、HTTP 往返、模型直调接口）
# 想要一个能 go run 的程序：把样例模板放进你自己的 module
#   本包/samples/agentkit-demo.go.txt → cmd/agentkit-demo/main.go
```

样例模板与 `preset/e2e_test.go` 一起构成框架的**第一个真实使用者**。
⚠️ 为什么仓内不放 `cmd/agentkit-demo/`：宿主仓库的全仓扫描约定，`model.NewClient`/`model.ClientConfig` 这类模型请求构造点只能出现在 `{宿主既有实现/llm/**, 本包/model/**}`（AC-2.1-1 单一出口）——
   样例程序自己构造客户端就是绕出口。**给守卫加豁免等于放宽不变量**，所以样例改以 `.go.txt` 模板存放（宿主构建与守卫都不看它），在陌生 module 里实测 `go run`；仓内的等价断言放 `_test.go`（两个守卫都豁免测试文件）。
`authz.NewRoleSet` 注入角色值域 → `preset.NewPerms` 声明权限点 → `preset.NewTools(preset.Granted(tool, perm))`
把授权角色**从权限点生成** → `preset.NewModel(宿主构造的 *model.Client, endpoint)` → `preset.NewLoop(model)` →
`kit.New().Use(...).Build()`。demo 故意先 `Use(loop)` 再 `Use(model)`：`Deps()` 让 Build 把 model 排到前面。
第二轮换角色 `auditor`，模型可见工具从 `[user.profile whoami]` 变成 `[whoami]` —— 同一条声明、只有一个授权谓词，
这就是"接口/工具/权限同源"的可运行证据。

## 2.2 深入文档（随包走，取走即用）

| 文件 | 回答什么 |
|---|---|
| [`docs/architecture.md`](docs/architecture.md) | 有哪些包、谁允许依赖谁、出/入域红线、一次请求的真实路径、生命周期 |
| [`docs/design.md`](docs/design.md) | 为什么是这个形状：7 条不变量、关键决策与代价、失败语义表、**刻意拒绝的抽象**、测试纪律 |
| [`docs/guide.md`](docs/guide.md) | 怎么用：五句起服务、逐项扩展、常见报错定位、别做的事 |
| [产品设计 + 界面原型](../.本包/docs/.specs/agent-engine/PRODUCT-DESIGN.html) | 九段制单文件 HTML（断网可开）；状态**起草中**，待人工逐章过审 |

> 这些文档刻意**不抄写会漂移的数字**（行数/包数/用例数一律用 §4 的命令现取），也不重述已在代码里的不变量。

## 3. 依赖口径（两条，必须同时成立）

1. **只依赖标准库**，外加**唯一**已登记第三方 `golang.org/x/sync/errgroup`（ADR-004 / D13：
   宿主 `go.mod` 里已有 `golang.org/x/sync v0.8.0 // indirect`，**不新增** module 依赖）。
   白名单**按 package 精确匹配**：同 module 的其它包（如 `golang.org/x/sync/semaphore`）**不允许**。
2. **框架内部各包可以互相 import**（`tools`/`agent` 用 `model`、`kitctx`），
   但**不得依赖业务代码**：任何 import 不得落在宿主 module 前缀之下，**除非**该路径位于
   **框架自己的子树内**（仓库里是 `<module>/本包/`）。
   ⇒ 禁止 `logic/**`、`controllers/**`、`pkg/**`、`config/**`、`errorx/**`、
   `middlewares/**` 等一切宿主业务实现。
   判定按**路径段**判断：`.../本包foo` 这类相似前缀**不算**框架子树（朴素 `HasPrefix` 会误放行）。

以上两条 + **单一出口基线**（`net/http` 按**符号方向**判定，`ADR-012` 取代原「按路径」判据）由
`guard_test.go` 机械固化：

- **D1 出域符号**（`Client`/`NewRequest*`/`Transport`/`Get`/`Post`/`httputil.ReverseProxy` …）
  所在包集合 **`== {model}`（等号，比原 `⊆` 更严）** 且非空；
- **D2 入域符号**（`Server`/`Handler*`/`ResponseWriter` …）所在包集合 **`⊆ {web}`**
  （web 层落地前该档无上界压力，其牙齿由分类器合成用例表承担，并如实打印）；
- **D3 中立符号**（`Request`/`Response`/`Header`/`Status*`/`Method*` …）**`⊆ {model, web}`** —— 中立 ≠ 自由；
- **D4 未归类符号一律判红**（新增 `net/http` 用量必须显式归类，禁止靠"再放开一个包"消红）；
- **旁路封堵**：非 `model/` 包引用 `HTTPDoer`，或函数/方法**返回** `*http.Response` ⇒ 判红
  （拿注入的 Doer 自己发请求不需要出现任何 D1 符号，旧判据对此完全无感）。

各档违规**全量收集后统一报红**（一处红不得掩盖其余判据 —— L-096）。

```bash
go test -race -count=1 ./本包/... -run TestGuard
python3 本包/tools/agentkit_code_pins.py                      # 本仓门禁：钉定=框架代码全量（文档不钉）
python3 本包/tools/portability_check.py        陌生 module 取走实验（判据性，~17s）
```

守卫**不写死宿主 module 路径**：它自底向上找最近的 `go.mod` 读 `module` 行，并以
**框架自身所在目录**推导豁免子树 ⇒ 整目录抽取到别的 module 后守卫自动适配。

## 4. 抽取步骤（可复制执行，已实测）

```bash
# 0) 前置：目标 module 需已有 golang.org/x/sync（errgroup 的 module）。
#    若没有：
go get golang.org/x/sync@v0.8.0     # 或 go mod tidy

# 1) 复制整个框架目录
cp -r 本包 <your/module>/agentkit

# 2) 重写 import 前缀
sed -i 's#<your-module>/本包#<your/module>/agentkit#g' $(find <your/module>/agentkit -name '*.go')

# 3) 验证（完整跑测：包数随交付增长，以命令输出为准（当前 `go test -race ./本包/...` 全绿））
go build ./agentkit/... && go test ./agentkit/...
```

实测记录（在 `/tmp` 用一个新 module `example.com/myapp` 走完整流程，仓库内零残留）：

- 步骤 2 只重写 **import 路径**：实测 **import 语句中的宿主前缀残留 = `0`**
  （用 `go/parser` 对 40 个 `.go` 文件真解析逐条核对）。
  注释里仍可能出现原宿主 module 字面（如「不得依赖 `...<宿主 module>/...`」这类说明性文字），
  纯属文档措辞，不影响构建，也不影响守卫（守卫只解析 import）。
- `go build ./agentkit/...` **通过**（exit=0）。
- 完整 `go test ./agentkit/...`（**不带 `-run` 过滤**）**13 个包全绿**（exit=0）：根守卫 +
  `agent` / `im` / `kitctx` / `model` / `security` / `semantic` / `tools`。
  ⇒ **「拿走就能用」在整棵子树（含 `tools` 包）上成立**。
- `go test ./agentkit/... -run TestGuard` 同样全绿（框架守卫在抽取后自动适配）。
- 抽取实测仍存在的**多余项**（README 显式列出，避免下游踩坑）：
  1. **errgroup 的 module 必须存在于下游 `go.mod`/`go.sum`**（`golang.org/x/sync v0.8.0`）；
     否则 `go build` 报
     `no required module provides package golang.org/x/sync/errgroup; to add it: go get golang.org/x/sync/errgroup`
     （已实测复现）。框架本身**不含** `go.mod`。
  2. ~~T11 包内守卫硬编码宿主 layout，抽取后 `tools` 包 FAIL~~ —— **该多余项已消除，不再是多余项**：
     `本包/tools/registry_test.go` 那份硬编码宿主 layout 的 `TestTools_ImportsStayInsideAgentkit`
     与本目录 `guard_test.go` 是同一判定式的重复实现（R3），且已被证明抽取后必然失效，**已删除**
     （`registry.go` 零改动）。守卫职责此后由 `guard_test.go` 单一承担，
     抽取场景的完整 `go test ./agentkit/...` 18 包全绿（P4 web 入口落地后再次实测复验，见上一段）。

## 4.1 性能基准（当场可跑，不是估的）

```bash
go test -run '^$' -bench Assemble          -benchmem -count=3 ./本包/session/
go test -run '^$' -bench LookupAuthorized  -benchmem -count=3 ./本包/tools/
```

读法在 `docs/architecture.md` §6：装配 **≈15–25µs/条、线性** ⇒ 单请求记忆建议 ≤1000 条（守 200ms）；
唯一授权谓词 **≈87ns/次、0 分配、与表规模无关** ⇒ 治理税不是瓶颈，别加缓存层。端到端 P95/QPS **未测**（只有单点基准）。

## 5. 禁止事项

- **禁止**把框架做成自带 `go.mod` 的独立子模块（`L-008`：上一轮正因此被全部还原）。
  它必须是宿主 module 内的**普通包**，靠「整目录复制 + 一次前缀重写」搬运。
- **禁止**在框架内 import 宿主任何业务包；**禁止**新增第三方依赖（白名单只有 errgroup）。
- **禁止**在 `model/` 之外新增出网/模型请求构造点（单一出口基线，`AC-2.1-1`）。
  该基线的机械判据已按 `ADR-012` 升级为**符号方向四档**（D1 出域 `== {model}` 等号 / D2 入域 `⊆ {web}` /
  D3 中立 `⊆ {model, web}` / D4 未归类即拒），并额外封堵「拿注入的 Doer 自行发请求」这条旁路。
