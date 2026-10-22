package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/malizhi2202-dev/agentkit/kitctx"
)

func okHandler(body string) Handler {
	return func(w http.ResponseWriter, r *http.Request) error {
		WriteJSON(w, body)
		return nil
	}
}

// decode 解析默认封套（键集合本身就是判据，见 TestWeb_EnvelopeShapeIsHostCompatible）。
func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应不是合法 JSON（%v）：%q", err, rec.Body.String())
	}
	return out
}

func newTestRouter(t *testing.T, cfg Config) *Router {
	t.Helper()
	return NewRouter(cfg)
}

func TestWeb_RouterRejectsBadSpecs(t *testing.T) {
	rt := newTestRouter(t, Config{})
	cases := []struct {
		name    string
		route   Route
		handler Handler
		wantErr error
	}{
		{"缺处理函数", Route{Method: "GET", Path: "/a"}, nil, ErrHandlerRequired},
		{"未知方法", Route{Method: "FETCH", Path: "/a"}, okHandler("x"), ErrMethodUnsupported},
		{"方法带空格", Route{Method: "GE T", Path: "/a"}, okHandler("x"), ErrMethodUnsupported},
		{"路径无前置斜杠", Route{Method: "GET", Path: "a"}, okHandler("x"), ErrPathInvalid},
		{"路径为空", Route{Method: "GET", Path: "  "}, okHandler("x"), ErrPathInvalid},
		{"路径含空段", Route{Method: "GET", Path: "/a//b"}, okHandler("x"), ErrPathInvalid},
		{"路径含点段", Route{Method: "GET", Path: "/a/../b"}, okHandler("x"), ErrPathInvalid},
		{"路径带尾斜杠", Route{Method: "GET", Path: "/a/"}, okHandler("x"), ErrPathInvalid},
		{"参数段不支持", Route{Method: "GET", Path: "/a/:id"}, okHandler("x"), ErrPathPatternUnsupported},
		{"花括号段不支持", Route{Method: "GET", Path: "/a/{id}"}, okHandler("x"), ErrPathPatternUnsupported},
		{"通配不支持", Route{Method: "GET", Path: "/a/*"}, okHandler("x"), ErrPathPatternUnsupported},
	}
	for _, c := range cases {
		err := rt.Register(c.route, c.handler)
		if !errors.Is(err, c.wantErr) {
			t.Errorf("%s：应判 %v，实得 %v", c.name, c.wantErr, err)
		}
	}
	if n := len(rt.Routes()); n != 0 {
		t.Fatalf("非法注册却留下了 %d 条路由（半成品表）", n)
	}
	// 合法注册：方法归一化、路径原样。
	if err := rt.Register(Route{Method: " get ", Path: "/ping", Perm: "engine.ping"}, okHandler("pong")); err != nil {
		t.Fatalf("合法注册被拒：%v", err)
	}
	got := rt.Routes()
	if len(got) != 1 || got[0].Method != "GET" || got[0].Path != "/ping" || got[0].Perm != "engine.ping" {
		t.Fatalf("注册事实丢失：%+v", got)
	}
	if err := rt.Register(Route{Method: "GET", Path: "/ping"}, okHandler("dup")); !errors.Is(err, ErrDuplicateRoute) {
		t.Fatalf("重复路由应被拒，实得 %v", err)
	}
}

func TestWeb_StaticDispatchNotFoundAndMethodNotAllowed(t *testing.T) {
	rt := newTestRouter(t, Config{})
	if err := rt.Handle("GET", "/ping", okHandler("pong")); err != nil {
		t.Fatalf("注册：%v", err)
	}
	srv := httptest.NewServer(rt)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/ping") // 测试侧发起，属被测面对象
	if err != nil {
		t.Fatalf("请求失败：%v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("已注册路由应 200，实得 %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Fatalf("Content-Type=%q", got)
	}

	// 未注册路径 ⇒ 404 + not_found（判据：不能静默 200 空响应）
	rec := httptest.NewRecorder()
	rt.ServeHTTP(rec, httptest.NewRequest("GET", "/nope", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("未注册路由应 404，实得 %d", rec.Code)
	}
	if body := decode(t, rec); body["code"] != string(ErrKindNotFound) {
		t.Fatalf("404 应带稳定类别，实得 %v", body["code"])
	}

	// 路径存在但方法不对 ⇒ 405 + Allow 头
	rec = httptest.NewRecorder()
	rt.ServeHTTP(rec, httptest.NewRequest("DELETE", "/ping", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("方法不匹配应 405，实得 %d", rec.Code)
	}
	if allow := rec.Header().Get("Allow"); allow != "GET" {
		t.Fatalf("Allow 头应为 GET，实得 %q", allow)
	}
	// HEAD 不自动继承 GET（否则路由可访问集与工具集比对会出现隐式通道）
	rec = httptest.NewRecorder()
	rt.ServeHTTP(rec, httptest.NewRequest("HEAD", "/ping", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("HEAD 不应被 GET 自动接走，实得 %d", rec.Code)
	}
}

// TestWeb_MiddlewareOrderIsDeterministic 是"顺序确定性快照"判据：
// 注册顺序 = 外层到内层，且进入与退出严格镜像。
func TestWeb_MiddlewareOrderIsDeterministic(t *testing.T) {
	rt := newTestRouter(t, Config{})
	var mu sync.Mutex
	var trace []string
	mw := func(tag string) Middleware {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				trace = append(trace, "pre:"+tag)
				mu.Unlock()
				next.ServeHTTP(w, r)
				mu.Lock()
				trace = append(trace, "post:"+tag)
				mu.Unlock()
			})
		}
	}
	if err := rt.Use(mw("first"), mw("second")); err != nil {
		t.Fatalf("Use：%v", err)
	}
	if err := rt.Use(mw("third")); err != nil {
		t.Fatalf("Use：%v", err)
	}
	if err := rt.Handle("GET", "/x", func(w http.ResponseWriter, r *http.Request) error {
		mu.Lock()
		trace = append(trace, "handler")
		mu.Unlock()
		WriteJSON(w, "ok")
		return nil
	}); err != nil {
		t.Fatalf("Handle：%v", err)
	}
	rec := httptest.NewRecorder()
	rt.ServeHTTP(rec, httptest.NewRequest("GET", "/x", nil))
	want := "pre:first,pre:second,pre:third,handler,post:third,post:second,post:first"
	if got := strings.Join(trace, ","); got != want {
		t.Fatalf("中间件顺序漂移\n  期望 %s\n  实得 %s", want, got)
	}
	fresh := newTestRouter(t, Config{})
	if err := fresh.Use(nil); !errors.Is(err, ErrMiddlewareNil) {
		t.Fatalf("nil 中间件应被拒，实得 %v", err)
	}
	if len(fresh.Routes()) != 0 {
		t.Fatal("非法中间件不该留下任何状态")
	}
}

func TestWeb_SealedAfterFirstUse(t *testing.T) {
	rt := newTestRouter(t, Config{})
	if err := rt.Handle("GET", "/a", okHandler("a")); err != nil {
		t.Fatalf("Handle：%v", err)
	}
	h, err := rt.Handler()
	if err != nil || h == nil {
		t.Fatalf("Handler：%v", err)
	}
	if err := rt.Handle("GET", "/b", okHandler("b")); !errors.Is(err, ErrSealed) {
		t.Fatalf("封存后注册应判 ErrSealed，实得 %v", err)
	}
	if err := rt.Use(func(n http.Handler) http.Handler { return n }); !errors.Is(err, ErrSealed) {
		t.Fatalf("封存后加中间件应判 ErrSealed，实得 %v", err)
	}
	rec := httptest.NewRecorder()
	rt.ServeHTTP(rec, httptest.NewRequest("GET", "/b", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("迟注册的路由不得生效（否则会跑到一半请求里），实得 %d", rec.Code)
	}
}

func TestWeb_PanicRecovered(t *testing.T) {
	var hookCalls int
	var logged []string
	rt := newTestRouter(t, Config{
		Recovered: func(r *http.Request, kind ErrKind, rec any) {
			hookCalls++
			if kind != ErrKindInternal {
				t.Errorf("panic 兜底类别应为 internal，实得 %q", kind)
			}
		},
		Log: captureLogger{on: func(msg string, _ map[string]any) { logged = append(logged, msg) }},
	})
	if err := rt.Handle("POST", "/boom", func(http.ResponseWriter, *http.Request) error {
		panic("kaboom")
	}); err != nil {
		t.Fatalf("Handle：%v", err)
	}
	rec := httptest.NewRecorder()
	rt.ServeHTTP(rec, httptest.NewRequest("POST", "/boom", strings.NewReader("{}")))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("panic 应兜底为 500，实得 %d", rec.Code)
	}
	if hookCalls != 1 {
		t.Fatalf("Recovered 钩子被调 %d 次", hookCalls)
	}
	if len(logged) == 0 {
		t.Fatal("panic 兜底未落日志 ⇒ 失效不可观测（AD-14）")
	}
	if body := decode(t, rec); body["code"] != string(ErrKindInternal) {
		t.Fatalf("panic 响应类别应为 internal，实得 %v", body["code"])
	}
}

func TestWeb_BodyLimitAndDecode(t *testing.T) {
	echo := func(w http.ResponseWriter, r *http.Request) error {
		var in struct {
			Name string `json:"name"`
		}
		if err := DecodeJSON(r, &in); err != nil {
			return err
		}
		WriteJSON(w, in.Name)
		return nil
	}
	rt := newTestRouter(t, Config{MaxBodyBytes: 16})
	if err := rt.Handle("POST", "/echo", echo); err != nil {
		t.Fatalf("Handle：%v", err)
	}
	post := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/echo", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rt.ServeHTTP(rec, req)
		return rec
	}
	if rec := post(`{"name":"ok"}`); rec.Code != http.StatusOK {
		t.Fatalf("合法请求应 200，实得 %d（%s）", rec.Code, rec.Body.String())
	}
	for _, c := range []struct {
		name, body string
		want       int
		wantCode   ErrKind
	}{
		{"空体", "", http.StatusBadRequest, ErrKindInvalidInput},
		{"空白体", "   ", http.StatusBadRequest, ErrKindInvalidInput},
		{"非法 JSON", "{", http.StatusBadRequest, ErrKindInvalidInput},
		{"超限", `{"name":"` + strings.Repeat("x", 200) + `"}`, http.StatusRequestEntityTooLarge, ErrKindTooLarge},
	} {
		rec := post(c.body)
		if rec.Code != c.want {
			t.Fatalf("%s：应 %d，实得 %d", c.name, c.want, rec.Code)
		}
		if code := decode(t, rec)["code"]; code != string(c.wantCode) {
			t.Fatalf("%s：类别应为 %s，实得 %v", c.name, c.wantCode, code)
		}
	}
	// Content-Type 不是 json ⇒ 拒绝（不"顺手宽松宽松"）
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/echo", strings.NewReader(`{"name":"a"}`))
	req.Header.Set("Content-Type", "text/plain")
	rt.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("错误 Content-Type 应被拒，实得 %d", rec.Code)
	}
	// 超限信息不外泄细节：msg 里不该出现字节数之外的内部结构（这里断言有稳定类别即可）
	if !strings.Contains(decode(t, post(`{"name":"`+strings.Repeat("y", 300)+`"}`))["msg"].(string), "请求体") {
		t.Fatal("超限消息应可诊断")
	}
}

func TestWeb_ErrorKindMappingAndOverride(t *testing.T) {
	rt := newTestRouter(t, Config{})
	if err := rt.Register(Route{Method: "GET", Path: "/denied"}, func(http.ResponseWriter, *http.Request) error {
		return Errorf(ErrKindForbidden, "无权")
	}); err != nil {
		t.Fatalf("Register：%v", err)
	}
	if err := rt.Register(Route{Method: "GET", Path: "/wrapped"}, func(http.ResponseWriter, *http.Request) error {
		return fmt.Errorf("外层上下文：%w", Errorf(ErrKindUnauthenticated, "未认证"))
	}); err != nil {
		t.Fatalf("Register：%v", err)
	}
	if err := rt.Register(Route{Method: "GET", Path: "/plain"}, func(http.ResponseWriter, *http.Request) error {
		return errors.New("实现细节：sql 报错")
	}); err != nil {
		t.Fatalf("Register：%v", err)
	}
	for path, want := range map[string]int{
		"/denied":  http.StatusForbidden,
		"/wrapped": http.StatusUnauthorized,
		"/plain":   http.StatusInternalServerError,
	} {
		rec := httptest.NewRecorder()
		rt.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != want {
			t.Fatalf("%s：应 %d，实得 %d", path, want, rec.Code)
		}
	}
	// 未包装错误不得把内部细节吐给客户端？—— 本包按宿主封套原样写 msg，
	// 但类别必须是 internal 且状态码 500（上面已断言）。这里补：msg 可诊断。
	rec := httptest.NewRecorder()
	rt.ServeHTTP(rec, httptest.NewRequest("GET", "/plain", nil))
	if !strings.Contains(decode(t, rec)["msg"].(string), "sql") {
		t.Fatal("错误消息应可诊断（由宿主渲染器决定是否脱敏）")
	}

	// 状态码可整体覆盖（框架裁决：真实码值属宿主）
	custom := newTestRouter(t, Config{Status: func(kind ErrKind) int {
		if kind == ErrKindForbidden {
			return http.StatusOK // 历史口径：越权也回 200，靠 code 字段区分
		}
		return DefaultStatus()(kind)
	}})
	if err := custom.Handle("GET", "/denied", func(http.ResponseWriter, *http.Request) error {
		return Errorf(ErrKindForbidden, "无权")
	}); err != nil {
		t.Fatalf("Handle：%v", err)
	}
	rec = httptest.NewRecorder()
	custom.ServeHTTP(rec, httptest.NewRequest("GET", "/denied", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("注入的状态表应生效，实得 %d", rec.Code)
	}
	if decode(t, rec)["code"] != string(ErrKindForbidden) {
		t.Fatal("类别标识必须与状态码解耦")
	}
}

func TestWeb_IdentityEstablishesContextButNotAuthorization(t *testing.T) {
	var sawRole string
	var sawOK bool
	handler := func(w http.ResponseWriter, r *http.Request) error {
		role, ok := kitctx.RoleFrom(r.Context())
		sawRole, sawOK = string(role), ok
		if !ok {
			return Errorf(ErrKindUnauthenticated, "角色不可判定")
		}
		WriteJSON(w, role)
		return nil
	}
	// 未注入 Identity ⇒ 不会凭空有一个角色
	rt := newTestRouter(t, Config{})
	if err := rt.Handle("GET", "/who", handler); err != nil {
		t.Fatalf("Handle：%v", err)
	}
	rec := httptest.NewRecorder()
	rt.ServeHTTP(rec, httptest.NewRequest("GET", "/who", nil))
	if sawOK || rec.Code != http.StatusUnauthorized {
		t.Fatalf("无身份注入时应判不可判定，sawOK=%v code=%d", sawOK, rec.Code)
	}

	// 注入后下游可见
	rt2 := newTestRouter(t, Config{Identity: func(r *http.Request) (kitctx.RequestContext, error) {
		return kitctx.RequestContext{UserID: "u1", Role: kitctx.Role(r.Header.Get("X-Role")), TraceID: "t1"}, nil
	}})
	if err := rt2.Handle("GET", "/who", handler); err != nil {
		t.Fatalf("Handle：%v", err)
	}
	rec = httptest.NewRecorder()
	rt2.ServeHTTP(rec, httptest.NewRequest("GET", "/who", nil))
	if sawOK {
		t.Fatalf("空 Role 必须被下游判为不可用（框架不补默认角色）：role=%q", sawRole)
	}
	if sawRole != "" {
		t.Fatalf("空 Role 不该被补成任何默认值：role=%q", sawRole)
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("空 Role 应由下游判定为不可用，实得 %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/who", nil)
	req.Header.Set("X-Role", "support")
	rt2.ServeHTTP(rec, req)
	if !sawOK || sawRole != "support" {
		t.Fatalf("注入身份未传到下游：sawOK=%v role=%q", sawOK, sawRole)
	}

	// Identity 自身失败 ⇒ 401，且**不进入**任何后续处理
	rt3 := newTestRouter(t, Config{Identity: func(*http.Request) (kitctx.RequestContext, error) {
		return kitctx.RequestContext{}, errors.New("会话过期")
	}})
	entered := false
	if err := rt3.Handle("GET", "/who", func(http.ResponseWriter, *http.Request) error {
		entered = true
		return nil
	}); err != nil {
		t.Fatalf("Handle：%v", err)
	}
	rec = httptest.NewRecorder()
	rt3.ServeHTTP(rec, httptest.NewRequest("GET", "/who", nil))
	if entered || rec.Code != http.StatusUnauthorized {
		t.Fatalf("身份建立失败应直接结束：entered=%v code=%d", entered, rec.Code)
	}
}

func TestWeb_ErrKindClosedSetAndUnknown(t *testing.T) {
	if len(Kinds()) < 9 {
		t.Fatalf("类别闭集疑似空转：%v", Kinds())
	}
	for _, k := range Kinds() {
		if !ErrKind(k).Valid() {
			t.Fatalf("Kinds() 里的 %q 自检不通过", k)
		}
		if DefaultStatus()(ErrKind(k)) < 400 && ErrKind(k) != "" {
			t.Fatalf("错误类别 %q 的默认状态码应为失败码", k)
		}
	}
	// 未知类别 ⇒ 归 internal，且原因留在 Cause 里可诊断（不静默）
	e := NewError(ErrKind("bogus"), "msg", errors.New("底层原因"))
	if e.Kind != ErrKindInternal {
		t.Fatalf("未知类别应归 internal，实得 %q", e.Kind)
	}
	if e.Cause == nil || !strings.Contains(e.Cause.Error(), "bogus") {
		t.Fatalf("未知类别应留下原因：%v", e.Cause)
	}
	if got := DefaultStatus()(ErrKind("bogus")); got != http.StatusInternalServerError {
		t.Fatalf("未收录类别应 500，实得 %d", got)
	}
	if _, ok := KindOf(errors.New("plain")); ok {
		t.Fatal("普通错误不该被说成有类别")
	}
}

func TestWeb_EnvelopeShapeIsHostCompatible(t *testing.T) {
	rt := newTestRouter(t, Config{})
	if err := rt.Handle("GET", "/ok", okHandler("hi")); err != nil {
		t.Fatalf("Handle：%v", err)
	}
	rec := httptest.NewRecorder()
	rt.ServeHTTP(rec, httptest.NewRequest("GET", "/ok", nil))
	body := decode(t, rec)
	keys := make([]string, 0, len(body))
	for k := range body {
		keys = append(keys, k)
	}
	if len(keys) != 3 {
		t.Fatalf("封套键应恰为 code/msg/data，实得 %v", keys)
	}
	for _, want := range []string{"code", "msg", "data"} {
		if _, ok := body[want]; !ok {
			t.Fatalf("缺少封套键 %q：%v", want, body)
		}
	}
	if body["code"] != CodeOK || body["data"] != "hi" {
		t.Fatalf("成功封套应为 code=\"0\" 且带 data，实得 %v", body)
	}
	// 自定义渲染器可整体替换（宿主保自己的码段）
	alt := newTestRouter(t, Config{Render: RendererFunc(func(w http.ResponseWriter, status int, kind ErrKind, msg string, data any) {
		w.Header().Set("X-Kind", string(kind))
		w.WriteHeader(status)
		_, _ = w.Write([]byte(msg))
	})})
	if err := alt.Handle("GET", "/nope", func(http.ResponseWriter, *http.Request) error {
		return Errorf(ErrKindUnavailable, "依赖不可用")
	}); err != nil {
		t.Fatalf("Handle：%v", err)
	}
	rec = httptest.NewRecorder()
	alt.ServeHTTP(rec, httptest.NewRequest("GET", "/nope", nil))
	if rec.Code != http.StatusServiceUnavailable || rec.Body.String() != "依赖不可用" || rec.Header().Get("X-Kind") != string(ErrKindUnavailable) {
		t.Fatalf("自定义渲染器未生效：%d %q", rec.Code, rec.Body.String())
	}
}

// captureLogger 只收集消息，用于断言"失效必须可观测"。
type captureLogger struct{ on func(string, map[string]any) }

func (l captureLogger) Debug(msg string, f map[string]any) {}
func (l captureLogger) Info(msg string, f map[string]any)  {}
func (l captureLogger) Warn(msg string, f map[string]any)  { l.on(msg, f) }
func (l captureLogger) Error(msg string, f map[string]any) { l.on(msg, f) }

func TestWeb_ServerConstructionAndDefaults(t *testing.T) {
	if _, err := NewServer(ServerConfig{}); !errors.Is(err, ErrRouterRequired) {
		t.Fatalf("无路由表的服务器应被拒，实得 %v", err)
	}
	rt := newTestRouter(t, Config{})
	srv, err := NewServer(ServerConfig{Router: rt, Addr: "127.0.0.1:0"})
	if err != nil {
		t.Fatalf("NewServer：%v", err)
	}
	if srv.Addr() != "127.0.0.1:0" {
		t.Fatalf("Addr 未透传：%q", srv.Addr())
	}
	// 零值超时补常识兜底（没有 ReadHeaderTimeout 的服务器可被慢头连接拖死）
	if got := srv.srv.ReadHeaderTimeout; got <= 0 {
		t.Fatalf("ReadHeaderTimeout 兜底未生效：%v", got)
	}
	if srv.shutAt <= 0 {
		t.Fatalf("ShutdownTimeout 兜底未生效：%v", srv.shutAt)
	}
	// 构造不得封存路由表：先建服务器再补路由是常见次序
	if err := rt.Handle("GET", "/late", okHandler("ok")); err != nil {
		t.Fatalf("NewServer 之后仍应可注册：%v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/late") // 测试侧发起
	if err != nil {
		t.Fatalf("请求：%v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("迟注册（但在使用前）的路由应可访问，实得 %d", resp.StatusCode)
	}
	// 真实监听：证明"起了就得能停"，且停之后再请求必然失败（不留野服务）
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen：%v", err)
	}
	srv2, err := NewServer(ServerConfig{Router: rt, Addr: ln.Addr().String()})
	if err != nil {
		t.Fatalf("NewServer(srv2)：%v", err)
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv2.Serve(ln) }()
	url := "http://" + ln.Addr().String() + "/late"
	resp2, err := http.Get(url) // 测试侧发起
	if err != nil {
		t.Fatalf("真实监听请求失败：%v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("真实监听应 200，实得 %d", resp2.StatusCode)
	}
	if err := srv2.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown：%v", err)
	}
	if err := <-serveErr; !errors.Is(err, http.ErrServerClosed) {
		t.Fatalf("Serve 应以 ErrServerClosed 收尾（正常停止），实得 %v", err)
	}
	if _, err := http.Get(url); err == nil { //nolint:bodyclose // 断言连接被拒
		t.Fatal("Shutdown 后端口仍在接受请求")
	}
	if err := srv.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown（未启动实例）：%v", err)
	}
	// 首个请求即封存：之后注册必须被拒
	if err := rt.Handle("GET", "/too-late", okHandler("x")); !errors.Is(err, ErrSealed) {
		t.Fatalf("服务已开始，注册应判 ErrSealed，实得 %v", err)
	}
}

func TestWeb_PublicRoutesSkipIdentityOnlyWhenDeclared(t *testing.T) {
	denyAll := func(*http.Request) (kitctx.RequestContext, error) {
		return kitctx.RequestContext{}, errors.New("无会话")
	}
	probe := func(t *testing.T, public bool, wantCode int) {
		t.Helper()
		rt := newTestRouter(t, Config{Identity: denyAll})
		if err := rt.Register(Route{Method: "GET", Path: "/p", Public: public}, okHandler("pong")); err != nil {
			t.Fatalf("Register：%v", err)
		}
		rec := httptest.NewRecorder()
		rt.ServeHTTP(rec, httptest.NewRequest("GET", "/p", nil))
		if rec.Code != wantCode {
			t.Fatalf("Public=%v 时期望 %d，实得 %d（%s）", public, wantCode, rec.Code, rec.Body.String())
		}
	}
	probe(t, false, http.StatusUnauthorized)
	probe(t, true, http.StatusOK)

	// 公开声明只对**登记过的那条** method+path 生效：同路径另一方法仍要身份
	rt := newTestRouter(t, Config{Identity: denyAll})
	if err := rt.Register(Route{Method: "GET", Path: "/p", Public: true}, okHandler("pong")); err != nil {
		t.Fatalf("Register：%v", err)
	}
	rec := httptest.NewRecorder()
	rt.ServeHTTP(rec, httptest.NewRequest("POST", "/p", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("公开声明不得顺带放行同路径的其它方法，实得 %d", rec.Code)
	}
	// 未注册路径：身份先行 ⇒ 不给未认证方透露"这条路径存不存在"
	rec = httptest.NewRecorder()
	rt.ServeHTTP(rec, httptest.NewRequest("GET", "/nope", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("未认证方不应从 404/401 的差异里学到路由存在性，实得 %d", rec.Code)
	}
}
