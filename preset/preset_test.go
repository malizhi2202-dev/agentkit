package preset

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"agentkit/audit"
	"agentkit/authz"
	"agentkit/kit"
	"agentkit/kitctx"
	"agentkit/model"
	"agentkit/tools"
	"agentkit/web"
)

// errDoer 只在测试里存在：本包（非出域包）一旦在实现文件里出现 *http.Response 就会被
// `guard_test.go` 的方向/旁路判据抓红 —— 测试文件按既有口径豁免。
type errDoer struct{}

func (errDoer) Do(*http.Request) (*http.Response, error) {
	return nil, errors.New("preset 测试不应出网")
}

func testClient(t *testing.T) *model.Client {
	t.Helper()
	return model.NewClient(model.ClientConfig{
		Doer:        errDoer{},
		Policy:      model.NewEndpointPolicy(model.EndpointPolicyConfig{AllowedHosts: []string{"192.168.16.16"}}),
		MaxAttempts: 1,
	})
}

func testModel(t *testing.T) *Model {
	t.Helper()
	m, err := NewModel(testClient(t), "http://192.168.16.16:8080/v1/chat/completions")
	if err != nil {
		t.Fatalf("NewModel 失败：%v", err)
	}
	return m
}

func testRoles(t *testing.T) *authz.RoleSet {
	t.Helper()
	s, err := authz.NewRoleSet("support", "auditor", "admin")
	if err != nil {
		t.Fatalf("NewRoleSet 失败：%v", err)
	}
	return s
}

func permPoint() authz.PermissionPoint {
	return authz.PermissionPoint{
		Key: "user.profile.read", Label: "IP 领域对象档案", Scope: authz.ScopePlatform,
		DefaultRoles: []kitctx.Role{"support", "admin"},
		Enforced:     authz.DirectEnforcement(),
	}
}

func demoTool(name string) tools.Tool {
	return tools.Tool{
		Name:    name,
		Summary: name + " 摘要",
		Run:     func(_ context.Context, _ map[string]any) (any, error) { return name, nil },
	}
}

func TestPreset_ModelRejections(t *testing.T) {
	if _, err := NewModel(nil, "http://192.168.16.16/x"); !errors.Is(err, ErrModelClientRequired) {
		t.Fatalf("空客户端应判失败，实得 %v", err)
	}
	if _, err := NewModel(testClient(t), "   "); !errors.Is(err, ErrModelEndpointRequired) {
		t.Fatalf("空端点应在**装配期**判失败（不等运行期降级），实得 %v", err)
	}
	m := testModel(t)
	if m.Client() != nil {
		t.Fatal("未 Apply 前不得交出客户端（否则两阶段无意义）")
	}
	k, err := kit.New(kit.Config{})
	if err != nil {
		t.Fatalf("kit.New 失败：%v", err)
	}
	if err := k.Use(m); err != nil {
		t.Fatalf("Use 失败：%v", err)
	}
	if err := k.Build(); err != nil {
		t.Fatalf("Build 失败：%v", err)
	}
	if m.Client() == nil {
		t.Fatal("Apply 后应可取到客户端")
	}
	if m.Endpoint() == "" {
		t.Fatal("Endpoint 应透传")
	}
}

func TestPreset_PermsRejections(t *testing.T) {
	if _, err := NewPerms(nil, permPoint()); !errors.Is(err, ErrPermsRolesRequired) {
		t.Fatalf("缺角色闭集应判失败，实得 %v", err)
	}
	bad := permPoint()
	bad.DefaultRoles = nil
	if _, err := NewPerms(testRoles(t), bad); !errors.Is(err, authz.ErrDefaultRolesRequired) {
		t.Fatalf("空默认角色应被 authz 拒绝（本包不复制第二套校验），实得 %v", err)
	}
	p, err := NewPerms(testRoles(t), permPoint())
	if err != nil {
		t.Fatalf("NewPerms 失败：%v", err)
	}
	if p.Registry() != nil {
		t.Fatal("未 Apply 前不得交出权限表")
	}
	k, _ := kit.New(kit.Config{})
	if err := k.Use(p); err != nil {
		t.Fatalf("Use 失败：%v", err)
	}
	if err := k.Build(); err != nil {
		t.Fatalf("Build 失败：%v", err)
	}
	if p.Registry() == nil || p.Registry().Len() != 1 {
		t.Fatalf("Apply 后权限表应可用，实得 %+v", p.Registry())
	}
}

// TestPreset_GrantedRolesSameSource 是"接口/工具/权限同源"的最小实证：
// 工具上的角色集合**只能**来自权限点，且框架里没有第二个填写处。
func TestPreset_GrantedRolesSameSource(t *testing.T) {
	p, err := NewPerms(testRoles(t), permPoint())
	if err != nil {
		t.Fatalf("NewPerms 失败：%v", err)
	}
	decl := NewTools(p, Granted(demoTool("user.profile"), "user.profile.read"), Direct(mustRoleTool("plain", "auditor")))

	k, err := kit.New(kit.Config{})
	if err != nil {
		t.Fatalf("kit.New 失败：%v", err)
	}
	if err := k.Use(p, decl); err != nil {
		t.Fatalf("Use 失败：%v", err)
	}
	if err := k.Build(); err != nil {
		t.Fatalf("Build 失败：%v", err)
	}
	got, ok := k.Tools().Lookup("user.profile")
	if !ok {
		t.Fatal("绑定权限点的工具未登记")
	}
	if strings.Join(got.RequiredRoles, ",") != "support,admin" {
		t.Fatalf("工具角色应由权限点生成（support,admin），实得 %v", got.RequiredRoles)
	}
	// 手填与绑定共存 ⇒ 两个来源，必须拒绝。
	conflict := demoTool("conflict")
	conflict.RequiredRoles = []string{"auditor"}
	k2, _ := kit.New(kit.Config{})
	p2, _ := NewPerms(testRoles(t), permPoint())
	if err := k2.Use(p2, NewTools(p2, Granted(conflict, "user.profile.read"))); err != nil {
		t.Fatalf("Use 失败：%v", err)
	}
	if err := k2.Build(); !errors.Is(err, ErrRequiredRolesConflict) {
		t.Fatalf("双来源应判 ErrRequiredRolesConflict，实得 %v", err)
	}
	if k2.Tools().Len() != 0 {
		t.Fatalf("冲突声明不得有任何工具落地，实得 %d 条", k2.Tools().Len())
	}
}

// TestPreset_UnknownPermissionFailsClosed 是最要紧的一条：
// 权限点写错 ⇒ 整条声明失败，**绝不**注册成"无角色要求的公开工具"。
func TestPreset_UnknownPermissionFailsClosed(t *testing.T) {
	p, err := NewPerms(testRoles(t), permPoint())
	if err != nil {
		t.Fatalf("NewPerms 失败：%v", err)
	}
	k, _ := kit.New(kit.Config{})
	if err := k.Use(p, NewTools(p, Granted(demoTool("user.profile"), "profile.ip.typo"))); err != nil {
		t.Fatalf("Use 失败：%v", err)
	}
	err = k.Build()
	if err == nil || !errors.Is(err, authz.ErrUnknownPermission) {
		t.Fatalf("未知权限点必须让 Build 失败，实得 %v", err)
	}
	if !strings.Contains(err.Error(), "user.profile") {
		t.Fatalf("错误必须点名是哪条工具，实得 %v", err)
	}
	if k.Tools().Len() != 0 {
		t.Fatalf("fail-open 检查：越权工具仍被登记为公开工具（%v）⇒ 空 RequiredRoles 等于放开", k.Tools().Names())
	}
	// 绑了权限点却没给权限表 ⇒ 同样拒绝（不猜、不降级）。
	k2, _ := kit.New(kit.Config{})
	if err := k2.Use(NewTools(nil, Granted(demoTool("x"), "any.perm"))); err != nil {
		t.Fatalf("Use 失败：%v", err)
	}
	if err := k2.Build(); !errors.Is(err, ErrPermsNil) {
		t.Fatalf("缺权限表应判 ErrPermsNil，实得 %v", err)
	}
}

func TestPreset_LoopNeedsModelInKit(t *testing.T) {
	m := testModel(t)
	loop := NewLoop(m)
	k, _ := kit.New(kit.Config{})
	if err := k.Use(loop); err != nil {
		t.Fatalf("Use 失败：%v", err)
	}
	// 只 Use 了 loop：它依赖 preset.model，但 model 未参与装配 ⇒ 必须失败，而不是静默降级。
	if err := k.Build(); !errors.Is(err, kit.ErrMissingDependency) {
		t.Fatalf("缺依赖应判 ErrMissingDependency，实得 %v", err)
	}

	// NewLoop(nil) 也要在 Build 期被抓（不给你一台"看着能跑"的循环）。
	k2, _ := kit.New(kit.Config{})
	if err := k2.Use(NewLoop(nil)); err != nil {
		t.Fatalf("Use 失败：%v", err)
	}
	if err := k2.Build(); err == nil {
		t.Fatal("无模型依赖的循环必须失败")
	}
}

func TestPreset_LoopLifecycle(t *testing.T) {
	m := testModel(t)
	loop := NewLoop(m)
	k, _ := kit.New(kit.Config{})
	if err := k.Use(m, loop); err != nil {
		t.Fatalf("Use 失败：%v", err)
	}
	if _, err := loop.Run(context.Background(), nil); !errors.Is(err, ErrLoopNotAssembled) {
		t.Fatalf("Build 前 Run 应判 ErrLoopNotAssembled，实得 %v", err)
	}
	if err := k.Build(); err != nil {
		t.Fatalf("Build 失败：%v", err)
	}
	if got := k.ApplyOrder(); len(got) != 2 || got[0] != NameModel {
		t.Fatalf("依赖应把 model 排到 loop 之前，实得 %v", got)
	}
	// Stop 之后旧产物必须作废（撤销不只撤销钩子，也让产物拒绝服务）。
	if err := k.Stop(); err != nil {
		t.Fatalf("Stop 失败：%v", err)
	}
	if _, err := loop.Run(context.Background(), nil); !errors.Is(err, ErrLoopStopped) {
		t.Fatalf("Stop 后 Run 应判 ErrLoopStopped，实得 %v", err)
	}
}

func TestPreset_PluginSurface(t *testing.T) {
	m := testModel(t)
	p, err := NewPerms(testRoles(t), permPoint())
	if err != nil {
		t.Fatalf("NewPerms 失败：%v", err)
	}
	decl := NewTools(p, Direct(demoTool("a")))
	loop := NewLoop(m)
	plugins := []kit.Plugin{m, p, decl, loop}
	seen := map[string]bool{}
	for _, pl := range plugins {
		if pl.Name() == "" || seen[pl.Name()] {
			t.Fatalf("插件名必须非空且唯一：%v", seen)
		}
		seen[pl.Name()] = true
		info := pl.(kit.Introspectable).Describe()
		if info.Name != pl.Name() {
			t.Errorf("Describe 的 Name 应与插件一致：%q vs %q", info.Name, pl.Name())
		}
	}
	if strings.Join(loop.Deps(), ",") != NameModel {
		t.Fatalf("loop 应依赖 model，实得 %v", loop.Deps())
	}
	if decl.Registered() != 0 {
		t.Fatal("Declare 之前登记数应为 0")
	}
}

func TestPreset_AuditLifecycle(t *testing.T) {
	if _, err := NewAudit(nil); !errors.Is(err, ErrAuditFanoutRequired) {
		t.Fatalf("空投递器应被拒，实得 %v", err)
	}
	fan, err := audit.New(audit.BlacklistMasker(), nil, audit.Nop())
	if err != nil {
		t.Fatalf("audit.New：%v", err)
	}
	a, err := NewAudit(fan)
	if err != nil {
		t.Fatalf("NewAudit：%v", err)
	}
	if a.Fanout() != nil {
		t.Fatal("未 Apply 前不得交出投递器")
	}
	if err := a.Emit(context.Background(), audit.Entry{Action: "x", Outcome: audit.OutcomeOK}); !errors.Is(err, ErrAuditNotReady) {
		t.Fatalf("Build 前发审计应判 ErrAuditNotReady，实得 %v", err)
	}
	k, _ := kit.New(kit.Config{})
	if err := k.Use(a); err != nil {
		t.Fatalf("Use：%v", err)
	}
	if err := k.Build(); err != nil {
		t.Fatalf("Build：%v", err)
	}
	if a.Fanout() == nil {
		t.Fatal("Apply 后应可取到投递器")
	}
	if err := a.Emit(context.Background(), audit.Entry{Action: "x", Outcome: audit.OutcomeOK}); err != nil {
		t.Fatalf("Apply 后发审计失败：%v", err)
	}
	if a.Name() != NameAudit {
		t.Fatalf("Name=%q", a.Name())
	}
	// Stop 必须让这条链彻底停用：既不再交出投递器，也不再接受新事实（否则 flush 之后还能写）。
	if err := k.Stop(); err != nil {
		t.Fatalf("Stop：%v", err)
	}
	if a.Fanout() != nil {
		t.Fatal("Stop 后仍交出投递器 ⇒ 撤销不彻底")
	}
	if err := a.Emit(context.Background(), audit.Entry{Action: "x", Outcome: audit.OutcomeOK}); !errors.Is(err, ErrAuditStopped) {
		t.Fatalf("Stop 后投递应判 ErrAuditStopped，实得 %v", err)
	}
}

// ---- 小工具 ----

func mustRoleTool(name, role string) tools.Tool {
	tt := demoTool(name)
	tt.RequiredRoles = []string{role}
	return tt
}

func TestPreset_ServerLifecycle(t *testing.T) {
	// 没有服务器就构造插件：Apply 必须拒绝（不留一个"看起来就绪"的假服务）
	k0, err := kit.New(kit.Config{})
	if err != nil {
		t.Fatalf("kit.New：%v", err)
	}
	if err := k0.Use(NewServer(nil)); err != nil {
		t.Fatalf("Use 阶段只做名字/依赖检查，不该报错：%v", err)
	}
	if err := k0.Build(); !errors.Is(err, ErrServerRequired) {
		t.Fatalf("空服务器应在 Apply 被判 ErrServerRequired，实得 %v", err)
	}
	rt := web.NewRouter(web.Config{})
	if err := rt.Handle("GET", "/ping", func(w http.ResponseWriter, r *http.Request) error {
		web.WriteJSON(w, "pong")
		return nil
	}); err != nil {
		t.Fatalf("Handle：%v", err)
	}
	srv, err := web.NewServer(web.ServerConfig{Router: rt, Addr: "127.0.0.1:0"})
	if err != nil {
		t.Fatalf("NewServer：%v", err)
	}
	sp := NewServer(srv)
	if sp.Name() != NameServer {
		t.Fatalf("插件名：%s", sp.Name())
	}
	k, err := kit.New(kit.Config{})
	if err != nil {
		t.Fatalf("kit.New：%v", err)
	}
	if err := k.Use(sp); err != nil {
		t.Fatalf("Use：%v", err)
	}
	// Build 前不承诺可用（未就绪时 Addr 交空，Serve 判 ErrServerNotReady）
	if got := sp.Addr(); got != "" {
		t.Fatalf("Build 前 Addr 应为空，实得 %q", got)
	}
	if err := sp.Serve(); !errors.Is(err, ErrServerNotReady) {
		t.Fatalf("Build 前 Serve 应判 ErrServerNotReady，实得 %v", err)
	}
	if err := k.Build(); err != nil {
		t.Fatalf("Build：%v", err)
	}
	if sp.Addr() != "127.0.0.1:0" {
		t.Fatalf("Build 后 Addr 未就绪：%q", sp.Addr())
	}
	if err := k.Stop(); err != nil {
		t.Fatalf("Stop：%v", err)
	}
	if err := sp.Serve(); !errors.Is(err, ErrServerStopped) {
		t.Fatalf("停止后 Serve 应判 ErrServerStopped，实得 %v", err)
	}
	if sp.Addr() != "" {
		t.Fatal("停止后仍暴露就绪地址")
	}
}
