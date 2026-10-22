// Package web 是可插拔的 HTTP 入口层：静态路由表 + 中间件链 + JSON 编解码 + 错误闭集。
//
// 它是框架里**唯一**允许使用入域（服务端）HTTP 符号的包（框架裁决 的 D2 基线：
// 入域符号所在包集合 ⊆ {web}）。出域能力仍只在 `model`：本包不 import 客户端类型，
// 也不得有任何函数返回 `*http.Response`（守卫旁路判据）。
//
// 三条刻意的取舍（都不是审美偏好）：
//
//  1. **只做静态路由**。参数段（`:id`、`{id}`）、通配、自动重定向都不支持，注册期即拒绝并说明原因。
//     理由：“ 的 route↔tool 同源要求"路由表可枚举、可静态比对"，
//     一旦引入模式匹配，"某角色可访问的路由集 ≡ 可见工具集"这条判据就必须在运行期展开通配，
//     退化成猜。宿主需要参数段时用注入的解析中间件把值放进 ctx，本包不替它发明语法。
//  2. **状态码可注入**（框架裁决）。本包内置一张 `ErrKind → 真实状态码` 的默认表，
//     宿主可整体覆盖，以便继续用 `宿主错误包`/`宿主错误包` 的既有码段口径；
//     错误对外的稳定标识是 `ErrKind`（闭集），不是状态码、也不是错误文本。
//  3. **响应封套可注入，默认与宿主同形**（`{code,msg,data}`，成功 `code=="0"`）。
//     封套形状属宿主协议，不属框架不变量；默认值只为"开箱能跑"，不承诺跨宿主一致。
//
// 装配口径：路由与中间件在**首次使用前**登记完（`Handler` 一旦调用即封存），
// 之后任何注册尝试都返回 `ErrSealed` —— 与 `kit` 的两阶段注册同一套纪律，
// 避免"边跑边改表"造成同一进程内两次请求看到不同的路由集。
package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/malizhi2202-dev/agentkit/kitctx"
)

// ErrKind 是错误类别闭集。宿主按它映射状态码与业务码，日志与告警也按它 grep。
type ErrKind string

const (
	ErrKindInvalidInput     ErrKind = "invalid_input"
	ErrKindUnauthenticated  ErrKind = "unauthenticated"
	ErrKindForbidden        ErrKind = "forbidden"
	ErrKindNotFound         ErrKind = "not_found"
	ErrKindMethodNotAllowed ErrKind = "method_not_allowed"
	ErrKindTooLarge         ErrKind = "payload_too_large"
	ErrKindRateLimited      ErrKind = "too_many_requests"
	ErrKindInternal         ErrKind = "internal"
	ErrKindUnavailable      ErrKind = "unavailable"
)

var errKinds = map[ErrKind]bool{
	ErrKindInvalidInput: true, ErrKindUnauthenticated: true, ErrKindForbidden: true,
	ErrKindNotFound: true, ErrKindMethodNotAllowed: true, ErrKindTooLarge: true,
	ErrKindRateLimited: true, ErrKindInternal: true, ErrKindUnavailable: true,
}

// Valid 报告类别是否在闭集内。
func (k ErrKind) Valid() bool { return errKinds[k] }

// Error 让类别可以直接作为 `errors.Is` 的目标（`errors.Is(err, ErrKindForbidden)`）。
//
// 类别是对外的稳定标识，因此"是不是这一类"必须可判，而不必依赖调用方记住 `*Error` 这层包装。
// 代价是 `return ErrKindNotFound` 也能编译 —— 这是允许的同义写法，`KindOf` 两条都认。
func (k ErrKind) Error() string { return string(k) }

// Kinds 返回闭集成员（字典序，供管理面展示与判据比对）。
func Kinds() []string {
	out := make([]string, 0, len(errKinds))
	for k := range errKinds {
		out = append(out, string(k))
	}
	sort.Strings(out)
	return out
}

// Error 是带类别的错误。未导出字段为 0 个语义字段之外的最小集合：类别、消息、原因。
type Error struct {
	Kind  ErrKind
	Msg   string
	Cause error
}

// Error 实现 error。
func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Cause != nil {
		return fmt.Sprintf("%s: %s: %v", string(e.Kind), e.Msg, e.Cause)
	}
	return fmt.Sprintf("%s: %s", string(e.Kind), e.Msg)
}

// Is 支持 `errors.Is(err, ErrKindXxx)`：类别相同即同一类错误（不看消息）。
func (e *Error) Is(target error) bool {
	k, ok := target.(ErrKind)
	return ok && e != nil && e.Kind == k
}

// Unwrap 让 errors.Is/As 能穿过类别层。
func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// NewError 造一个带类别的错误（类别不在闭集内 ⇒ 归为 internal，不静默丢弃原因）。
func NewError(kind ErrKind, msg string, cause error) *Error {
	if !kind.Valid() {
		cause = errors.Join(fmt.Errorf("web: 未知错误类别 %q，按 internal 处理", string(kind)), cause)
		kind = ErrKindInternal
	}
	if msg == "" {
		msg = string(kind)
	}
	return &Error{Kind: kind, Msg: msg, Cause: cause}
}

// Errorf 造带格式化消息的类别错误。
func Errorf(kind ErrKind, format string, args ...any) *Error {
	return NewError(kind, fmt.Sprintf(format, args...), nil)
}

// KindOf 从错误链里取类别；取不到 ⇒ ok=false（调用方决定默认类别，本包不猜）。
func KindOf(err error) (ErrKind, bool) {
	var e *Error
	if errors.As(err, &e) && e.Kind.Valid() {
		return e.Kind, true
	}
	var k ErrKind
	if errors.As(err, &k) && k.Valid() {
		return k, true
	}
	return "", false
}

// StatusMap 把类别映射到真实 HTTP 状态码。
type StatusMap func(kind ErrKind) int

// DefaultStatus 返回内置映射。未收录类别（含宿主自定义类别）⇒ 500，而不是 200 或 0。
func DefaultStatus() StatusMap {
	table := map[ErrKind]int{
		ErrKindInvalidInput:     http.StatusBadRequest,
		ErrKindUnauthenticated:  http.StatusUnauthorized,
		ErrKindForbidden:        http.StatusForbidden,
		ErrKindNotFound:         http.StatusNotFound,
		ErrKindMethodNotAllowed: http.StatusMethodNotAllowed,
		ErrKindTooLarge:         http.StatusRequestEntityTooLarge,
		ErrKindRateLimited:      http.StatusTooManyRequests,
		ErrKindInternal:         http.StatusInternalServerError,
		ErrKindUnavailable:      http.StatusServiceUnavailable,
	}
	return func(kind ErrKind) int {
		if code, ok := table[kind]; ok {
			return code
		}
		return http.StatusInternalServerError
	}
}

// Envelope 是默认响应封套（与宿主既有形状同形：code 为字符串，成功 "0"）。
type Envelope struct {
	Code string `json:"code"`
	Msg  string `json:"msg"`
	Data any    `json:"data"`
}

// CodeOK 是成功业务码（沿用宿主取值）。
const CodeOK = "0"

// Renderer 负责写响应。实现必须自行设置 Content-Type 与状态码。
type Renderer interface {
	Render(w http.ResponseWriter, status int, kind ErrKind, msg string, data any)
}

// RendererFunc 让宿主一行接入。
type RendererFunc func(w http.ResponseWriter, status int, kind ErrKind, msg string, data any)

// Render 实现 Renderer。
func (f RendererFunc) Render(w http.ResponseWriter, status int, kind ErrKind, msg string, data any) {
	f(w, status, kind, msg, data)
}

// JSONRenderer 是默认渲染器：成功 data 直接封套，失败封套 msg=错误消息。
var JSONRenderer Renderer = RendererFunc(func(w http.ResponseWriter, status int, kind ErrKind, msg string, data any) {
	env := Envelope{Code: CodeOK, Msg: msg, Data: data}
	if kind != "" {
		env.Code = string(kind)
		// 失败不回带半成品数据。
		env.Data = nil
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(env); err != nil {
		// 响应已开始写出，无法再改状态码：只能让日志看见（AD-14 失效可观测）。
		_ = err
	}
})

// Config 是 Router 的注入面。零值可用（全部字段有安全默认）。
type Config struct {
	Status StatusMap     // nil ⇒ DefaultStatus()
	Render Renderer      // nil ⇒ JSONRenderer
	Log    kitctx.Logger // nil ⇒ 丢弃（与 kit.Config 同一口径）
	// MaxBodyBytes 单个请求体上限；≤0 ⇒ 不限（显式选择，不替调用方补默认值）。
	MaxBodyBytes int64
	// Recovered 在 panic 兜底后被调用（用于告警/上报）；nil ⇒ 只写响应与日志。
	Recovered func(r *http.Request, kind ErrKind, recovered any)
	// Identity 建立"这次请求是谁"（认证/取会话），把 RequestContext 放进 ctx 供下游取用。
	//
	// 它**不做授权**：能不能碰某个资源仍只有 `tools.allows` 一条谓词（框架裁决 规则 5）。
	// 返回 error ⇒ 本层直接以 unauthenticated(401) 结束；返回空 Role ⇒ 不报错也不补值，
	// 由下游 `kitctx.RoleFrom` 自己判定为"角色不可用"（框架不替宿主猜一个默认角色）。
	Identity func(r *http.Request) (kitctx.RequestContext, error)
	// Authorize 是"当前身份能否使用这条路由"的钩子。
	//
	// 本包**不实现**这个判定：框架里授权谓词只有一处（`tools.allows`，框架裁决 规则 5），
	// 在这里再写一遍就会有两份可能互相矛盾的答案。宿主通常把它接到同一个 authz 注册表上。
	//
	// nil ⇒ 声明了 `Route.Perm` 的路由在 HTTP 侧不被网关（Perm 退化为纯元数据）。
	// 这不是可以静默接受的状态，因此封存时会就这一问题打一条 Warn（见 sealWarnPerms）。
	Authorize func(ctx context.Context, route Route) error
}

// Handler 是业务处理函数：返回 error 即交给统一错误渲染（不必自己写响应）。
type Handler func(w http.ResponseWriter, r *http.Request) error

// Middleware 包装下一段链。形状与标准库一致（`func(next http.Handler) http.Handler`），
// 因此宿主既有中间件可以直接接进来。
type Middleware func(next http.Handler) http.Handler

// Registrar 是**声明期**能看到的全部路由面（`*Router` 天然满足它）。
//
// 刻意不含 `Handler`/`Use`：封存路由表是不可逆的全局动作，
// 若声明期插件能拿到它，"先注册者优先"的两阶段纪律就会被一个 `Handler` 调用打断。
// 于是框架给声明期的只有：登记、枚举、进程内直调。
type Registrar interface {
	Register(Route, Handler) error
	Routes() []Route
	Invoke(ctx context.Context, name string, args map[string]any) (InvokeResult, error)
}

var _ Registrar = (*Router)(nil)

// Route 是一条已登记路由的可枚举事实（P5 的 route↔tool 同源比对以此为准）。
type Route struct {
	Method  string
	Path    string
	Name    string // 可选的稳定标识（管理面与工具名映射用）
	Summary string
	// Perm 是本接口要求的权限点键。**只是引用**：存在性由 P5 与 authz 注册表比对，
	// 本包不校验也不判定权限（授权谓词只有一处，见 框架裁决 规则 5）。
	Perm string
	// Public 声明"此接口不要求身份"（健康检查、登录入口、第三方回调）。
	//
	// 默认 false：没有显式声明的接口一律要求身份 —— 因为身份是全局注入的，
	// 少了这个开关，健康检查也会被 401，运维探活直接失效（P4 样例实测抓到）。
	// 注意：未匹配到任何路由的请求仍会先做身份建立，于是"路径是否存在"
	// 不会泄露给未认证调用方（这是取舍，不是漏）。
	Public bool
}

// 方法闭集（登记时归一为这些值；不在表内 ⇒ 拒绝）。
var methods = map[string]bool{
	http.MethodGet: true, http.MethodPost: true, http.MethodPut: true,
	http.MethodPatch: true, http.MethodDelete: true, http.MethodHead: true,
	http.MethodOptions: true,
}

// Methods 返回方法闭集成员（字典序）。
func Methods() []string {
	out := make([]string, 0, len(methods))
	for m := range methods {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

// 装配期错误。
var (
	// ErrSealed 表示路由表已封存（首次 Handler/Serve 之后不得再改）。
	ErrSealed = errors.New("web: 路由表已封存，不再接受注册")
	// ErrHandlerRequired 表示注册时没给处理函数。
	ErrHandlerRequired = errors.New("web: 缺少处理函数")
	// ErrMethodUnsupported 表示方法不在闭集内。
	ErrMethodUnsupported = errors.New("web: 方法不在闭集内")
	// ErrPathInvalid 表示路径不是以 / 开头的干净路径。
	ErrPathInvalid = errors.New("web: 路径非法（须以 / 开头且不含 . 或 // 段）")
	// ErrPathPatternUnsupported 表示路径里出现参数段/通配语法。
	ErrPathPatternUnsupported = errors.New("web: 本包只做静态路由，不支持 :参数 / {参数} / * 通配（需要参数段请用解析中间件放进 ctx）")
	// ErrDuplicateRoute 表示同一 method+path 重复登记。
	ErrDuplicateRoute = errors.New("web: 路由重复登记")
	// ErrMiddlewareNil 表示中间件列表里有 nil。
	ErrMiddlewareNil = errors.New("web: 中间件不得为 nil")
	// ErrAlreadyBuilt 是内部哨兵（不应外泄）。
	ErrAlreadyBuilt = errors.New("web: 链已构建")
	// ErrDuplicateRouteName 表示两条路由用了同一个 Name（直调会歧义）。
	ErrDuplicateRouteName = errors.New("web: 路由名称重复")
	// ErrRouterRequired 表示构造服务器时没给路由表。
	ErrRouterRequired = errors.New("web: 服务器必须注入 *web.Router")
)

// Router 是静态路由表 + 中间件链，实现 http.Handler。
type Router struct {
	cfg Config

	mu       sync.Mutex
	sealed   bool
	middle   []Middleware
	specs    []Route
	handlers map[string]Handler // "METHOD path" → handler
	byKey    map[string]Route   // "METHOD path" → 声明事实（Authorize 钩子要读 Perm）
	byName   map[string]string  // Route.Name → "METHOD path"（进程内直调用）
	public   map[string]bool    // "METHOD path" → 声明为无需身份
	byPath   map[string][]string
	order    int
	chain    http.Handler
	chainErr error
	once     sync.Once
}

// NewRouter 构造路由表。
func NewRouter(cfg Config) *Router {
	if cfg.Status == nil {
		cfg.Status = DefaultStatus()
	}
	if cfg.Render == nil {
		cfg.Render = JSONRenderer
	}
	if cfg.Log == nil {
		cfg.Log = kitctx.NopLogger()
	}
	return &Router{cfg: cfg, handlers: map[string]Handler{}, byName: map[string]string{}, byKey: map[string]Route{},
		public: map[string]bool{}, byPath: map[string][]string{}}
}

// Register 登记一条路由（校验失败 ⇒ 不留半成品）。
func (rt *Router) Register(r Route, h Handler) error {
	if h == nil {
		return ErrHandlerRequired
	}
	method, err := normalizeMethod(r.Method)
	if err != nil {
		return err
	}
	path, err := normalizePath(r.Path)
	if err != nil {
		return err
	}
	r.Method, r.Path = method, path

	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.sealed {
		return ErrSealed
	}
	key := routeKey(method, path)
	if _, dup := rt.handlers[key]; dup {
		return fmt.Errorf("%w：%s %s", ErrDuplicateRoute, method, path)
	}
	if r.Name != "" {
		if dup, exists := rt.byName[r.Name]; exists {
			return fmt.Errorf("%w：%q 已被 %s 占用", ErrDuplicateRouteName, r.Name, dup)
		}
		rt.byName[r.Name] = key
	}
	rt.handlers[key] = h
	rt.byKey[key] = r
	if r.Public {
		rt.public[key] = true
	}
	rt.byPath[path] = append(rt.byPath[path], method)
	rt.specs = append(rt.specs, r)
	rt.order++
	return nil
}

// Handle 是 Register 的简写。
func (rt *Router) Handle(method, path string, h Handler) error {
	return rt.Register(Route{Method: method, Path: path}, h)
}

// Use 追加中间件：**注册顺序即外层到内层**（先注册的最先看到请求）。
// panic 兜底始终在最外层，且不可被摘除。
func (rt *Router) Use(mw ...Middleware) error {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.sealed {
		return ErrSealed
	}
	for i, m := range mw {
		if m == nil {
			return fmt.Errorf("%w：第 %d 个", ErrMiddlewareNil, i)
		}
		rt.middle = append(rt.middle, m)
	}
	return nil
}

// Routes 返回已登记路由（注册顺序的副本，字典序整理后交出）。
func (rt *Router) Routes() []Route {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	out := append([]Route(nil), rt.specs...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		return out[i].Method < out[j].Method
	})
	return out
}

// ServeHTTP 实现 http.Handler：先封存并建链（一次），再按 method+path 精确分发。
func (rt *Router) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h, err := rt.build()
	if err != nil {
		rt.cfg.Render.Render(w, rt.cfg.Status(ErrKindInternal), ErrKindInternal, err.Error(), nil)
		return
	}
	h.ServeHTTP(w, r)
}

// Handler 返回封存后的处理链（可用于 http.Server 或 httptest）。
// 首次调用即封存路由表 —— 之后再注册会得到 ErrSealed。
func (rt *Router) Handler() (http.Handler, error) { return rt.build() }

func (rt *Router) build() (http.Handler, error) {
	rt.once.Do(func() {
		rt.mu.Lock()
		rt.sealed = true
		mws := append([]Middleware(nil), rt.middle...)
		leaf := http.HandlerFunc(rt.dispatch)
		rt.mu.Unlock()

		// 逆序包裹：最后注册的最靠内 ⇒ 注册顺序 = 外层到内层（确定性快照判据依赖这条）。
		var next http.Handler = leaf
		for i := len(mws) - 1; i >= 0; i-- {
			next = mws[i](next)
		}
		rt.chain = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rt.recoverAndServe(next, w, r)
		})
		rt.warnUngatedPerms()
	})
	if rt.chain == nil {
		return nil, ErrAlreadyBuilt
	}
	return rt.chain, rt.chainErr
}

// warnUngatedPerms 在封存时检查一次：有路由声明了 Perm，但没有任何地方判定它。
// 静默容忍这个状态，就等于让"看起来受保护"变成"其实公开"。
func (rt *Router) warnUngatedPerms() {
	rt.mu.Lock()
	n := 0
	for _, r := range rt.specs {
		if r.Perm != "" && !r.Public {
			n++
		}
	}
	hasAuthorize := rt.cfg.Authorize != nil
	rt.mu.Unlock()
	if n > 0 && !hasAuthorize {
		rt.cfg.Log.Warn("web: 路由声明了 Perm 但未注入 Authorize ⇒ HTTP 侧不会拦截，Perm 只是元数据",
			map[string]any{"routes_with_perm": n})
	}
}

func (rt *Router) recoverAndServe(next http.Handler, w http.ResponseWriter, r *http.Request) {
	defer func() {
		rec := recover()
		if rec == nil {
			return
		}
		rt.cfg.Log.Error("web: 处理链 panic 已兜底", map[string]any{
			"method": r.Method, "path": r.URL.Path, "panic": fmt.Sprint(rec),
		})
		if rt.cfg.Recovered != nil {
			rt.cfg.Recovered(r, ErrKindInternal, rec)
		}
		rt.cfg.Render.Render(w, rt.cfg.Status(ErrKindInternal), ErrKindInternal, "服务内部错误", nil)
	}()
	if rt.cfg.MaxBodyBytes > 0 && r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, rt.cfg.MaxBodyBytes)
	}
	// 身份建立：公开接口与进程内直调（`Invoke`）跳过 —— 后者没有"请求头里的会话"可解析，
	// 身份必须由调用方放进 ctx，框架不在这里造身份（授权谓词仍只有一处）。
	if rt.cfg.Identity != nil && !rt.isPublic(r.Method, r.URL.Path) && !isInvoked(r.Context()) {
		rc, err := rt.cfg.Identity(r)
		if err != nil {
			kind, okk := KindOf(err)
			if !okk {
				kind = ErrKindUnauthenticated
			}
			rt.cfg.Log.Warn("web: 身份建立失败", map[string]any{"path": r.URL.Path, "kind": string(kind), "err": err.Error()})
			rt.cfg.Render.Render(w, rt.cfg.Status(kind), kind, messageFor(err), nil)
			return
		}
		r = r.WithContext(kitctx.WithRequest(r.Context(), rc))
	}
	next.ServeHTTP(w, r)
}

// dispatch 做精确匹配与错误渲染（404 / 405 / 处理函数返回的 error）。
func (rt *Router) dispatch(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	methods4Path := rt.methodsFor(path)
	if len(methods4Path) == 0 {
		rt.writeErr(w, r, Errorf(ErrKindNotFound, "no route for %s %s", r.Method, path), ErrKindNotFound)
		return
	}
	h, ok := rt.handlers[routeKey(r.Method, path)]
	if !ok {
		w.Header().Set("Allow", strings.Join(methods4Path, ", "))
		rt.writeErr(w, r, Errorf(ErrKindMethodNotAllowed, "%s %s allows %v", r.Method, path, methods4Path), ErrKindMethodNotAllowed)
		return
	}
	if rt.cfg.Authorize != nil {
		if err := rt.cfg.Authorize(r.Context(), rt.routeFor(r.Method, path)); err != nil {
			kind, okk := KindOf(err)
			if !okk {
				kind = ErrKindForbidden
			}
			rt.writeErr(w, r, err, kind)
			return
		}
	}
	if err := h(w, r); err != nil {
		kind, okk := KindOf(err)
		if !okk {
			kind = ErrKindInternal
		}
		rt.writeErr(w, r, err, kind)
	}
}

// messageFor 取给用户看的消息：带类别的错误只取其 Msg（类别已由 kind 参数单独交出，
// 不该在消息里再印一遍前缀 —— 那会让宿主无法自行决定"要不要显示类别"）。
func messageFor(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Msg
	}
	return err.Error()
}

func (rt *Router) writeErr(w http.ResponseWriter, r *http.Request, err error, kind ErrKind) {
	if kind != ErrKindNotFound && kind != ErrKindMethodNotAllowed {
		rt.cfg.Log.Warn("web: 请求以错误结束", map[string]any{
			"method": r.Method, "path": r.URL.Path, "kind": string(kind), "err": err.Error(),
		})
	}
	rt.cfg.Render.Render(w, rt.cfg.Status(kind), kind, messageFor(err), nil)
}

// routeFor 取声明事实（未登记 ⇒ 零值 Route）。
func (rt *Router) routeFor(method, path string) Route {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.byKey[routeKey(method, path)]
}

// isPublic 查询某 method+path 是否显式声明为无需身份。
func (rt *Router) isPublic(method, path string) bool {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.public[routeKey(method, path)]
}

func (rt *Router) methodsFor(path string) []string {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	ms := rt.byPath[path]
	if len(ms) == 0 {
		return nil
	}
	out := append([]string(nil), ms...)
	sort.Strings(out)
	return out
}

// ---- 编解码助手 ----

// DecodeJSON 读取并解析请求体。超限归为 payload_too_large，解析失败归为 invalid_input
// —— 都不外泄底层错误细节给客户端（渲染的是 msg，日志里才有完整 err）。
func DecodeJSON(r *http.Request, dst any) error {
	if r.Body == nil {
		return Errorf(ErrKindInvalidInput, "请求体为空")
	}
	if ct := r.Header.Get("Content-Type"); ct != "" && !strings.HasPrefix(ct, "application/json") {
		return Errorf(ErrKindInvalidInput, "Content-Type 需为 application/json，实得 %q", ct)
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return Errorf(ErrKindTooLarge, "请求体超过上限")
		}
		return NewError(ErrKindInvalidInput, "读取请求体失败", err)
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return Errorf(ErrKindInvalidInput, "请求体为空")
	}
	if err := json.Unmarshal(body, dst); err != nil {
		return NewError(ErrKindInvalidInput, "请求体不是合法 JSON", err)
	}
	return nil
}

// WriteJSON 写出成功响应（data 可以是任意可序列化值）。
func (rt *Router) WriteJSON(w http.ResponseWriter, data any) {
	rt.cfg.Render.Render(w, http.StatusOK, "", "", data)
}

// WriteJSON 用默认渲染器写出成功响应（给不想持有 Router 的处理函数用）。
func WriteJSON(w http.ResponseWriter, data any) {
	JSONRenderer.Render(w, http.StatusOK, "", "", data)
}

// ---- 归一化 ----

func normalizeMethod(m string) (string, error) {
	up := strings.ToUpper(strings.TrimSpace(m))
	if !methods[up] {
		return "", fmt.Errorf("%w：%q（允许 %v）", ErrMethodUnsupported, m, Methods())
	}
	return up, nil
}

func normalizePath(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", ErrPathInvalid
	}
	if p[0] != '/' {
		return "", fmt.Errorf("%w：%q 未以 / 开头", ErrPathInvalid, p)
	}
	if strings.ContainsAny(p, ":*{}?") {
		return "", fmt.Errorf("%w：%q", ErrPathPatternUnsupported, p)
	}
	if strings.Contains(p, "//") {
		return "", fmt.Errorf("%w：%q 含空段", ErrPathInvalid, p)
	}
	for _, seg := range strings.Split(strings.TrimPrefix(p, "/"), "/") {
		if seg == "." || seg == ".." {
			return "", fmt.Errorf("%w：%q 含 . / .. 段", ErrPathInvalid, p)
		}
	}
	if len(p) > 1 && p[len(p)-1] == '/' {
		return "", fmt.Errorf("%w：%q 带尾斜杠（宿主若需尾斜杠请在注册前归一）", ErrPathInvalid, p)
	}
	return p, nil
}

func routeKey(method, path string) string { return method + " " + path }

// ---- Server：入域生命周期（`http.Server` 只能出现在本包，框架裁决 D2）----

// ServerConfig 是服务构造参数。
type ServerConfig struct {
	Router *Router
	// Addr 监听地址（":8080" / "127.0.0.1:0"）。由宿主从配置给出，本包不读配置。
	Addr string
	// ReadHeaderTimeout ≤0 时补 10s：没有这个超时的服务器可被慢头连接拖死（不是偏好，是常识兜底）。
	ReadHeaderTimeout time.Duration
	// ShutdownTimeout ≤0 时补 5s。
	ShutdownTimeout time.Duration
}

// Server 把路由表挂到一个 HTTP 服务器上。
//
// 与 `preset.Server` 的分工：本类型管"怎么服务"，插件管"什么时候起、什么时候必须停"。
type Server struct {
	srv    *http.Server
	shutAt time.Duration
}

// NewServer 构造服务器（Router 为 nil ⇒ 拒绝：没有路由表的服务器等于一个 404 生成器）。
func NewServer(cfg ServerConfig) (*Server, error) {
	if cfg.Router == nil {
		return nil, ErrRouterRequired
	}
	// 不在此处调用 Router.Handler：那会立刻封存路由表，把"先建服务器再补路由"
	// 这一常见次序卡死。封存留到首次使用（Handler/首个请求）发生。
	rho := cfg.ReadHeaderTimeout
	if rho <= 0 {
		rho = 10 * time.Second
	}
	shto := cfg.ShutdownTimeout
	if shto <= 0 {
		shto = 5 * time.Second
	}
	return &Server{
		srv:    &http.Server{Addr: cfg.Addr, Handler: cfg.Router, ReadHeaderTimeout: rho},
		shutAt: shto,
	}, nil
}

// Handler 返回已封存的请求链（宿主可以把它挂到自己的服务器上，或交给 httptest）。
func (s *Server) Handler() http.Handler { return s.srv.Handler }

// Addr 返回监听地址。
func (s *Server) Addr() string { return s.srv.Addr }

// ListenAndServe 开始服务并阻塞；返回 `http.ErrServerClosed` 表示是被 Shutdown 停的（正常收尾）。
func (s *Server) ListenAndServe() error { return s.srv.ListenAndServe() }

// Serve 用调用方持有的监听器开始服务（`:0` 随机端口、unix socket、宿主自己包 TLS 等场景）。
// 监听器由调用方关闭；本方法的返回语义与 ListenAndServe 一致（ErrServerClosed 表示正常收尾）。
func (s *Server) Serve(ln net.Listener) error { return s.srv.Serve(ln) }

// Shutdown 优雅停止：等待在途请求，最多 ShutdownTimeout。
func (s *Server) Shutdown(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, s.shutAt)
	defer cancel()
	return s.srv.Shutdown(ctx)
}
