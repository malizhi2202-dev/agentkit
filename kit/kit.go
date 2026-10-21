package kit

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/malizhi2202-dev/agentkit/kitctx"
	"github.com/malizhi2202-dev/agentkit/tools"
	"github.com/malizhi2202-dev/agentkit/web"
)

// Config 是装配根的输入。字段全部可选，缺省值一律取"无副作用"那一侧
// （未注入时钟 ⇒ 系统时钟，未注入日志 ⇒ 丢弃），与框架既有口径一致（kitctx.SystemClock/NopLogger）。
type Config struct {
	// Clock 注入时钟；nil ⇒ kitctx.SystemClock。
	Clock kitctx.Clock
	// Logger 注入日志；nil ⇒ kitctx.NopLogger。
	Logger kitctx.Logger
	// Tools 复用宿主已构造的工具注册表；nil ⇒ 内部新建一张空表。
	//
	// 允许传入既有表是有意为之：同一份工具声明可能已由宿主在其他阶段登记过，
	// 再造一张就会出现"两个注册表各握半套工具"（正是 L-014 反复踩的坑）。
	Tools *tools.Registry
	// Web 复用宿主已构造的路由表（`*web.Router` 满足 `web.Registrar`）。
	//
	// nil ⇒ 本装配根不提供路由扩展点，声明了「路由即工具」的插件会在 Declare 期报错，
	// 而不是"只登记了工具、路由却悄悄少了"（同源声明不允许半张表）。
	Web web.Registrar
}

// 装配阶段。
type stateKind int32

const (
	stateAssembling stateKind = iota // 可 Use
	stateBuilding                    // Declare/Apply 进行中
	stateReady                       // 可运行
	stateStopped                     // 已撤销，不可再用
)

func (s stateKind) String() string {
	switch s {
	case stateAssembling:
		return "assembling"
	case stateBuilding:
		return "building"
	case stateReady:
		return "ready"
	case stateStopped:
		return "stopped"
	default:
		return "unknown"
	}
}

// Kit 是装配根：持有插件表、扩展点句柄与撤销钩子。
//
// 零用户状态承诺（ADR-002）：本结构不持有会话、记忆、历史或任何业务数据 ——
// 只有"能力"与"生命周期钩子"。用户数据一律由调用方每次经 `kitctx.RequestContext` 携带。
//
// 加锁纪律（写死，别改）：**任何插件回调（Declare/Apply/撤销钩子）都在不持 `mu` 的状态下执行**，
// 否则插件里一句 `k.Plugins` / `k.OnStop` 就会自锁死。这类死锁只在真实装配时出现，
// 单测最容易漏，故由 TestKit_OnStopInsideApplyDoesNotDeadlock 专门盯。
type Kit struct {
	clock  kitctx.Clock
	logger kitctx.Logger
	tools  *tools.Registry
	web    web.Registrar // 宿主注入的路由扩展点（nil ⇒ 不提供）

	state atomic.Int32 // 无锁读：插件回调里也要读（存的是 stateKind）
	build atomic.Bool  // 防重入：Build 进行中不接受第二次 Build / 不接受 Use

	mu            sync.Mutex       // 护 slots / byName / order / slot 的 declared·applied 位
	stopMu        sync.Mutex       // 护撤销钩子与 applying
	applying      *slot            // 当前正在 Apply 的插件（OnStop 的归属；stopMu 护）
	declaredTools []string         // Declare 相结束时的工具名快照（检出"迟登记"用）
	slots         []*slot          // 注册顺序（内省与 Declare 相的遍历顺序）
	byName        map[string]*slot // 名字 → slot（依赖查重与解析用）
	order         []*slot          // Apply 顺序（拓扑排序结果，Build 成功后才写）
}

// New 构造装配根。
//
// 唯一的失败路径是"建不出空工具注册表"（`tools.NewRegistry` 零工具时按构造不会失败，
// 但这里不假设它永不失败 —— 忽略错误的习惯，比它想省掉的那几行更贵）。
func New(cfg Config) (*Kit, error) {
	k := &Kit{clock: cfg.Clock, logger: cfg.Logger, byName: make(map[string]*slot)}
	if k.clock == nil {
		k.clock = kitctx.SystemClock()
	}
	if k.logger == nil {
		k.logger = kitctx.NopLogger()
	}
	switch {
	case cfg.Tools != nil:
		k.tools = cfg.Tools
	default:
		r, err := tools.NewRegistry()
		if err != nil {
			return nil, fmt.Errorf("kit: 无法创建工具注册表：%w", err)
		}
		if r == nil {
			return nil, errors.New("kit: 无法创建工具注册表（返回 nil）")
		}
		k.tools = r
	}
	// 路由扩展点与工具表无关：宿主可以只注入路由表而自带 Tools。
	k.web = cfg.Web
	k.storeState(stateAssembling)
	return k, nil
}

// Use 登记插件（装配期专用，可多次调用）。
//
// 立刻拒绝：nil、空名、重名 —— 依赖与内省都按名字寻址，两个同名插件会让
// "这个能力是谁提供的"变成不可判定。`Enabled` 为 false 的插件**登记但整体跳过**
// （状态里仍然可见，便于确认"确实被拔掉了"，而不是悄悄地"在但没生效"）。
func (k *Kit) Use(ps ...Plugin) error {
	if k.build.Swap(true) {
		return fmt.Errorf("kit: 装配进行中（Build/Use 未结束），不接受新的 Use；当前状态 %s", k.now())
	}
	defer k.build.Store(false)

	k.mu.Lock()
	defer k.mu.Unlock()

	if st := k.now(); st != stateAssembling {
		return fmt.Errorf("kit: 当前状态 %s，不接受新的插件登记（只有装配期可以）", st)
	}
	var problems []string
	for _, p := range ps {
		if p == nil {
			problems = append(problems, ErrPluginNil.Error())
			continue
		}
		name := p.Name()
		if strings.TrimSpace(name) == "" {
			problems = append(problems, fmt.Sprintf("%v：%T", ErrPluginNameRequired, p))
			continue
		}
		if _, dup := k.byName[name]; dup {
			problems = append(problems, fmt.Sprintf("%v：%q", ErrPluginDuplicate, name))
			continue
		}
		s := &slot{p: p, name: name, enabled: true}
		if e, ok := p.(Enableable); ok && !e.Enabled() {
			s.enabled = false
		}
		if d, ok := p.(Dependent); ok {
			s.deps = append([]string(nil), d.Deps()...)
		}
		k.slots = append(k.slots, s)
		k.byName[name] = s
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "；"))
	}
	return nil
}

// Build 执行两阶段装配：Declare（全量收集问题）→ 依赖定序 → Apply（失败即逆序撤销）。
//
// 失败不留半初始化对象：
//   - 声明期有任何问题 ⇒ 一个都不 Apply，转 `stopped`，返回**全部**问题（L-096：一处红不得掩盖其余红）；
//   - 依赖缺失/成环 ⇒ 同上，且缺依赖一次性报全；
//   - Apply 期某插件失败或 panic ⇒ 已 Apply 的按逆序撤销钩子，再转 `stopped`；
//   - Apply 相偷偷往扩展点登记声明 ⇒ 判 `ErrLateDeclare`（闭集必须在装配期定死）。
func (k *Kit) Build() error {
	if st := k.now(); st != stateAssembling {
		return fmt.Errorf("kit: 当前状态 %s，不能 Build（每个 Kit 只装配一次）", st)
	}
	if k.build.Swap(true) {
		return errors.New("kit: 装配已在进行中（Build 不可重入）")
	}
	defer k.build.Store(false)
	k.storeState(stateBuilding)

	slots := k.snapshot()

	// ---- 阶段一 Declare：按注册顺序登记声明；问题全量收集（[]error 保住 %w 哨兵）----
	var problems []error
	for _, s := range slots {
		if !s.enabled {
			continue
		}
		d := &Declarer{owner: s.name, tools: k.tools, web: k.declaredWeb()}
		if err := safeDeclare(s.name, s.p.Declare, d); err != nil {
			problems = append(problems, err)
		}
		for _, m := range d.problems {
			problems = append(problems, errors.New(m))
		}
		k.markDeclared(s)
	}
	if len(problems) > 0 {
		k.storeState(stateStopped)
		return errors.Join(problems...)
	}
	k.declaredTools = append([]string(nil), k.tools.Names()...)

	// ---- 阶段二 依赖定序（注册顺序起算 ⇒ 结果确定，可快照比对）----
	order, err := resolveOrder(slots)
	if err != nil {
		k.storeState(stateStopped)
		return err
	}

	// ---- 阶段三 Apply：按拓扑序接线 ----
	var applied []*slot
	for _, s := range order {
		k.setApplying(s)
		applyErr := safeApply(s.name, s.p.Apply, k)
		k.setApplying(nil)
		if applyErr != nil {
			k.rollback(applied)
			k.storeState(stateStopped)
			return applyErr
		}
		k.markApplied(s)
		applied = append(applied, s)
	}

	// ---- 阶段四 迟登记检测 ----
	if drift := addedNames(k.declaredTools, k.tools.Names()); len(drift) > 0 {
		k.rollback(applied)
		k.storeState(stateStopped)
		return fmt.Errorf("%w：%v（Apply 相不得登记声明，请放到 Declare 相）", ErrLateDeclare, drift)
	}

	k.mu.Lock()
	k.order = order
	k.mu.Unlock()
	k.storeState(stateReady)
	k.logger.Info("kit: 装配完成", map[string]any{
		"plugins": len(order),
		"order":   slotNames(order),
		"tools":   k.tools.Len(),
	})
	return nil
}

// OnStop 登记撤销钩子，**只能在插件自己的 Apply 期间调用**。
//
// 卡这么紧的理由：可插拔的前提是可撤销。允许任何时刻随手挂钩子，
// 等价于允许"Stop 之后还有东西在跑"。Build 之后才需要启动的资源，请在自己的 Apply 里建好。
func (k *Kit) OnStop(f func()) error {
	if f == nil {
		return errors.New("kit: OnStop 需要非 nil 钩子")
	}
	if st := k.now(); st == stateStopped {
		return ErrKitStopped
	}
	k.stopMu.Lock()
	s := k.applying
	k.stopMu.Unlock()
	if s == nil {
		return fmt.Errorf("%w：OnStop 只能在插件 Apply 期间调用（当前 %s）", ErrNotBuilt, k.now())
	}
	k.stopMu.Lock()
	s.cleanups = append(s.cleanups, f)
	k.stopMu.Unlock()
	return nil
}

// Stop 按 Apply 的**逆序**撤销全部钩子（单个插件内部也是后登记先执行）。
//
// 幂等：重复调用返回 nil。已停止的 Kit 再 Use/Build/OnStop 一律判失败 —— 不假装还活着。
func (k *Kit) Stop() error {
	switch st := k.now(); st {
	case stateStopped:
		return nil
	case stateBuilding:
		return errors.New("kit: Build 进行中，请先让 Build 返回（它失败时已自行撤销）")
	}

	k.mu.Lock()
	order := append([]*slot(nil), k.order...)
	k.mu.Unlock()

	// 未进入 order 的已登记插件也扫一遍（防漏撤：Declare 成功但没轮到 Apply 的路径）。
	seen := make(map[*slot]bool, len(order))
	for _, s := range order {
		seen[s] = true
	}
	for _, s := range k.snapshot() {
		if !seen[s] && s.appliedFlag() {
			order = append(order, s)
		}
	}

	var problems []string
	for i := len(order) - 1; i >= 0; i-- {
		s := order[i]
		k.stopMu.Lock()
		cs := s.cleanups
		s.cleanups = nil
		k.stopMu.Unlock()
		for j := len(cs) - 1; j >= 0; j-- {
			k.runCleanup(cs[j], s.name, &problems)
		}
	}
	k.storeState(stateStopped)

	if len(problems) > 0 {
		return errors.Join(joinAll(problems)...)
	}
	return nil
}

// Tools 返回框架内**唯一**的工具注册表（永不 nil）。Build 之后按约定只读；
// 迟登记不靠约定兜，由 Build 末尾的快照比对实际检出（ErrLateDeclare）。
func (k *Kit) Tools() *tools.Registry { return k.tools }

// Clock 返回生效时钟（永不 nil）。
func (k *Kit) Clock() kitctx.Clock { return k.clock }

// Logger 返回生效日志（永不 nil）。
func (k *Kit) Logger() kitctx.Logger { return k.logger }

// Ready 报告装配是否完成且未被停止。
func (k *Kit) Ready() bool { return k.now() == stateReady }

// State 返回当前装配阶段名（内省用，免调用方去猜枚举）。
func (k *Kit) State() string { return k.now().String() }

// ApplyOrder 返回拓扑排序后的 Apply 顺序（Build 成功前为 nil）。
// 插拔与依赖声明是否真的生效，看这条最直接。
func (k *Kit) ApplyOrder() []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return slotNames(k.order)
}

// Plugins 返回插件状态快照（**注册顺序**，含被拔除者）。
func (k *Kit) Plugins() []Status {
	slots := k.snapshot()

	out := make([]Status, 0, len(slots))
	for _, s := range slots {
		st := Status{
			Name:     s.name,
			Enabled:  s.enabled,
			Declared: s.declaredFlag(),
			Applied:  s.appliedFlag(),
			Deps:     append([]string(nil), s.deps...),
		}
		if in, ok := s.p.(Introspectable); ok {
			info := in.Describe()
			if info.Name == "" {
				info.Name = s.name
			}
			st.Info = &info
		}
		out = append(out, st)
	}
	return out
}

// Status 是插件的装配状态。管理面渲染时沿用既有可见性口径（仅 admin 可见），由宿主执行。
type Status struct {
	Name     string   `json:"name"`
	Enabled  bool     `json:"enabled"`
	Declared bool     `json:"declared"`
	Applied  bool     `json:"applied"`
	Deps     []string `json:"deps,omitempty"`
	Info     *Info    `json:"info,omitempty"`
}

// ---- 内部 ----

type slot struct {
	p        Plugin
	name     string
	deps     []string
	enabled  bool
	declared bool
	applied  bool
	cleanups []func()
}

func (k *Kit) now() stateKind         { return stateKind(k.state.Load()) }
func (k *Kit) storeState(s stateKind) { k.state.Store(int32(s)) }

func (k *Kit) snapshot() []*slot {
	k.mu.Lock()
	defer k.mu.Unlock()
	return append([]*slot(nil), k.slots...)
}

func (k *Kit) markDeclared(s *slot) {
	k.mu.Lock()
	s.declared = true
	k.mu.Unlock()
}

func (k *Kit) markApplied(s *slot) {
	k.mu.Lock()
	s.applied = true
	k.mu.Unlock()
}

func (k *Kit) setApplying(s *slot) {
	k.stopMu.Lock()
	k.applying = s
	k.stopMu.Unlock()
}

func (s *slot) declaredFlag() bool { return s != nil && s.declared }
func (s *slot) appliedFlag() bool  { return s != nil && s.applied }

// safeDeclare / safeApply 把插件回调的 panic 转成 error：
// 一个坏插件不该能决定进程生死，更不能绕过 Build 的撤销流程。
func safeDeclare(name string, fn func(*Declarer) error, d *Declarer) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("kit: 插件 %q Declare panic：%v", name, r)
		}
	}()
	if e := fn(d); e != nil {
		err = fmt.Errorf("kit: 插件 %q Declare 失败：%w", name, e)
	}
	return err
}

func safeApply(name string, fn func(*Kit) error, k *Kit) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("kit: 插件 %q Apply panic：%v", name, r)
		}
	}()
	if e := fn(k); e != nil {
		err = fmt.Errorf("kit: 插件 %q Apply 失败：%w", name, e)
	}
	return err
}

func (k *Kit) rollback(applied []*slot) {
	for i := len(applied) - 1; i >= 0; i-- {
		s := applied[i]
		var problems []string
		k.stopMu.Lock()
		cs := s.cleanups
		s.cleanups = nil
		k.stopMu.Unlock()
		for j := len(cs) - 1; j >= 0; j-- {
			k.runCleanup(cs[j], s.name, &problems)
		}
		k.mu.Lock()
		s.applied = false
		k.mu.Unlock()
		for _, p := range problems {
			k.logger.Warn("kit: 装配失败后的撤销钩子报错", map[string]any{"plugin": s.name, "err": p})
		}
	}
}

func (k *Kit) runCleanup(f func(), plugin string, problems *[]string) {
	defer func() {
		if r := recover(); r != nil {
			*problems = append(*problems, fmt.Sprintf("kit: 插件 %q 的撤销钩子 panic：%v", plugin, r))
		}
	}()
	f()
}

// resolveOrder 拓扑排序：被依赖者在前；无依赖声明时严格按注册顺序 ⇒ 结果确定、可快照比对。
func resolveOrder(slots []*slot) ([]*slot, error) {
	enabled := make(map[string]*slot, len(slots))
	for _, s := range slots {
		if s.enabled {
			enabled[s.name] = s
		}
	}

	var problems []error
	for _, s := range slots {
		if !s.enabled {
			continue
		}
		for _, dep := range s.deps {
			t, ok := enabled[dep]
			switch {
			case !ok:
				problems = append(problems, fmt.Errorf("%w：%q 依赖 %q（未参与装配：不存在或已被拔除）", ErrMissingDependency, s.name, dep))
			case t == s:
				problems = append(problems, fmt.Errorf("%w：%q 依赖自身", ErrDependencyCycle, s.name))
			}
		}
	}
	if len(problems) > 0 {
		return nil, errors.Join(problems...)
	}

	const (
		visiting = 1
		done     = 2
	)
	mark := make(map[string]int, len(enabled))
	var order []*slot

	var visit func(s *slot, path []string) error
	visit = func(s *slot, path []string) error {
		switch mark[s.name] {
		case done:
			return nil
		case visiting:
			return fmt.Errorf("%w：%s", ErrDependencyCycle, strings.Join(append(path, s.name), " → "))
		}
		mark[s.name] = visiting
		for _, dep := range s.deps {
			if err := visit(enabled[dep], append(path, s.name)); err != nil {
				return err
			}
		}
		mark[s.name] = done
		order = append(order, s)
		return nil
	}
	for _, s := range slots {
		if s.enabled {
			if err := visit(s, nil); err != nil {
				return nil, err
			}
		}
	}
	return order, nil
}

func joinAll(msgs []string) []error {
	out := make([]error, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, errors.New(m))
	}
	return out
}

func slotNames(slots []*slot) []string {
	if len(slots) == 0 {
		return nil
	}
	out := make([]string, 0, len(slots))
	for _, s := range slots {
		out = append(out, s.name)
	}
	return out
}

// addedNames 返回 after 相对 before 新增的名字（升序，便于稳定断言）。
func addedNames(before, after []string) []string {
	seen := make(map[string]bool, len(before))
	for _, n := range before {
		seen[n] = true
	}
	var out []string
	for _, n := range after {
		if !seen[n] {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// declaredRoutes 是声明期看到的路由视图：只转发 `web.Registrar` 的三个方法。
//
// 为什么必须包这一层而不是直接交 `web.Registrar`：接口值的**动态类型**仍是 `*web.Router`，
// 插件里一句 `.(interface{ Handler ... })` 就能拿到封存整张路由表的能力，
// "窄接口"就退化成注释。包一层后动态类型只有这三个方法，断言拿不到别的。
type declaredRoutes struct{ inner web.Registrar }

func (d declaredRoutes) Register(r web.Route, h web.Handler) error { return d.inner.Register(r, h) }
func (d declaredRoutes) Routes() []web.Route                       { return d.inner.Routes() }
func (d declaredRoutes) Invoke(ctx context.Context, name string, args map[string]any) (web.InvokeResult, error) {
	return d.inner.Invoke(ctx, name, args)
}

// declaredWeb 交出声明期视图；未配置路由表 ⇒ nil（调用方必须处理，不给你一个"什么都不做"的假表）。
func (k *Kit) declaredWeb() web.Registrar {
	if k == nil || k.web == nil {
		return nil
	}
	return declaredRoutes{inner: k.web}
}
