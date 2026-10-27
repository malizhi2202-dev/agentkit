// Package preset 是预制插件集：把既有能力包逐个包成 kit.Plugin，
// 让一个程序能用「构造 → kit.New → Use → Build → 取产物」五步把 agent 装起来。
//
// 这个包存在的唯一理由是**证明框架能被真的用起来**：只有 `kit` 而没有 preset，
// 装配根就只是"一个能跑自己测试的容器"（沉淀完成 ≠ 可复用）。
//
// 三条形状约束（都是被别的判据逼出来的，不是审美）：
//
//  1. **本包不出现 `model.HTTPDoer`，也不 import `net/http`**。`agentkit/guard_test.go`
//     的旁路判据规定：非出域包一旦引用出域传输类型就具备"绕开唯一出口自行发请求"的能力。
//     所以出域客户端必须由**宿主程序**构造好后交进来（`NewModel(client, endpoint)`）——
//     恰好也是 「框架不读配置」的落点：配置在宿主，框架只接受成品。
//  2. **不引入服务定位器**。插件实例由调用方构造，产物就从**调用方手里的那个实例**取
//     （`m.Client` / `p.Registry` / `l.Run(...)`）。框架侧只保留已有的类型化扩展点
//     （`Declarer.Tools`）。字符串键的 `Get("model")` 会把编译期能抓到的错推到运行期。
//  3. **跨插件的数据流靠显式依赖，不靠注册顺序的巧合**。`Loop` 读 `Model` 的产物，
//     于是它声明 `Deps = [model 的 Name]` ⇒ 忘了把 Model 交给 Use，Build 直接判
//     `ErrMissingDependency`；顺序错了也不可能出现"读到未就绪客户端"。
//     反过来，`Perms` 把校验放在**构造期**（纯声明校验、不碰 Kit），所以 `Tools` 绑权限点时
//     不需要依赖它 —— 少一条顺序耦合。
package preset

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"
	"sync"

	"github.com/malizhi2202-dev/agentkit/agent"
	"github.com/malizhi2202-dev/agentkit/audit"
	"github.com/malizhi2202-dev/agentkit/authz"
	"github.com/malizhi2202-dev/agentkit/kit"
	"github.com/malizhi2202-dev/agentkit/kitctx"
	"github.com/malizhi2202-dev/agentkit/model"
	"github.com/malizhi2202-dev/agentkit/semantic"
	"github.com/malizhi2202-dev/agentkit/tools"
	"github.com/malizhi2202-dev/agentkit/web"
)

// 预制插件的默认名字（宿主可另起，Deps 里引用的是实例的 Name，不受常量影响）。
const (
	NameModel  = "preset.model"
	NamePerms  = "preset.perms"
	NameTools  = "preset.tools"
	NameLoop   = "preset.loop"
	NameAudit  = "preset.audit"
	NameServer = "preset.server"
)

// 装配期可识别错误。
var (
	// ErrModelClientRequired 表示没交出域客户端：框架不替你造（配置与凭据都在宿主）。
	ErrModelClientRequired = errors.New("preset: 必须注入已构造的 *model.Client")
	// ErrModelEndpointRequired 表示端点为空。**装配期**就拒绝，而不是运行期悄悄降级；
	// `agent.Loop` 自身的"端点为空 ⇒ 降级"行为不变（那是运行时策略，本包只是把装配错误提前）。
	ErrModelEndpointRequired = errors.New("preset: 模型端点不能为空")
	// ErrPermsRolesRequired 表示没有角色闭集（authz 的前置条件）。
	ErrPermsRolesRequired = errors.New("preset: 必须注入角色闭集 authz.RoleSet")
	// ErrPermsNil 表示工具绑了权限点却没有提供权限表。
	ErrPermsNil = errors.New("preset: 工具绑定权限点时必须提供 *Perms（否则角色集合无处可解析）")
	// ErrRequiredRolesConflict 表示同一条工具既写了 RequiredRoles 又绑了权限点。
	ErrRequiredRolesConflict = errors.New("preset: 授权角色只能有一个来源（手填 RequiredRoles 或绑定权限点）")
	// ErrModelNotReady 表示 Model 插件尚未接线就被 Loop 使用。
	ErrModelNotReady = errors.New("preset: 模型插件未就绪（Deps 应保证先 Apply）")
	// ErrLoopNotAssembled 表示 Build 之前调用循环。
	ErrLoopNotAssembled = errors.New("preset: 循环未装配（需先 kit.Build）")
	// ErrAuditFanoutRequired 表示没有注入审计投递器。
	ErrAuditFanoutRequired = errors.New("preset: 必须注入已构造的 *audit.Fanout")
	// ErrAuditNotReady 表示 Kit 未 Build 就想发审计。
	ErrAuditNotReady = errors.New("preset: 审计投递器未就绪（需先 kit.Build）")
	// ErrAuditStopped 表示 Kit 已停止 ⇒ 审计产物不再可用（与 Loop 同一撤销口径）。
	ErrAuditStopped = errors.New("preset: 装配根已停止，审计不可再投递")
	// ErrServerRequired 表示没有注入 web.Server。
	ErrServerRequired = errors.New("preset: 必须注入已构造的 *web.Server")
	// ErrServerNotReady 表示 Kit 未 Build 就想用服务器。
	ErrServerNotReady = errors.New("preset: 服务器未就绪（需先 kit.Build）")
	// ErrServerStopped 表示 Kit 已停止 ⇒ 不得再起服务。
	ErrServerStopped = errors.New("preset: 装配根已停止，服务不可再启动")
	// ErrWebRouterRequired 表示登记「路由即工具」却没给 `kit.Config.Web`。
	ErrWebRouterRequired = errors.New("preset: 未配置路由表（kit.Config.Web），同源声明拒绝半张表")
	// ErrServedPermRequired 表示同源声明没绑定权限点。
	ErrServedPermRequired = errors.New("preset: 路由即工具必须绑定权限点（否则它对所有角色可见）")
	// ErrServedNameRequired 表示同源声明缺少稳定名称（进程内直调按它寻址）。
	ErrServedNameRequired = errors.New("preset: 路由即工具必须有 Route.Name")
	// ErrServedPublicRoute 表示试图把公开（免身份）接口做成工具。
	ErrServedPublicRoute = errors.New("preset: 公开接口没有使用者身份，不能成为 agent 工具")
	// ErrServedHandlerRequired 表示同源声明缺少处理函数。
	ErrServedHandlerRequired = errors.New("preset: 路由即工具必须给出 web.Handler")
	// ErrServedPermMismatch 表示 Route.Perm 与 Served 的 perm 不是同一个键。
	ErrServedPermMismatch = errors.New("preset: Route.Perm 与权限点参数不一致")
	// ErrPlannerNotAssembled 表示 Build 之前使用语义绑定器（它需要 Kit 的那张唯一工具表）。
	ErrPlannerNotAssembled = errors.New("preset: 语义绑定器未装配（需先 kit.Build）")
	// ErrPlannerStopped 表示 Kit 已停止 ⇒ 不再产出新计划（停止后仍能量就等于撤销不彻底）。
	ErrPlannerStopped = errors.New("preset: 装配根已停止，语义绑定器不再产出计划")
	// ErrPlannerRulesRequired 表示零值构造的绑定器没有规则（必须走 NewPlanner）。
	ErrPlannerRulesRequired = errors.New("preset: 语义绑定器没有规则（请通过 preset.NewPlanner 构造）")
	// ErrServedArgsMissing 表示模型给的参数缺 schema 必填字段（校验发生在调用之前）。
	ErrServedArgsMissing = errors.New("preset: 参数缺少 schema 必填字段")
)

// ---- Model：已构造的出域客户端 + 端点，交给 Kit 管生命周期 ----

// Model 只承载"模型出口已就绪"这一事实。它不构造客户端（见包注释第 1 条）。
type Model struct {
	name     string
	client   *model.Client
	endpoint string
	ready    bool
}

// NewModel 用宿主构造好的客户端与端点建插件。空端点直接判失败（装配期报错优于运行期降级）。
func NewModel(client *model.Client, endpoint string) (*Model, error) {
	if client == nil {
		return nil, ErrModelClientRequired
	}
	if strings.TrimSpace(endpoint) == "" {
		return nil, ErrModelEndpointRequired
	}
	return &Model{name: NameModel, client: client, endpoint: endpoint}, nil
}

// Name 返回插件名。
func (m *Model) Name() string {
	if m == nil || m.name == "" {
		return NameModel
	}
	return m.name
}

// Declare 无需登记任何扩展点（模型出口不是声明式能力）。
func (m *Model) Declare(*kit.Declarer) error { return nil }

// Apply 确认客户端可用并置就绪位（产物只在这之后可见）。
func (m *Model) Apply(k *kit.Kit) error {
	if m.client == nil {
		return ErrModelClientRequired
	}
	m.ready = true
	k.Logger().Info("preset: 模型出口就绪", map[string]any{"endpoint": m.endpoint})
	return nil
}

// Describe 供管理面内省。
func (m *Model) Describe() kit.Info {
	return kit.Info{Name: m.Name(), Version: "preset", Notes: map[string]string{"endpoint": m.endpoint, "ready": fmt.Sprint(m.ready)}}
}

// Client 返回生效客户端；未就绪时为 nil（配合 ErrModelNotReady 使用，不返回半成品）。
func (m *Model) Client() *model.Client {
	if m == nil || !m.ready {
		return nil
	}
	return m.client
}

// Endpoint 返回本次出域端点。
func (m *Model) Endpoint() string {
	if m == nil {
		return ""
	}
	return m.endpoint
}

// ---- Perms：权限表（校验在构造期闭合） ----

// Perms 持有一张已校验的 authz 注册表。
//
// 为什么在构造函数里就建表：那是**纯声明校验**，不碰 Kit，因此与 Apply 顺序无关；
// 于是绑权限点的 Tools 插件不需要声明对它 depends-on（少一条顺序耦合）。
type Perms struct {
	name   string
	reg    *authz.Registry
	ready  bool
	points int
}

// NewPerms 建权限表。角色闭集必须注入；任何一条权限点不合法 ⇒ 构造失败（不落半成品）。
func NewPerms(roles *authz.RoleSet, points ...authz.PermissionPoint) (*Perms, error) {
	if roles == nil {
		return nil, ErrPermsRolesRequired
	}
	reg, err := authz.NewRegistry(roles, points...)
	if err != nil {
		return nil, err
	}
	return &Perms{name: NamePerms, reg: reg, points: reg.Len()}, nil
}

// Name 返回插件名。
func (p *Perms) Name() string {
	if p == nil || p.name == "" {
		return NamePerms
	}
	return p.name
}

// Declare 不登记 Kit 扩展点：权限点在构造期已闭合（P4/P5 有 web 扩展点时再一起接）。
func (p *Perms) Declare(*kit.Declarer) error { return nil }

// Apply 置就绪位。
func (p *Perms) Apply(k *kit.Kit) error {
	if p.reg == nil {
		return ErrPermsRolesRequired
	}
	p.ready = true
	k.Logger().Info("preset: 权限表就绪", map[string]any{"points": p.points, "roles": p.reg.Roles().Len()})
	return nil
}

// Describe 供管理面内省。
func (p *Perms) Describe() kit.Info {
	return kit.Info{Name: p.Name(), Version: "preset", Notes: map[string]string{"points": fmt.Sprint(p.points)}}
}

// Registry 返回权限表；未就绪时为 nil。
func (p *Perms) Registry() *authz.Registry {
	if p == nil || !p.ready {
		return nil
	}
	return p.reg
}

// ---- Tools：工具声明（可绑定权限点 ⇒ 授权角色同源生成） ----

// ToolSource 是一条工具声明来源：Direct 手填角色，Granted 由权限点**生成**角色，
// Served 则是"一条 HTTP 接口同时就是工具"（路由、参数 schema、权限点三者同源）。
type ToolSource struct {
	tool   tools.Tool
	perm   string
	served *servedSpec
}

// servedSpec 是一条同源声明的全部事实。它刻意不预生成 tools.Tool：
// Run 需要路由表才能直调，而路由表要到 Declare 相才从 `Declarer.Web` 拿到。
type servedSpec struct {
	route    web.Route
	schema   map[string]any
	required []string
	handler  web.Handler
}

// Direct 原样登记（授权角色由 tool.RequiredRoles 自己声明）。
func Direct(t tools.Tool) ToolSource { return ToolSource{tool: t} }

// Granted 登记并把授权角色交给权限点：接口/工具/权限三者的角色集合出自同一份声明（框架裁决 规则 5）。
func Granted(t tools.Tool, perm string) ToolSource { return ToolSource{tool: t, perm: perm} }

// Served 声明"一条 HTTP 接口同时是一个 agent 工具"，二者**同源**：
//
//   - 名字：`Route.Name` 既是直调寻址键，也是工具名（不允许两个名字互相指不同东西）；
//   - 参数 schema：由 `sample`（处理函数真正解码的那个结构体）生成 ⇒ 形状不可能与接口漂移；
//   - 授权角色：由 `perm` 指向的权限点生成 ⇒ 与 `Granted` 同一处解析；
//   - 执行：走 `Router.Invoke` 的同一条处理链（中间件、请求体上限、panic 兜底、错误渲染全生效），
//     不经过网络，也不重复实现一遍业务。
//
// 约束：必须有 `perm`（对所有角色公开的工具等于绕过授权裁剪）；不能是 `Public` 路由
// （免身份接口没有使用者身份可传递给取数逻辑）；`Route.Perm` 若已填必须与 `perm` 相同。
func Served(route web.Route, sample any, perm string, h web.Handler) (ToolSource, error) {
	name := strings.TrimSpace(route.Name)
	if name == "" {
		return ToolSource{}, ErrServedNameRequired
	}
	if strings.TrimSpace(perm) == "" {
		return ToolSource{}, fmt.Errorf("%w：%s", ErrServedPermRequired, name)
	}
	if route.Public {
		return ToolSource{}, fmt.Errorf("%w：%s", ErrServedPublicRoute, name)
	}
	if h == nil {
		return ToolSource{}, fmt.Errorf("%w：%s", ErrServedHandlerRequired, name)
	}
	if route.Perm != "" && route.Perm != perm {
		return ToolSource{}, fmt.Errorf("%w：%s 声明 %q，实得 %q", ErrServedPermMismatch, name, route.Perm, perm)
	}
	schema, err := tools.SchemaFor(sample)
	if err != nil {
		return ToolSource{}, fmt.Errorf("preset: 同源声明 %q 的参数 schema 生成失败：%w", name, err)
	}
	route.Name, route.Perm = name, strings.TrimSpace(perm)
	if strings.TrimSpace(route.Summary) == "" {
		route.Summary = fmt.Sprintf("HTTP %s %s", route.Method, route.Path)
	}
	return ToolSource{perm: route.Perm, served: &servedSpec{
		route:    route,
		schema:   schema,
		required: tools.RequiredFromSchema(schema),
		handler:  h,
	}}, nil
}

// Tools 把一组工具登记进 Kit 的唯一工具扩展点。
type Tools struct {
	name     string
	perms    *Perms
	sources  []ToolSource
	register int
}

// NewTools 构造工具插件。perms 可为 nil，但那时所有来源必须是 Direct。
func NewTools(perms *Perms, sources ...ToolSource) *Tools {
	return &Tools{name: NameTools, perms: perms, sources: sources}
}

// Name 返回插件名。
func (ts *Tools) Name() string {
	if ts == nil || ts.name == "" {
		return NameTools
	}
	return ts.name
}

// Declare 把每条声明登记进唯一工具注册表；绑定权限点的角色集合在此解析。
//
// 权限点未知 ⇒ **整条声明失败**，绝不"给个空角色继续注册"：
// `tools.allows` 把空 `RequiredRoles` 解释为"任意已判定角色可用"，放过就是 fail-open。
func (ts *Tools) Declare(d *kit.Declarer) error {
	for _, s := range ts.sources {
		if s.served != nil {
			if err := ts.declareServed(d, s.served); err != nil {
				return err
			}
			ts.register++
			continue
		}
		t := s.tool
		if s.perm == "" {
			if err := d.Tools().Register(t); err != nil {
				return fmt.Errorf("preset: 工具 %q 登记失败：%w", t.Name, err)
			}
			ts.register++
			continue
		}
		if ts.perms == nil {
			return fmt.Errorf("%w：工具 %q 绑定了 %q", ErrPermsNil, t.Name, s.perm)
		}
		if len(t.RequiredRoles) > 0 {
			return fmt.Errorf("%w：工具 %q 同时写了 RequiredRoles=%v 与权限点 %q",
				ErrRequiredRolesConflict, t.Name, t.RequiredRoles, s.perm)
		}
		reg := ts.perms.Registry()
		if reg == nil {
			// Apply 尚未发生（权限表未就绪）：改用构造期那张表，不为此引入顺序耦合。
			reg = ts.perms.reg
		}
		roles, err := reg.RequiredRolesFor(s.perm)
		if err != nil {
			return fmt.Errorf("preset: 工具 %q 的权限点解析失败：%w", t.Name, err)
		}
		t.RequiredRoles = roles
		if err := d.Tools().Register(t); err != nil {
			return fmt.Errorf("preset: 工具 %q 登记失败：%w", t.Name, err)
		}
		ts.register++
	}
	return nil
}

// declareServed 把一条同源声明落成两件事：路由表里的一条路由 + 工具表里的一个工具。
//
// 顺序刻意是"先查工具名冲突、再登记路由"：反过来的话，工具名冲突会让路由表里
// 留下一条没有任何工具对应的接口 —— 正是本切片要消灭的"两张表各握半套"。
func (ts *Tools) declareServed(d *kit.Declarer, spec *servedSpec) error {
	rt := d.Web()
	if rt == nil {
		return fmt.Errorf("%w：%s %s（%s）", ErrWebRouterRequired, spec.route.Method, spec.route.Path, spec.route.Name)
	}
	if ts.perms == nil {
		return fmt.Errorf("%w：路由即工具 %q 绑定了 %q", ErrPermsNil, spec.route.Name, spec.route.Perm)
	}
	if _, exists := d.Tools().Lookup(spec.route.Name); exists {
		return fmt.Errorf("preset: 工具名 %q 已被占用，路由 %s %s 不再登记（避免两张表不一致）",
			spec.route.Name, spec.route.Method, spec.route.Path)
	}
	reg := ts.perms.Registry()
	if reg == nil {
		reg = ts.perms.reg // Apply 尚未发生：用构造期那张表，不引入顺序耦合（与 Granted 同一路径）
	}
	roles, err := reg.RequiredRolesFor(spec.route.Perm)
	if err != nil {
		return fmt.Errorf("preset: 同源声明 %q 的权限点解析失败：%w", spec.route.Name, err)
	}
	if err := rt.Register(spec.route, spec.handler); err != nil {
		return fmt.Errorf("preset: 路由 %s %s 登记失败：%w", spec.route.Method, spec.route.Path, err)
	}
	tool := tools.Tool{
		Name:          spec.route.Name,
		Summary:       spec.route.Summary,
		Params:        spec.schema,
		RequiredRoles: roles,
		Run:           servedRun(rt, spec),
	}
	if err := d.Tools().Register(tool); err != nil {
		return fmt.Errorf("preset: 同源工具 %q 登记失败：%w", spec.route.Name, err)
	}
	return nil
}

// servedRun 是工具的执行体：先按 schema 的 required 校验参数（模型漏字段就会在这里停住，
// 而不是让处理函数拿着零值去查一个不存在的领域对象），再进程内直调同一条链。
func servedRun(rt web.Registrar, spec *servedSpec) tools.RunFunc {
	return func(ctx context.Context, args map[string]any) (any, error) {
		var missing []string
		for _, k := range spec.required {
			if v, ok := args[k]; !ok || v == nil {
				missing = append(missing, k)
			}
		}
		if len(missing) > 0 {
			sort.Strings(missing)
			return nil, fmt.Errorf("%w：%v（接口 %s %s）", ErrServedArgsMissing, missing, spec.route.Method, spec.route.Path)
		}
		res, err := rt.Invoke(ctx, spec.route.Name, args)
		if err != nil {
			return nil, err
		}
		var env web.Envelope
		if json.Unmarshal(res.Body, &env) == nil && env.Code != "" {
			return env.Data, nil
		}
		// 宿主换了渲染封套 ⇒ 不再假设能拆出 data，原样交出响应体（工具结果本就是文本给模型看）。
		return string(res.Body), nil
	}
}

// Apply 不做接线（工具已在 Declare 相闭合），只在停止时留下可核对事实。
func (ts *Tools) Apply(k *kit.Kit) error {
	k.Logger().Info("preset: 工具声明已登记", map[string]any{"count": ts.register, "total": k.Tools().Len()})
	return nil
}

// Describe 供管理面内省。
func (ts *Tools) Describe() kit.Info {
	return kit.Info{Name: ts.Name(), Version: "preset", Notes: map[string]string{"registered": fmt.Sprint(ts.register)}}
}

// Registered 返回本插件登记进表的工具数。
func (ts *Tools) Registered() int {
	if ts == nil {
		return 0
	}
	return ts.register
}

// ---- Audit：把审计投递器接进装配生命周期 ----

// Audit 承载一个程序构造好的 `*audit.Fanout`，并在 `Stop` 时替它 flush。
//
// 为什么值得为它做一个插件而不是让程序自己 `fan.Emit`：投递器的生命周期属于装配根
// （带缓冲的 sink 不 Close 就等于丢掉事后要拿去举证的事实），而 `Fanout.Close` 需要
// 一个"套件停止"的信号 —— 那正是 `Kit.OnStop` 的用途（框架裁决 的可撤销要求）。
type Audit struct {
	name    string
	fan     *audit.Fanout
	ready   bool
	stopped bool
}

// NewAudit 包一个已构造的投递器（脱敏函数与 sink 都在宿主侧选：框架不替你决定数据往哪去）。
func NewAudit(fan *audit.Fanout) (*Audit, error) {
	if fan == nil {
		return nil, ErrAuditFanoutRequired
	}
	return &Audit{name: NameAudit, fan: fan}, nil
}

// Name 返回插件名。
func (a *Audit) Name() string {
	if a == nil || a.name == "" {
		return NameAudit
	}
	return a.name
}

// Declare 无扩展点可登记。
func (a *Audit) Declare(*kit.Declarer) error { return nil }

// Apply 置就绪位并登记停止钩子（Close 一次，错误记进日志：撤销路径不该静默丢数据）。
func (a *Audit) Apply(k *kit.Kit) error {
	if a.fan == nil {
		return ErrAuditFanoutRequired
	}
	a.ready = true
	a.stopped = false
	return k.OnStop(func() {
		// 先置停用再 flush：停用的意思是"这条链不再接受事实"，而不是"数据不用写完"。
		if err := a.fan.Close(); err != nil {
			k.Logger().Warn("preset: 审计 sink 关闭失败", map[string]any{"err": err.Error()})
		}
		a.stopped = true
		a.ready = false
	})
}

// Describe 供管理面内省。
func (a *Audit) Describe() kit.Info {
	sinks, ready := 0, false
	if a != nil {
		if a.fan != nil {
			sinks = a.fan.Sinks()
		}
		ready = a.ready
	}
	return kit.Info{Name: a.Name(), Version: "preset", Notes: map[string]string{"sinks": fmt.Sprint(sinks), "ready": fmt.Sprint(ready)}}
}

// Emit 投递一条事实（脱敏由 audit 包保证，本插件只保证它活在装配周期内）。
func (a *Audit) Emit(ctx context.Context, e audit.Entry) error {
	if a == nil || a.stopped {
		return ErrAuditStopped
	}
	if !a.ready {
		return ErrAuditNotReady
	}
	return a.fan.Emit(ctx, e)
}

// Fanout 返回投递器；未就绪时为 nil（与 Model/Perms 同一口径）。
func (a *Audit) Fanout() *audit.Fanout {
	if a == nil || !a.ready || a.stopped {
		return nil
	}
	return a.fan
}

// ---- Loop：调用循环（唯一需要跨插件取产物处 ⇒ 显式 Deps） ----

// Loop 把 agent.Loop 包成插件：Apply 相用 Kit 的唯一工具表 + Model 的客户端固化执行器。
type Loop struct {
	name    string
	model   *Model
	loop    *agent.Loop
	stopped bool
}

// NewLoop 构造循环插件。model 为 nil 直接拒绝（宁可启动失败，不跑"看起来在跑"的空循环）。
func NewLoop(m *Model) *Loop {
	return &Loop{name: NameLoop, model: m}
}

// Name 返回插件名。
func (l *Loop) Name() string {
	if l == nil || l.name == "" {
		return NameLoop
	}
	return l.name
}

// Deps 声明"模型出口必须先接线"。这是跨插件取产量的唯一正当方式：
// 忘了 Use(model) ⇒ Build 判 ErrMissingDependency，而不是运行期拿到 nil 客户端。
func (l *Loop) Deps() []string {
	if l == nil || l.model == nil {
		return nil
	}
	return []string{l.model.Name()}
}

// Declare 无扩展点可登记。
func (l *Loop) Declare(*kit.Declarer) error { return nil }

// Apply 固化执行器，并登记撤销钩子：Kit 停止后本产物一并作废。
//
// 为什么要在意这个：`agent.Loop` 本身无状态、可长期复用（框架裁决），但"插件被拔掉了"
// 却还能通过旧句柄继续跑，等于撤销只对了一半。钩子把 stopped 位置起 ⇒ `Run` 明确拒绝，
// 而不是悄悄继续用一份宿主已经认为不存在的能力。
func (l *Loop) Apply(k *kit.Kit) error {
	if l.model == nil {
		return ErrModelNotReady
	}
	client := l.model.Client()
	if client == nil {
		return ErrModelNotReady
	}
	l.loop = agent.NewLoop(agent.LoopConfig{
		Registry: k.Tools(),
		Client:   client,
		Endpoint: l.model.Endpoint(),
	})
	return k.OnStop(func() { l.stopped = true })
}

// ErrLoopStopped 表示 Kit 已停止 ⇒ 该循环产物不再可用。
var ErrLoopStopped = errors.New("preset: 装配根已停止，循环不可再用")

// Describe 供管理面内省。
func (l *Loop) Describe() kit.Info {
	state := "未装配"
	if l != nil && l.loop != nil {
		state = "已装配"
	}
	return kit.Info{Name: l.Name(), Version: "preset", Notes: map[string]string{"loop": state}}
}

// Run 跑一轮调用循环。角色必须由调用方经 ctx 携带（框架裁决：引擎无会话状态）。
func (l *Loop) Run(ctx context.Context, messages []model.ChatMessage) (agent.LoopOutcome, error) {
	if l == nil || l.loop == nil {
		return agent.LoopOutcome{}, ErrLoopNotAssembled
	}
	if l.stopped {
		return agent.LoopOutcome{}, ErrLoopStopped
	}
	return l.loop.Run(ctx, messages)
}

// Ask 是单问句便利入口：把一句问题包成 user 消息。预算与角色由调用方经 ctx 决定。
func (l *Loop) Ask(ctx context.Context, question string) (agent.LoopOutcome, error) {
	return l.Run(ctx, []model.ChatMessage{{Role: "user", Content: question}})
}

// ---- Server：把 web 生命周期接进装配根 ----

// Server 承载一个 `web.Server`，并保证 Kit 停止时它被优雅关闭。
//
// 为什么需要这个插件：`ListenAndServe` 阻塞、`Shutdown` 需要一个 ctx，而"该停了"这件事
// 只有装配根知道（`Kit.Stop`）。插件把它翻译成"停止时必须执行的动作"，
// 于是宿主不会因为漏调 Shutdown 而留下一个还在收连接的服务器。
//
// 注意本包不出现 `net/http` 符号：`http.Handler`/`http.Server` 属入域能力面，
// 只能待在 `web`（框架裁决 D2），所以这里只持有 `*web.Server`。
type Server struct {
	name    string
	srv     *web.Server
	ready   bool
	stopped bool
	mu      sync.Mutex
}

// NewServer 包一个已构造的服务器（地址与路由表都由宿主在框架外决定）。
func NewServer(srv *web.Server) *Server {
	return &Server{name: NameServer, srv: srv}
}

// Name 返回插件名。
func (sv *Server) Name() string {
	if sv == nil || sv.name == "" {
		return NameServer
	}
	return sv.name
}

// Declare 无扩展点可登记。
func (sv *Server) Declare(*kit.Declarer) error { return nil }

// Apply 确认服务器可用，并登记停止钩子（优雅关闭）。
func (sv *Server) Apply(k *kit.Kit) error {
	if sv.srv == nil {
		return ErrServerRequired
	}
	sv.mu.Lock()
	sv.ready = true
	sv.stopped = false
	sv.mu.Unlock()
	k.Logger().Info("preset: 服务地址就绪", map[string]any{"addr": sv.srv.Addr()})
	return k.OnStop(func() {
		sv.mu.Lock()
		sv.stopped = true
		sv.ready = false
		sv.mu.Unlock()
		if err := sv.srv.Shutdown(context.Background()); err != nil {
			k.Logger().Warn("preset: 服务优雅关闭失败", map[string]any{"err": err.Error()})
		}
	})
}

// Describe 供管理面内省。
func (sv *Server) Describe() kit.Info {
	addr, ready := "", false
	if sv != nil {
		if sv.srv != nil {
			addr = sv.srv.Addr()
		}
		ready = sv.ready
	}
	return kit.Info{Name: sv.Name(), Version: "preset", Notes: map[string]string{"addr": addr, "ready": fmt.Sprint(ready)}}
}

// Addr 返回监听地址（未就绪时为空）。
func (sv *Server) Addr() string {
	if sv == nil || sv.srv == nil || !sv.ready {
		return ""
	}
	return sv.srv.Addr()
}

// Serve 阻塞地开始服务（用插件构造时给定的 Addr）。
// 停止判定先行：装配根已停 ⇒ 绝不返回"成功"，也绝不留下一个还在收连接的服务器。
func (sv *Server) Serve() error {
	ln, err := net.Listen("tcp", sv.addrOrEmpty())
	if err != nil {
		return err
	}
	return sv.ServeAt(ln)
}

// ServeAt 用调用方持有的监听器开始服务（`:0` 随机端口、unix socket、宿主自己包 TLS 等）。
// 监听器仍由调用方负责关闭；本方法的就绪/停止门禁与 Serve 完全一致。
func (sv *Server) ServeAt(ln net.Listener) error {
	if sv == nil || sv.srv == nil {
		return ErrServerRequired
	}
	sv.mu.Lock()
	ready, stopped := sv.ready, sv.stopped
	sv.mu.Unlock()
	if stopped {
		return ErrServerStopped
	}
	if !ready {
		return ErrServerNotReady
	}
	return sv.srv.Serve(ln)
}

func (sv *Server) addrOrEmpty() string {
	if sv == nil || sv.srv == nil {
		return ""
	}
	return sv.srv.Addr()
}

// 编译期自检：各插件都满足契约（漏实现接口在编译期就炸，不靠测试发现）。
var (
	_ kit.Plugin         = (*Model)(nil)
	_ kit.Introspectable = (*Model)(nil)
	_ kit.Plugin         = (*Perms)(nil)
	_ kit.Introspectable = (*Perms)(nil)
	_ kit.Plugin         = (*Tools)(nil)
	_ kit.Introspectable = (*Tools)(nil)
	_ kit.Plugin         = (*Loop)(nil)
	_ kit.Dependent      = (*Loop)(nil)
	_ kit.Introspectable = (*Loop)(nil)
)

// ---- Planner：语义绑定（意图/实体 → 同源工具的选择与参数装配） ----

// NamePlanner 是语义绑定插件的默认名。
const NamePlanner = "preset.planner"

// Planner 把 `semantic` 的理解结果翻成调用计划，是 P6 的消费者入口。
//
// 它刻意不做两件事：
//   - **不执行**：计划交给调用方（人/循环），执行仍走 `tools.Tool.Run`（也就是 P5 的 `Router.Invoke`）；
//   - **不判定权限**：工具表从 `k.Tools` 拿（框架内唯一那一张），授权与否由 `tools` 的那一条谓词回答，
//     角色从 `kitctx.RoleFrom(ctx)` 读——读不到就交空串，由谓词判成"角色不可用"，框架不猜默认角色。
type Planner struct {
	name    string
	binding *semantic.Planner
	reg     *tools.Registry
	stopped bool
}

// NewPlanner 用一组绑定规则构造插件；规则的畸形在这里（构造期）就被拒绝。
func NewPlanner(rules ...semantic.Rule) (*Planner, error) {
	b, err := semantic.NewPlanner(rules...)
	if err != nil {
		return nil, err
	}
	return &Planner{name: NamePlanner, binding: b}, nil
}

// Name 返回插件名。
func (p *Planner) Name() string {
	if p == nil || p.name == "" {
		return NamePlanner
	}
	return p.name
}

// Declare 不做登记（绑定器不往扩展点里放东西）。
func (p *Planner) Declare(*kit.Declarer) error { return nil }

// Apply 接上 Kit 的唯一工具表，并在停止时关掉计划能力。
func (p *Planner) Apply(k *kit.Kit) error {
	if p == nil || p.binding == nil {
		return ErrPlannerRulesRequired
	}
	p.reg = k.Tools()
	return k.OnStop(func() { p.stopped = true })
}

// Describe 供管理面内省。
func (p *Planner) Describe() kit.Info {
	n := 0
	if p != nil && p.binding != nil {
		n = len(p.binding.Rules())
	}
	return kit.Info{Name: p.Name(), Version: "preset", Notes: map[string]string{"rules": fmt.Sprint(n)}}
}

// Rules 返回绑定规则副本（管理面展示用）。
func (p *Planner) Rules() []semantic.Rule {
	if p == nil || p.binding == nil {
		return nil
	}
	return p.binding.Rules()
}

// Plan 按 ctx 里的角色产出调用计划。
//
// 返回的 error 只表示"这次没法判定"（未装配 / 已停止 / 缺工具表），
// 单条规则没成为调用不是错误 —— 那是 `Plan.Skipped` 里的留痕事实。
func (p *Planner) Plan(ctx context.Context, u semantic.Understanding) (semantic.Plan, error) {
	if p == nil || p.binding == nil {
		return semantic.Plan{}, ErrPlannerRulesRequired
	}
	if p.stopped {
		return semantic.Plan{}, ErrPlannerStopped
	}
	if p.reg == nil {
		return semantic.Plan{}, ErrPlannerNotAssembled
	}
	role, _ := kitctx.RoleFrom(ctx)
	return p.binding.Plan(u, string(role), p.reg)
}

var _ kit.Plugin = (*Planner)(nil)
