# agentkit 使用手册（随包文档 3/3）

> 所有签名都对照仓库实测（`go vet` + 样例编译）。**权威可运行版本**是
> `pkg/agentkit/samples/agentkit-demo.go.txt`（十段自校验、在陌生 module 里 `go run` 过）；
> 本文给形状与决策，样例给能跑的代码。两者不一致时以样例与编译器为准。

## 0. 前置认知（三句话）

1. 你写的是**普通 web 程序**：路由、handler、结构体入参，零 agent 概念。
2. 每个接口**声明一次**即同时成为：对外 HTTP 路由 + agent 可见工具 + 权限点。
3. 框架**什么都不猜**：角色、预算、traceid、上下文窗口、记忆、模型端点，全部由你在 `kitctx.RequestContext` 里显式带进来；漏配会被当场拒（而不是"看起来能跑"）。

## 1. 五句话起一个受治理的 agent 服务

```go
// ① 权限面：注入的是「值域」，不是校验规则
roles, _ := authz.NewRoleSet("support", "auditor", "admin")
perms, _ := preset.NewPerms(roles, authz.PermissionPoint{
    Key: "user.profile.read", Label: "按 IP 查档案",
    Scope: authz.ScopeProject, Enforced: authz.DirectEnforcement(),
    DefaultRoles: []kitctx.Role{"support", "admin"},
})

// ② 模型出口：唯一能发起 HTTP 的地方（策略 + 传输都是注入件，可换真客户端）
policy := model.NewEndpointPolicy(model.EndpointPolicyConfig{AllowedHosts: []string{"192.168.16.16"}})
client := model.NewClient(model.ClientConfig{Doer: httpClient, Policy: policy,
                                             Timeout: 2 * time.Second, MaxAttempts: 1})
m, _ := preset.NewModel(client, "http://192.168.16.16:8080/v1/chat/completions")

// ③ 接口即工具：路由 / 工具 / 权限点同源（params 与所需权限由入参结构体反推）
decl := preset.NewTools(perms,
    preset.Served(web.Route{Method: "POST", Path: "/api/user/profile", Summary: "按 IP 查档案"},
                  profileReq{}, "user.profile.read", queryProfile),
    preset.Direct(tools.Tool{Name: "user.note", Summary: "给用户加备注",
                             RequiredRoles: []string{"admin"}, Run: changeTag}),
)

// ④ 循环 + 总装
loop := preset.NewLoop(m)
e, err := engine.New(engine.Deps{Model: m, Perms: perms, Tools: decl, Loop: loop,
                                  Config: kit.Config{Logger: myLogger},
                                  Also: []kit.Plugin{auditPlugin, serverPlugin}})
defer e.Stop()

// ⑤ 一次请求（身份与预算显式携带）
ctx := kitctx.WithRequest(r.Context(), kitctx.RequestContext{
    UserID: uid, Role: role, TraceID: traceID,
    Limits: kitctx.Limits{MaxSteps: 5, StepTimeout: 3 * time.Second},
})
out, err := e.Handle(ctx, "198.51.100.23 的档案是什么")
// out.Steps（轨迹）· out.Verdict · out.Rejected · out.StepsLimited/out.Degraded + out.Notice
```

`engine.Deps` 的 `Model / Perms / Tools / Loop` **四个都是必填**，少任一个在 `New` 期就报聚合错误（一次看全，不用一轮试一个）。

## 2. 想扩什么，碰哪里

| 目标 | 做法 | 注意点 |
|---|---|---|
| 加一个能力 | 再 `preset.Served(...)` 或 `preset.Direct(...)` | 声明期即校验权限点存在；装配后重复登记 ⇒ `ErrLateDeclare` |
| 只对内开放、不开 HTTP | `preset.Direct` | 反之想"只对外服务、不给模型看"就别登记成工具 |
| 新工具形态（缓存/并发/重试/熔断） | 自己实现 `tools.Registry` 的包装件（框架侧刻意留的扩展位） | 授权判定仍只能问 `LookupAuthorized` |
| 加场景（业务想固定"这类问题用哪几个工具"） | `scene.NewRegistry(scene.Scene{ID, Label, Tools})` → `preset.NewScenes(reg)` → `scs.Resolve(ctx, id)` | 场景**不抄角色、不抄工具清单**；引用不存在的工具 ⇒ `Build` 期 `ErrScenesToolsMissing`（配置漂移在装配根失败，不是运行期才 404） |
| 加剧本（可逆流程：下发→失败撤销） | `playbook.New(id, label, playbook.Step{Name, Tool, Args, Compensation *Step})` → `Validate(role, reg)` / `DryRun` / `Execute(ctx, role, reg, log)` | 预演有错 ⇒ **零写入**；执行失败 ⇒ **逆序补偿**，补偿自己坏 ⇒ best-effort 继续 + 记 `CompensationProblems` |
| 加语义路由规则 | `preset.NewPlanner(semantic.Rule{Intent, Tool, FromEntity, Const})` | 计划里的 `Params`/`RequiredRoles` 从同一份声明反推 ⇒ 不可能与对外接口漂移；越权候选自动 `not_authorized` 留痕 |
| 换模型 / 加厂商 / 多环境 | `modelcfg.New(cfgs, modelcfg.PlatformDefault(id), modelcfg.ProjectBinding(pid, cid))` → `.Resolve(explicitID, projectID)` | 优先级**唯一**：显式 > 项目 > 平台；`Timeout<=0` 直接拒；健康检查靠注入 `Probe(ctx, func(ctx, cfg) error)`（本包不认识任何厂商协议） |
| 加审计出口 | `audit.New(masker, clock, sinks...)`（扇出）→ `preset.NewAudit(fan)` | 投递前**强制过 Masker**，出域前已脱敏，扇出里再脱一次是有意的 |
| 不用 agent，只当 web 框架 | `rt := web.NewRouter(web.Config{...})` → `rt.Register(...)` → `srv, _ := web.NewServer(web.ServerConfig{Router: rt, Addr: ":8080"})` | `web.Config.Authorize` 没挂会**告警**（声明了权限点却没人判 = 假安全感）；纯 handler 形态没有权限点 ⇒ 不告警也不保护 |
| 带会话记忆（多轮） | `session.Assemble(session.Request{Handle, Clear, Deletes, Edits, NewItems, Used, Window})` → 把 `Assembly.Handle` 回显给调用方，下次原样带回 | 顺序固定 `Clear→Deletes→Edits→NewItems→Compress`；矛盾指令（清空后又按 ID 删）显式报 `agent.ErrItemNotFound`；失败**不返回半成品**；`Window<=0` ⇒ 不压缩也不虚报 |

## 3. 常见报错 → 定位

| 报错 | 含义 | 怎么修 |
|---|---|---|
| `engine: 未注入…`（聚合） | 四必需注入点缺项 | 补齐；只想少一个能力就不要用 `engine`，直接 `kit.Use` |
| `kit: 尚未 Build…OnStop 只能在插件 Apply 期间调用` | 在装配完成后挂钩子 | 把该逻辑做成插件，在 `Apply` 里 `k.OnStop(...)`（`engine` 就是这么写的） |
| `ErrLateDeclare` | Build 之后又登记 | 登记全放 `Declare` 期 |
| `authz: Scope 取值不在闭集内` / `Enforced 写法不合法` | 权限点漏填必填字段 | 无默认值是有意的；显式给 `Scope` + `Enforced` |
| `agent.trace_id.required` | 没带 traceid | 没有可回放轨迹就不许进循环；`RequestContext.TraceID` 必填 |
| `session: 恢复请求携带的记忆句柄…` | 载荷坏 | **不要**吞成"新会话"，那是把坏请求伪装成清空 |
| `preset: ErrScenesToolsMissing：[...]` | 场景引用了不存在的工具 | 通常是删了接口忘了改场景 —— 这正是它该在装配期失败的原因 |
| 运行期 403/401 与 `Rejected` | 权限判定 | 只有一条谓词：`LookupAuthorized(name, role)`；确认角色是**已判定角色**（空角色不放行） |
| `out.Notice` 出现"步数/降级"文案 | 预算耗尽或模型不可用 | 读 `StepsLimited` / `Degraded` 两个布尔，文案是冻结的，别改字 |

## 4. 验证你自己接得对不对

```bash
go test -race -count=1 ./pkg/agentkit/...                     # 框架自身
python3 pkg/agentkit/tools/portability_check.py         # 拿到别处还能编译/测试/运行
python3 .specs/agent-engine/tools/gate_report.py --verify      # 门禁读数与工件是否一致
```

在你自己的程序里，至少断言这四件事（都是这个框架的既有测试形状，白抄即可）：
① 换角色 ⇒ 可见工具**只多少不爽**；② 越权候选被裁且**留痕**（不是静默少一项）；
③ 空角色的调用**拿不到任何东西**；④ `Stop()` 之后不再有新的写副作用与出域。

## 5. 别做的事

- 别在 `scene` / `playbook` / 你自己的中间件里比较角色字符串 —— 那条谓词只有一处。
- 别给 `Limits` / `Window` / 角色 补默认值 —— 缺就让它拒。
- 别绕过 `model` 直接发 HTTP（门禁的出域基线是**等号**，多一个包就红）。
- 别把 `Memory` / `Steps` 这类活对象整份 `JSON.stringify` 打日志 —— 只取需要上报的叶子字段（既有教训）。
- 别在文档里抄写"多少行多少用例"这类会漂移的数字 —— 用第 0 节的命令现取。
