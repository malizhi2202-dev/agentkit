package kit

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"agentkit/tools"
)

// ---- 测试脚手架 ----

// fake 是可编排的假插件：每个阶段的行为都由闭包给出，断言点写在测试里。
type fake struct {
	name    string
	enabled *bool
	deps    []string
	info    *Info
	declare func(d *Declarer) error
	apply   func(k *Kit) error
	cleanup func()
	log     *[]string
	logMu   *sync.Mutex
}

func (f *fake) Name() string { return f.name }

func (f *fake) Declare(d *Declarer) error {
	f.record("declare:" + f.name)
	if f.declare != nil {
		return f.declare(d)
	}
	return nil
}

func (f *fake) Apply(k *Kit) error {
	f.record("apply:" + f.name)
	if f.apply != nil {
		return f.apply(k)
	}
	if f.cleanup != nil {
		if err := k.OnStop(f.cleanup); err != nil {
			return err
		}
	}
	return nil
}

func (f *fake) Deps() []string { return f.deps }

func (f *fake) Enabled() bool {
	if f.enabled == nil {
		return true
	}
	return *f.enabled
}

func (f *fake) Describe() Info {
	if f.info != nil {
		return *f.info
	}
	return Info{Name: f.name, Version: "test"}
}

func (f *fake) record(s string) {
	if f.log == nil {
		return
	}
	if f.logMu != nil {
		f.logMu.Lock()
		defer f.logMu.Unlock()
	}
	*f.log = append(*f.log, s)
}

// tracer 记录 Apply / 撤销钩子的**先后顺序**（插拔与生命周期的直接证据）。
type tracer struct {
	mu   sync.Mutex
	seq  []string
	stop []string
}

func (t *tracer) note(s string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.seq = append(t.seq, s)
}

func (t *tracer) noteStop(s string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.stop = append(t.stop, s)
}

func (t *tracer) applyPlugin(name string, extra func()) func(k *Kit) error {
	return func(k *Kit) error {
		t.note("apply:" + name)
		if err := k.OnStop(func() {
			t.noteStop("stop:" + name)
			if extra != nil {
				extra()
			}
		}); err != nil {
			return err
		}
		if err := k.OnStop(func() { t.noteStop("stop2:" + name) }); err != nil { // 同插件第二个钩子
			return err
		}
		return nil
	}
}

func newT(t *testing.T, cfg Config) *Kit {
	t.Helper()
	k, err := New(cfg)
	if err != nil {
		t.Fatalf("New 失败：%v", err)
	}
	return k
}

func tool(name string) tools.Tool {
	return tools.Tool{
		Name:          name,
		Summary:       name + " 的摘要",
		RequiredRoles: []string{"admin"},
		Run:           func(context.Context, map[string]any) (any, error) { return name, nil },
	}
}

// ---- 基线：空 Kit 可跑 ----

func TestKit_EmptyKitBuildsRunsAndStops(t *testing.T) {
	k := newT(t, Config{})
	if k.Ready() {
		t.Fatal("Build 之前 Ready() 不该为真 ⇒ 半初始化对象可用")
	}
	if k.Tools() == nil {
		t.Fatal("Tools() 不得为 nil（框架承诺永不返回 nil 句柄）")
	}
	if k.Clock() == nil || k.Logger() == nil {
		t.Fatal("Clock()/Logger() 不得为 nil")
	}
	if err := k.Build(); err != nil {
		t.Fatalf("空 Kit 应当能 Build：%v", err)
	}
	if !k.Ready() || k.State() != "ready" {
		t.Fatalf("Build 后状态应为 ready，实得 %q", k.State())
	}
	if got := k.Tools().Len(); got != 0 {
		t.Fatalf("空 Kit 工具数应为 0，实得 %d", got)
	}
	if err := k.Stop(); err != nil {
		t.Fatalf("Stop 失败：%v", err)
	}
	if k.Ready() {
		t.Fatal("Stop 后仍 Ready ⇒ 撤销不生效")
	}
	if err := k.Stop(); err != nil {
		t.Fatalf("Stop 必须幂等，第二次返回：%v", err)
	}
	// 停止后不接受新的生命周期动作（不假装还活着）。
	if err := k.Use(&fake{name: "late"}); !errors.Is(err, ErrKitStopped) && err == nil {
		t.Fatalf("Stop 后 Use 应判失败，实得 %v", err)
	}
	if err := k.Build(); err == nil {
		t.Fatal("Stop 后 Build 应判失败")
	}
	if err := k.OnStop(func() {}); !errors.Is(err, ErrKitStopped) {
		t.Fatalf("Stop 后 OnStop 应判 ErrKitStopped，实得 %v", err)
	}
}

func TestKit_ReuseHostToolsRegistry(t *testing.T) {
	host, err := tools.NewRegistry(tool("already"))
	if err != nil {
		t.Fatalf("宿主注册表构造失败：%v", err)
	}
	k := newT(t, Config{Tools: host})
	if k.Tools() != host {
		t.Fatal("Config.Tools 必须**复用**同一张表，不能复制或另建（L-014）")
	}
	if err := k.Build(); err != nil {
		t.Fatalf("Build 失败：%v", err)
	}
	if got := k.Tools().Names(); len(got) != 1 || got[0] != "already" {
		t.Fatalf("宿主已登记的工具应可见，实得 %v", got)
	}
}

// ---- 反证 1：登记面拒绝 nil / 空名 / 重名 ----

func TestKit_UseRejectsBadPlugins(t *testing.T) {
	cases := []struct {
		name  string
		use   []Plugin
		want  string
		check func(t *testing.T, err error)
	}{
		{"nil 插件", []Plugin{nil}, "插件不能为 nil", nil},
		{"空名", []Plugin{&fake{name: "   "}}, "Name() 不能为空", nil},
		{"一次调用内重名", []Plugin{&fake{name: "dup"}, &fake{name: "dup"}}, "插件名重复", nil},
		{"跨调用重名", nil, "插件名重复", func(t *testing.T, err error) {
			k := newT(t, Config{})
			if e := k.Use(&fake{name: "dup"}); e != nil {
				t.Fatalf("首次登记应成功：%v", e)
			}
			err = k.Use(&fake{name: "dup"})
			if err == nil || !strings.Contains(err.Error(), "dup") {
				t.Fatalf("跨调用重名应判失败并点名，实得 %v", err)
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			k := newT(t, Config{})
			var err error
			if c.check != nil {
				c.check(t, err)
				return
			}
			err = k.Use(c.use...)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("期望包含 %q，实得 %v", c.want, err)
			}
			// 违规必须**整批**留下痕迹：不能一条违规把同批其它问题掩盖掉。
			if !strings.Contains(err.Error(), "kit:") {
				t.Fatalf("错误信息应带 kit: 前缀，实得 %v", err)
			}
		})
	}
}

// ---- 反证 2：声明期问题必须**全量**报出（L-096），且不进入 Apply ----

func TestKit_DeclareProblemsAreAllCollectedAndNoApply(t *testing.T) {
	var log []string
	var logMu sync.Mutex

	a := &fake{name: "a", log: &log, logMu: &logMu, declare: func(d *Declarer) error {
		d.Errorf("第一条声明问题")
		d.Errorf("第二条声明问题")
		return errors.New("a 自己返回的错误")
	}}
	b := &fake{name: "b", log: &log, logMu: &logMu, declare: func(d *Declarer) error {
		return errors.New("b 也失败了")
	}}
	c := &fake{name: "c", log: &log, logMu: &logMu}

	k := newT(t, Config{})
	if err := k.Use(a, b, c); err != nil {
		t.Fatalf("Use 失败：%v", err)
	}
	err := k.Build()
	if err == nil {
		t.Fatal("声明期有问题时 Build 必须失败")
	}
	for _, want := range []string{"第一条声明问题", "第二条声明问题", "a 自己返回的错误", "b 也失败了"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("问题未被报出（一处红掩盖了其余红）：%q\n完整错误：%v", want, err)
		}
	}
	if strings.Contains(err.Error(), "c 的") {
		t.Errorf("不相关插件的噪声不该出现")
	}
	if k.Ready() {
		t.Fatal("声明期失败后不得进入 ready")
	}
	if k.State() != "stopped" {
		t.Fatalf("失败后应为 stopped（不可再用），实得 %q", k.State())
	}
	// 一个插件都不能被 Apply（Apply 只发生在全部声明成功之后）。
	logMu.Lock()
	defer logMu.Unlock()
	for _, l := range log {
		if strings.HasPrefix(l, "apply:") {
			t.Fatalf("声明期失败却执行了 Apply：%v", log)
		}
	}
	for _, st := range k.Plugins() {
		if st.Applied {
			t.Errorf("插件 %q 不该处于 applied 状态", st.Name)
		}
	}
}

// ---- 反证 3：依赖缺失 / 成环（含"拔除导致缺依赖"）----

func TestKit_DependencyFailures(t *testing.T) {
	off := false

	cases := []struct {
		name  string
		pls   []Plugin
		is    error
		wantS string
	}{
		{
			name:  "依赖不存在的插件",
			pls:   []Plugin{&fake{name: "app", deps: []string{"ghost"}}},
			is:    ErrMissingDependency,
			wantS: "ghost",
		},
		{
			name:  "依赖被拔除的插件（不得静默降级）",
			pls:   []Plugin{&fake{name: "core", enabled: &off}, &fake{name: "app", deps: []string{"core"}}},
			is:    ErrMissingDependency,
			wantS: "已被拔除",
		},
		{
			name:  "环形依赖",
			pls:   []Plugin{&fake{name: "a", deps: []string{"b"}}, &fake{name: "b", deps: []string{"a"}}},
			is:    ErrDependencyCycle,
			wantS: "a → b → a",
		},
		{
			name:  "自依赖",
			pls:   []Plugin{&fake{name: "solo", deps: []string{"solo"}}},
			is:    ErrDependencyCycle,
			wantS: "依赖自身",
		},
		{
			name: "多个插件同时缺依赖 ⇒ 全部报出",
			pls: []Plugin{
				&fake{name: "x", deps: []string{"miss-1"}},
				&fake{name: "y", deps: []string{"miss-2"}},
			},
			is:    ErrMissingDependency,
			wantS: "miss-2",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			k := newT(t, Config{})
			if err := k.Use(c.pls...); err != nil {
				t.Fatalf("Use 失败：%v", err)
			}
			err := k.Build()
			if err == nil {
				t.Fatal("期望 Build 失败")
			}
			if !errors.Is(err, c.is) {
				t.Fatalf("哨兵不可判定（errors.Is(%v) = false）：%v", c.is, err)
			}
			if !strings.Contains(err.Error(), c.wantS) {
				t.Fatalf("错误应含 %q，实得 %v", c.wantS, err)
			}
			if k.Ready() {
				t.Fatal("依赖失败后不得 ready")
			}
		})
	}
}

// ---- 插拔生效：拓扑顺序 + 被拔除者不参与 ----

func TestKit_TopologicalOrderAndPlugging(t *testing.T) {
	tr := &tracer{}
	off := false
	core := &fake{name: "core"}
	app := &fake{name: "app", deps: []string{"core", "extra"}}
	extra := &fake{name: "extra", deps: []string{"core"}}
	skipped := &fake{name: "disabled-one", enabled: &off}
	core.apply = tr.applyPlugin("core", nil)
	app.apply = tr.applyPlugin("app", nil)
	extra.apply = tr.applyPlugin("extra", nil)
	skipped.apply = tr.applyPlugin("disabled-one", nil)

	k := newT(t, Config{})
	if err := k.Use(app, extra, core, skipped); err != nil { // 注册顺序故意打乱
		t.Fatalf("Use 失败：%v", err)
	}
	if err := k.Build(); err != nil {
		t.Fatalf("Build 失败：%v", err)
	}
	want := []string{"core", "extra", "app"} // 被依赖者先接线
	got := k.ApplyOrder()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("Apply 顺序应为 %v，实得 %v", want, got)
	}

	// 撤销必须严格按 Apply 逆序，且同插件内后登记先执行。
	if err := k.Stop(); err != nil {
		t.Fatalf("Stop 失败：%v", err)
	}
	tr.mu.Lock()
	defer tr.mu.Unlock()
	wantStop := []string{"stop2:app", "stop:app", "stop2:extra", "stop:extra", "stop2:core", "stop:core"}
	if strings.Join(tr.stop, ",") != strings.Join(wantStop, ",") {
		t.Fatalf("撤销顺序应为 %v，实得 %v", wantStop, tr.stop)
	}
	// 被拔除的插件既没声明也没接线（"关掉即不在"）。
	for _, s := range k.Plugins() {
		if s.Name == "disabled-one" && (s.Declared || s.Applied || s.Enabled) {
			t.Fatalf("被拔除插件不应参与任何阶段：%+v", s)
		}
	}
}

func TestKit_NoDepsKeepsRegistrationOrder(t *testing.T) {
	k := newT(t, Config{})
	if err := k.Use(&fake{name: "z"}, &fake{name: "a"}, &fake{name: "m"}); err != nil {
		t.Fatalf("Use 失败：%v", err)
	}
	if err := k.Build(); err != nil {
		t.Fatalf("Build 失败：%v", err)
	}
	got := strings.Join(k.ApplyOrder(), ",")
	if got != "z,a,m" {
		t.Fatalf("无依赖声明时顺序必须等于注册顺序（确定性要求），实得 %q", got)
	}
	names := make([]string, 0, 3)
	for _, s := range k.Plugins() {
		names = append(names, s.Name)
	}
	if strings.Join(names, ",") != "z,a,m" {
		t.Fatalf("内省必须按注册顺序，实得 %v", names)
	}
	if k.Plugins()[0].Info == nil || k.Plugins()[0].Info.Version != "test" {
		t.Fatal("Introspectable 的自述未进入内省结果")
	}
}

// ---- 反证 4：Apply 失败 / panic ⇒ 逆序撤销，不留活口 ----

func TestKit_ApplyFailureRollsBackInReverse(t *testing.T) {
	tr := &tracer{}
	a := &fake{name: "a"}
	b := &fake{name: "b"}
	b.apply = func(k *Kit) error { return errors.New("b 起不来") }
	a.apply = tr.applyPlugin("a", nil)

	k := newT(t, Config{})
	if err := k.Use(a, b); err != nil {
		t.Fatalf("Use 失败：%v", err)
	}
	err := k.Build()
	if err == nil || !strings.Contains(err.Error(), "b 起不来") {
		t.Fatalf("Apply 失败必须上抛，实得 %v", err)
	}
	if k.State() != "stopped" {
		t.Fatalf("失败后应为 stopped，实得 %q", k.State())
	}
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if strings.Join(tr.stop, ",") != "stop2:a,stop:a" {
		t.Fatalf("已成功接线的 a 必须被逆序撤销，实得 %v", tr.stop)
	}
	if pluginApplied(k, "b") {
		t.Fatal("失败的插件不该标记 applied")
	}
}

func pluginApplied(k *Kit, name string) bool {
	for _, s := range k.Plugins() {
		if s.Name == name {
			return s.Applied
		}
	}
	return false
}

func TestKit_PanicInApplyIsCaughtAndRolledBack(t *testing.T) {
	tr := &tracer{}
	good := &fake{name: "good"}
	good.apply = tr.applyPlugin("good", nil)
	bad := &fake{name: "bad", apply: func(*Kit) error { panic("插件内部炸了") }}

	k := newT(t, Config{})
	if err := k.Use(good, bad); err != nil {
		t.Fatalf("Use 失败：%v", err)
	}
	err := k.Build() // 不让 panic 炸穿测试进程
	if err == nil || !strings.Contains(err.Error(), "panic") {
		t.Fatalf("Apply panic 应转成错误，实得 %v", err)
	}
	if k.Ready() {
		t.Fatal("panic 后不得 ready")
	}
	tr.mu.Lock()
	gotStop := strings.Join(tr.stop, ",")
	tr.mu.Unlock()
	if !strings.Contains(gotStop, "stop:good") {
		t.Fatalf("panic 也必须走撤销流程，实得 %v", gotStop)
	}
}

func TestKit_PanicInDeclareIsCaught(t *testing.T) {
	k := newT(t, Config{})
	if err := k.Use(&fake{name: "boom", declare: func(*Declarer) error { panic("声明期炸") }}); err != nil {
		t.Fatalf("Use 失败：%v", err)
	}
	if err := k.Build(); err == nil || !strings.Contains(err.Error(), "Declare panic") {
		t.Fatalf("期望 Declare panic 被转成错误，实得 %v", err)
	}
}

func TestKit_CleanupPanicStillStopsTheRest(t *testing.T) {
	tr := &tracer{}
	a := &fake{name: "a", apply: func(k *Kit) error {
		if err := k.OnStop(func() { panic("撤销炸了") }); err != nil {
			return err
		}
		return nil
	}}
	b := &fake{name: "b", apply: tr.applyPlugin("b", nil)}

	k := newT(t, Config{})
	if err := k.Use(a, b); err != nil {
		t.Fatalf("Use 失败：%v", err)
	}
	if err := k.Build(); err != nil {
		t.Fatalf("Build 失败：%v", err)
	}
	err := k.Stop()
	if err == nil || !strings.Contains(err.Error(), "撤销钩子 panic") {
		t.Fatalf("撤销 panic 必须可见，实得 %v", err)
	}
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if !strings.Contains(strings.Join(tr.stop, ","), "stop:b") {
		t.Fatalf("一个钩子 panic 不得影响其余钩子执行，实得 %v", tr.stop)
	}
}

// ---- 反证 5：迟登记（Apply 相往扩展点塞声明）⇒ 实际检出 ----

func TestKit_LateToolRegistrationDetected(t *testing.T) {
	k := newT(t, Config{})
	if err := k.Use(&fake{name: "sneaky", declare: func(d *Declarer) error {
		return d.Tools().Register(tool("declared-in-declare"))
	}, apply: func(k *Kit) error {
		return k.Tools().Register(tool("sneaked-in-apply")) // 不该发生
	}}); err != nil {
		t.Fatalf("Use 失败：%v", err)
	}
	err := k.Build()
	if !errors.Is(err, ErrLateDeclare) {
		t.Fatalf("期望 ErrLateDeclare，实得 %v", err)
	}
	if !strings.Contains(err.Error(), "sneaked-in-apply") {
		t.Fatalf("错误必须点名是哪条迟登记，实得 %v", err)
	}
	if k.Ready() {
		t.Fatal("迟登记检出后不得 ready")
	}
}

// ---- 反证 6：Declare 相**类型上**拿不到 Kit；扩展点校验不复制 ----

func TestKit_DeclarerCannotReachKit(t *testing.T) {
	rt := reflect.TypeOf((*Declarer)(nil))
	kitT := reflect.TypeOf((*Kit)(nil))
	kitStr := kitT.String() // "*kit.Kit"：用于识别藏在切片/映射/指针里的句柄

	names := make([]string, 0, rt.NumMethod())
	for i := 0; i < rt.NumMethod(); i++ {
		m := rt.Method(i)
		names = append(names, m.Name)
		// 返回值与**入参**都不能带 *Kit：入参能把它存进 Declarer 字段，效果和返回值泄漏一样。
		// （接收者固定是 *Declarer，故从 0 扫不会误报。）
		for j := 0; j < m.Type.NumOut(); j++ {
			if m.Type.Out(j) == kitT {
				t.Errorf("Declarer.%s 返回 *Kit ⇒ Declare 相摸到装配根，两阶段边界失效", m.Name)
			}
		}
		for j := 0; j < m.Type.NumIn(); j++ {
			if m.Type.In(j) == kitT {
				t.Errorf("Declarer.%s 以 *Kit 为入参 ⇒ 可以被存起来留到 Declare 相之后用，边界失效", m.Name)
			}
		}
	}
	// Web：P5 落「路由即工具」时新增。它只是**第二个登记入口**（与 Tools 同性质），
	// 且交出的是窄接口 `web.Registrar`（无 Handler/Use），
	// 因此既给不了装配根句柄、也给不了"提前封存全局"的能力 —— 两阶段边界仍然成立。
	want := []string{"Errorf", "Owner", "Tools", "Web"} // 多一个都要说明理由
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("Declarer 方法集应为 %v，实得 %v", want, names)
	}
	// 字段（含未导出）里不能藏 *Kit，也不能藏在切片/映射里。
	st := reflect.TypeOf(Declarer{})
	for i := 0; i < st.NumField(); i++ {
		ft := st.Field(i).Type
		if ft == kitT || strings.Contains(ft.String(), kitStr) {
			t.Errorf("Declarer 藏了装配根字段 %q (%s) ⇒ 两阶段边界形同虚设", st.Field(i).Name, ft)
		}
	}
	// ⚠️ 已知边界（写清楚，别让读者以为这里证得比实际更多）：
	// reflect **无法枚举未导出方法**，故"未导出方法收 *Kit 参数"这一条本测试看不见。
	// 它只在包内可调（框架自己递给它才行），故由「字段扫描 + 方法集恰好三个」共同兜住：
	// 想留下句柄必须有字段可放，而任何新增字段都会被上面这条断言抓到。
	if st.NumField() != 4 { // 第 4 个是 web（窄接口，非 *Kit，不破坏两阶段边界）
		t.Fatalf("Declarer 字段数变化（当前 %d），请复核两阶段边界是否仍成立", st.NumField())
	}
}

func TestKit_DeclarePhaseReusesToolsSingleValidation(t *testing.T) {
	// 半成品声明（Run==nil）与重名必须仍由 tools.Registry 那**一个**谓词拒绝，
	// kit 内不存在第二套校验（L-014/R3）。
	k := newT(t, Config{})
	if err := k.Use(&fake{name: "a", declare: func(d *Declarer) error {
		return d.Tools().Register(tools.Tool{Name: "no-run"}) // 缺 Run
	}}); err != nil {
		t.Fatalf("Use 失败：%v", err)
	}
	err := k.Build()
	if err == nil || !strings.Contains(err.Error(), "Run") {
		t.Fatalf("期望工具表拒绝半成品声明，实得 %v", err)
	}
	if strings.Contains(err.Error(), "kit: 校验失败") {
		t.Fatal("kit 不得自己实现一套声明校验")
	}

	k2 := newT(t, Config{})
	dup := func(d *Declarer) error { return d.Tools().Register(tool("same-name")) }
	if err := k2.Use(&fake{name: "p1", declare: dup}, &fake{name: "p2", declare: dup}); err != nil {
		t.Fatalf("Use 失败：%v", err)
	}
	err = k2.Build()
	if err == nil || !errors.Is(err, tools.ErrToolDuplicate) {
		t.Fatalf("跨插件重名应透出 tools.ErrToolDuplicate，实得 %v", err)
	}
}

// ---- 钩子使用面：不在 Apply 相就别想挂 ----

func TestKit_OnStopInsideApplyDoesNotDeadlock(t *testing.T) {
	k := newT(t, Config{})
	if err := k.Use(&fake{name: "wired", apply: func(k *Kit) error {
		for i := 0; i < 8; i++ {
			if err := k.OnStop(func() {}); err != nil {
				return err
			}
		}
		return nil
	}}); err != nil {
		t.Fatalf("Use 失败：%v", err)
	}
	done := make(chan error, 1)
	go func() { done <- k.Build() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Build 失败：%v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Build 持锁调用插件回调 ⇒ 自锁死（加锁纪律被破坏）")
	}
	// Build 之外没有归属插件 ⇒ 不接受钩子。
	if err := k.OnStop(func() {}); !errors.Is(err, ErrNotBuilt) {
		t.Fatalf("Build 之后 OnStop 应判 ErrNotBuilt，实得 %v", err)
	}
	if err := k.Stop(); err != nil {
		t.Fatalf("Stop 失败：%v", err)
	}
}

func TestKit_OnStopNilAndBadState(t *testing.T) {
	k := newT(t, Config{})
	if err := k.OnStop(nil); err == nil {
		t.Fatal("nil 钩子必须判失败")
	}
	if err := k.OnStop(func() {}); !errors.Is(err, ErrNotBuilt) {
		t.Fatalf("Build 之前 OnStop 应判 ErrNotBuilt，实得 %v", err)
	}
}

func TestKit_DoubleBuildRejected(t *testing.T) {
	k := newT(t, Config{})
	if err := k.Build(); err != nil {
		t.Fatalf("首次 Build 失败：%v", err)
	}
	if err := k.Build(); err == nil || !strings.Contains(err.Error(), "不能 Build") {
		t.Fatalf("重复 Build 必须判失败，实得 %v", err)
	}
	if err := k.Use(&fake{name: "after"}); err == nil {
		t.Fatal("Build 之后 Use 必须判失败（装配期已过）")
	}
}

// ---- 并发使用面（-race 的落点）----

func TestKit_ConcurrentAccessorsAreRaceFree(t *testing.T) {
	tr := &tracer{}
	k := newT(t, Config{})
	for i := 0; i < 6; i++ {
		name := fmt.Sprintf("p%d", i)
		p := &fake{name: name}
		p.apply = tr.applyPlugin(name, nil)
		if err := k.Use(p); err != nil {
			t.Fatalf("Use(%s) 失败：%v", name, err)
		}
	}
	if err := k.Build(); err != nil {
		t.Fatalf("Build 失败：%v", err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = k.Plugins()
			_ = k.ApplyOrder()
			_ = k.State()
			_ = k.Ready()
			_ = k.Tools().Names()
		}()
	}
	wg.Wait()
	if err := k.Stop(); err != nil {
		t.Fatalf("Stop 失败：%v", err)
	}
	tr.mu.Lock()
	n := len(tr.stop)
	tr.mu.Unlock()
	if n != 12 {
		t.Fatalf("6 个插件 × 2 个钩子应全部撤销，实得 %d 条", n)
	}
}
