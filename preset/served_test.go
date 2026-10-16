package preset

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"agentkit/authz"
	"agentkit/kit"
	"agentkit/kitctx"
	"agentkit/tools"
	"agentkit/web"
)

type askIn struct {
	Q     string `json:"q"`
	Limit int    `json:"limit,omitempty"`
	Skip  string `json:"-"`
	priv  string
}

func servedPerms(t *testing.T) *Perms {
	t.Helper()
	roles, err := authz.NewRoleSet("support", "auditor", "admin")
	if err != nil {
		t.Fatalf("NewRoleSet：%v", err)
	}
	p, err := NewPerms(roles, authz.PermissionPoint{
		Key: "engine.ask", Label: "领域对象问答", Scope: authz.ScopePlatform,
		DefaultRoles: []kitctx.Role{"support", "admin"}, Enforced: authz.DirectEnforcement(),
	})
	if err != nil {
		t.Fatalf("NewPerms：%v", err)
	}
	return p
}

func askHandler(calls *int) web.Handler {
	return func(w http.ResponseWriter, r *http.Request) error {
		var in askIn
		if err := web.DecodeJSON(r, &in); err != nil {
			if kind, ok := web.KindOf(err); ok && kind == web.ErrKindTooLarge {
				return err
			}
			// GET 走查询参数
			in.Q = r.URL.Query().Get("q")
		}
		if in.Q == "boom" {
			return web.Errorf(web.ErrKindForbidden, "这个问题被拒绝")
		}
		if calls != nil {
			*calls++
		}
		role, _ := kitctx.RoleFrom(r.Context())
		web.WriteJSON(w, map[string]string{"answer": "关于 " + in.Q, "role": string(role)})
		return nil
	}
}

func TestPreset_ServedConstructorRejects(t *testing.T) {
	base := web.Route{Method: "POST", Path: "/engine/ask", Name: "engine.ask"}
	for _, c := range []struct {
		name    string
		route   web.Route
		sample  any
		perm    string
		handler web.Handler
		want    error
	}{
		{"无稳定名称", web.Route{Method: "POST", Path: "/x"}, askIn{}, "engine.ask", askHandler(nil), ErrServedNameRequired},
		{"未绑权限点", base, askIn{}, "  ", askHandler(nil), ErrServedPermRequired},
		{"公开接口想做工具", web.Route{Method: "POST", Path: "/x", Name: "x", Public: true}, askIn{}, "engine.ask", askHandler(nil), ErrServedPublicRoute},
		{"缺处理函数", base, askIn{}, "engine.ask", nil, ErrServedHandlerRequired},
		{"两处权限键不一致", web.Route{Method: "POST", Path: "/x", Name: "x", Perm: "other"}, askIn{}, "engine.ask", askHandler(nil), ErrServedPermMismatch},
		{"参数样例非结构体", base, "不是结构体", "engine.ask", askHandler(nil), tools.ErrSchemaUnsupported},
	} {
		if _, err := Served(c.route, c.sample, c.perm, c.handler); !errors.Is(err, c.want) {
			t.Errorf("%s：应判 %v，实得 %v", c.name, c.want, err)
		}
	}
	src, err := Served(web.Route{Method: "POST", Path: "/engine/ask", Name: " engine.ask "}, askIn{}, " engine.ask ", askHandler(nil))
	if err != nil {
		t.Fatalf("合法声明被拒：%v", err)
	}
	if src.served.route.Name != "engine.ask" || src.served.route.Perm != "engine.ask" {
		t.Fatalf("归一化未生效：%+v", src.served.route)
	}
	if src.served.route.Summary == "" {
		t.Fatal("工具摘要应有默认值（模型选型只看名字与摘要）")
	}
	// schema 与结构体同源：`-` 与未导出字段都不该出现
	p := src.served.schema["properties"].(map[string]any)
	if len(p) != 2 {
		t.Fatalf("properties 应只含 q/limit：%v", p)
	}
	if got := src.served.required; len(got) != 1 || got[0] != "q" {
		t.Fatalf("required 应恰为 [q]：%v", got)
	}
}

func TestPreset_ServedRegistersBothOrNeither(t *testing.T) {
	src, err := Served(web.Route{Method: "POST", Path: "/engine/ask", Name: "engine.ask"}, askIn{}, "engine.ask", askHandler(nil))
	if err != nil {
		t.Fatalf("Served：%v", err)
	}
	// ① 没有路由表的装配根：同源声明必须整条失败，不能"只登记工具"
	k0, err := kit.New(kit.Config{})
	if err != nil {
		t.Fatalf("kit.New：%v", err)
	}
	if err := k0.Use(NewTools(servedPerms(t), src)); err != nil {
		t.Fatalf("Use：%v", err)
	}
	if err := k0.Build(); !errors.Is(err, ErrWebRouterRequired) {
		t.Fatalf("缺路由表应判 ErrWebRouterRequired，实得 %v", err)
	}
	if k0.Tools().Len() != 0 {
		t.Fatalf("失败声明却登记了 %d 个工具 ⇒ 半张表", k0.Tools().Len())
	}

	// ② 有路由表：两张表同时出现，且键一致
	rt := web.NewRouter(web.Config{})
	k, err := kit.New(kit.Config{Web: rt})
	if err != nil {
		t.Fatalf("kit.New：%v", err)
	}
	decl := NewTools(servedPerms(t), src)
	if err := k.Use(decl, servedPerms(t)); err != nil {
		t.Fatalf("Use：%v", err)
	}
	if err := k.Build(); err != nil {
		t.Fatalf("Build：%v", err)
	}
	routes := rt.Routes()
	if len(routes) != 1 || routes[0].Name != "engine.ask" || routes[0].Perm != "engine.ask" {
		t.Fatalf("路由表未拿到同源事实：%+v", routes)
	}
	tool, ok := k.Tools().Lookup("engine.ask")
	if !ok {
		t.Fatal("工具表里没有同名工具")
	}
	if tool.Summary != routes[0].Summary {
		t.Fatalf("摘要两处不一致：%q vs %q", tool.Summary, routes[0].Summary)
	}
	sch, err := tools.SchemaFor(askIn{})
	if err != nil {
		t.Fatalf("SchemaFor：%v", err)
	}
	if got, _ := json.Marshal(tool.Params); string(got) != mustJSON(t, sch) {
		t.Fatalf("工具参数 schema 与结构体生成结果不一致：%s", got)
	}

	// ③ 名字冲突：宁可整条失败，也不留下"有路由没工具"的孤儿接口
	rt2 := web.NewRouter(web.Config{})
	k2, err := kit.New(kit.Config{Web: rt2})
	if err != nil {
		t.Fatalf("kit.New：%v", err)
	}
	dup, err := Served(web.Route{Method: "POST", Path: "/engine/ask", Name: "engine.ask"}, askIn{}, "engine.ask", askHandler(nil))
	if err != nil {
		t.Fatalf("Served：%v", err)
	}
	if err := k2.Use(NewTools(servedPerms(t), Direct(demoTool("engine.ask")), dup), servedPerms(t)); err != nil {
		t.Fatalf("Use：%v", err)
	}
	if err := k2.Build(); err == nil {
		t.Fatal("重名应让整条同源声明失败")
	}
	if len(rt2.Routes()) != 0 {
		t.Fatalf("失败后路由表却多了 %d 条 ⇒ 两张表又不一致了", len(rt2.Routes()))
	}
}

func TestPreset_ServedRunInvokesSameChainAndChecksRequired(t *testing.T) {
	var calls int
	src, err := Served(web.Route{Method: "POST", Path: "/engine/ask", Name: "engine.ask"}, askIn{}, "engine.ask", askHandler(&calls))
	if err != nil {
		t.Fatalf("Served：%v", err)
	}
	rt := web.NewRouter(web.Config{MaxBodyBytes: 64})
	mwSeen := 0
	if err := rt.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mwSeen++
			next.ServeHTTP(w, r)
		})
	}); err != nil {
		t.Fatalf("Use：%v", err)
	}
	k, err := kit.New(kit.Config{Web: rt})
	if err != nil {
		t.Fatalf("kit.New：%v", err)
	}
	if err := k.Use(NewTools(servedPerms(t), src), servedPerms(t)); err != nil {
		t.Fatalf("Use：%v", err)
	}
	if err := k.Build(); err != nil {
		t.Fatalf("Build：%v", err)
	}
	tool, _ := k.Tools().Lookup("engine.ask")
	ctx := kitctx.WithRequest(context.Background(), kitctx.RequestContext{UserID: "u1", Role: "support", TraceID: "tr"})

	// 必填齐全 ⇒ 进程内直调，中间件照跑
	data, err := tool.Run(ctx, map[string]any{"q": "领域对象 1.2.3.4", "limit": float64(5)})
	if err != nil {
		t.Fatalf("直调失败：%v", err)
	}
	body, _ := json.Marshal(data)
	if !strings.Contains(string(body), "领域对象 1.2.3.4") || !strings.Contains(string(body), `"role":"support"`) {
		t.Fatalf("结果缺要素：%s", body)
	}
	if calls != 1 || mwSeen != 1 {
		t.Fatalf("工具调用未走同一条链（handler=%d mw=%d）", calls, mwSeen)
	}
	// 漏必填 ⇒ 停在工具边界，不产生任何调用
	if _, err := tool.Run(ctx, map[string]any{"limit": float64(5)}); !errors.Is(err, ErrServedArgsMissing) {
		t.Fatalf("漏 q 应被 schema 拦下，实得 %v", err)
	}
	if calls != 1 {
		t.Fatalf("参数不全却调用了处理函数（calls=%d）", calls)
	}
	// 显式 null 也算缺（模型常给 null 占位）
	if _, err := tool.Run(ctx, map[string]any{"q": nil}); !errors.Is(err, ErrServedArgsMissing) {
		t.Fatalf("q:null 应被判缺字段，实得 %v", err)
	}
	// 处理函数返回业务错误 ⇒ 类别原样上抛（Loop 据此记缺口）
	if _, err := tool.Run(ctx, map[string]any{"q": "boom"}); !errors.Is(err, web.ErrKindForbidden) {
		t.Fatalf("业务错误应带类别上抛，实得 %v", err)
	}
	// 零值不污染入参表
	args := map[string]any{"q": "hi"}
	if _, err := tool.Run(ctx, args); err != nil {
		t.Fatalf("直调失败：%v", err)
	}
	if len(args) != 1 {
		t.Fatalf("Run 改写了调用方给的参数表：%v", args)
	}
}

// TestPreset_ServedParityBetweenTables 是 P5 的门面判据：
// 同一角色下「工具可见」与「路由声明的权限点」必须给出同一个答案 —— 因为二者由同一处生成。
func TestPreset_ServedParityBetweenTables(t *testing.T) {
	src, err := Served(web.Route{Method: "POST", Path: "/engine/ask", Name: "engine.ask"}, askIn{}, "engine.ask", askHandler(nil))
	if err != nil {
		t.Fatalf("Served：%v", err)
	}
	plain, err := Served(web.Route{Method: "GET", Path: "/engine/other", Name: "engine.other"}, askIn{}, "engine.ask", askHandler(nil))
	if err != nil {
		t.Fatalf("Served：%v", err)
	}
	rt := web.NewRouter(web.Config{})
	k, err := kit.New(kit.Config{Web: rt})
	if err != nil {
		t.Fatalf("kit.New：%v", err)
	}
	if err := k.Use(NewTools(servedPerms(t), src, plain), servedPerms(t)); err != nil {
		t.Fatalf("Use：%v", err)
	}
	if err := k.Build(); err != nil {
		t.Fatalf("Build：%v", err)
	}
	for _, role := range []kitctx.Role{"support", "admin", "auditor"} {
		visible := map[string]bool{}
		for _, v := range k.Tools().VisibleByRole(string(role)) {
			visible[v.Name] = true
		}
		for _, r := range rt.Routes() {
			_, _, allowed := k.Tools().LookupAuthorized(r.Name, string(role))
			if allowed != visible[r.Name] {
				t.Fatalf("角色 %s 下路由 %q 与工具可见性不一致（路由 %v / 工具 %v）", role, r.Name, allowed, visible[r.Name])
			}
		}
		if role == "auditor" && len(visible) != 0 {
			t.Fatalf("无权角色却看得到工具：%v", visible)
		}
		if role != "auditor" && len(visible) != 2 {
			t.Fatalf("有权角色应看到两条同源工具：%v", visible)
		}
	}
}

// TestPreset_ServedPermUnknownFailsWholeDeclaration 复述 fail-open 防线在 web 路径上同样成立。
func TestPreset_ServedPermUnknownFailsWholeDeclaration(t *testing.T) {
	src, err := Served(web.Route{Method: "GET", Path: "/x", Name: "x"}, askIn{}, "no.such.point", askHandler(nil))
	if err != nil {
		t.Fatalf("Served：%v", err)
	}
	rt := web.NewRouter(web.Config{})
	k, err := kit.New(kit.Config{Web: rt})
	if err != nil {
		t.Fatalf("kit.New：%v", err)
	}
	if err := k.Use(NewTools(servedPerms(t), src), servedPerms(t)); err != nil {
		t.Fatalf("Use：%v", err)
	}
	if err := k.Build(); err == nil {
		t.Fatal("未知权限点必须让整条声明失败")
	}
	if len(rt.Routes()) != 0 || k.Tools().Len() != 0 {
		t.Fatalf("失败后仍留下半张表：routes=%d tools=%d", len(rt.Routes()), k.Tools().Len())
	}
}

// webProbe 在 Declare 相把 `Declarer.Web` 交出来的东西存下来供断言。
type webProbe struct{ got web.Registrar }

func (w *webProbe) Name() string                  { return "preset.webprobe" }
func (w *webProbe) Declare(d *kit.Declarer) error { w.got = d.Web(); return nil }
func (w *webProbe) Apply(*kit.Kit) error          { return nil }

// TestPreset_DeclarePhaseCannotSealRouter 证明"窄接口"是事实而不是修辞：
// 声明期拿到的值，动态类型只有 Register/Routes/Invoke 三个方法，
// 一句类型断言也够不到 `Handler`（提前封存整张路由表）或 `Use`。
func TestPreset_DeclarePhaseCannotSealRouter(t *testing.T) {
	rt := web.NewRouter(web.Config{})
	probe := &webProbe{}
	k, err := kit.New(kit.Config{Web: rt})
	if err != nil {
		t.Fatalf("kit.New：%v", err)
	}
	if err := k.Use(probe); err != nil {
		t.Fatalf("Use：%v", err)
	}
	if err := k.Build(); err != nil {
		t.Fatalf("Build：%v", err)
	}
	if probe.got == nil {
		t.Fatal("配置了路由表却交出 nil")
	}
	for _, c := range []struct {
		name string
		hint func() bool
	}{
		{"Handler()", func() bool { _, ok := probe.got.(interface{ Handler() (http.Handler, error) }); return ok }},
		{"Use()", func() bool { _, ok := probe.got.(interface{ Use(...web.Middleware) error }); return ok }},
		{"Serve/Shutdown()", func() bool { _, ok := probe.got.(interface{ Shutdown(context.Context) error }); return ok }},
	} {
		if c.hint() {
			t.Fatalf("声明期视图竟能拿到 %s ⇒ 一个插件就能封存/停掉全局路由表", c.name)
		}
	}
	// 但三个正当能力必须在：登记、枚举、直调
	if _, err := probe.got.Invoke(context.Background(), "nope", nil); !errors.Is(err, web.ErrRouteUnknown) {
		t.Fatalf("声明期视图少了直调能力，实得 %v", err)
	}
	// 未配置路由表时不得给你一个"什么都不做"的假表（那会让同源声明静默退化成只有工具）
	k2, err := kit.New(kit.Config{})
	if err != nil {
		t.Fatalf("kit.New：%v", err)
	}
	probe2 := &webProbe{}
	if err := k2.Use(probe2); err != nil {
		t.Fatalf("Use：%v", err)
	}
	if err := k2.Build(); err != nil {
		t.Fatalf("Build：%v", err)
	}
	if probe2.got != nil {
		t.Fatal("未配置路由表却交出了非 nil 视图")
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("Marshal：%v", err)
	}
	return string(b)
}
