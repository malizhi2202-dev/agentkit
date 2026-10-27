package web

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

// 类别既是闭集成员也要能直接作为 errors.Is 目标：两种用法都得成立。
func TestErrKindAndErrorWrapping(t *testing.T) {
	if got := ErrKindNotFound.Error(); got != string(ErrKindNotFound) {
		t.Fatalf("ErrKind.Error()=%q", got)
	}

	var nilErr *Error
	if got := nilErr.Error(); got != "<nil>" {
		t.Fatalf("nil *Error.Error()=%q，期望 \"<nil>\"", got)
	}
	if nilErr.Unwrap() != nil {
		t.Fatal("nil *Error.Unwrap() 必须为 nil")
	}

	e := &Error{Kind: ErrKindInternal, Msg: "boom", Cause: io.EOF}
	if !errors.Is(e, io.EOF) {
		t.Fatal("Unwrap 应让 errors.Is 穿透到 Cause")
	}
	if !errors.Is(e, ErrKindInternal) {
		t.Fatal("*Error 应能被类别匹配")
	}
	if got := e.Error(); got == "" {
		t.Fatal("Error() 不得为空")
	}
}

// WriteJSON 有两个入口（方法版给持有 Router 的人，包级版给裸 handler），行为必须一致。
func TestWriteJSONBothEntrypoints(t *testing.T) {
	rt := newTestRouter(t, Config{})

	for name, fn := range map[string]func(*httptest.ResponseRecorder){
		"method": func(w *httptest.ResponseRecorder) { rt.WriteJSON(w, map[string]any{"ok": true}) },
		"package": func(w *httptest.ResponseRecorder) {
			WriteJSON(w, map[string]any{"ok": true})
		},
	} {
		rr := httptest.NewRecorder()
		fn(rr)
		if rr.Code != http.StatusOK {
			t.Fatalf("%s 版状态码=%d", name, rr.Code)
		}
		var body map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s 版响应不是 JSON：%v（body=%q）", name, err, rr.Body.String())
		}
		data, _ := body["data"].(map[string]any)
		if data["ok"] != true {
			t.Fatalf("%s 版响应信封不符（期望 data.ok=true）：%v", name, body)
		}
	}
}

// Routes/routeFor 是管理面与工具名映射的数据源：登记即可查，未登记返回零值。
func TestRoutesAndRouteFor(t *testing.T) {
	rt := newTestRouter(t, Config{})
	spec := Route{Method: "GET", Path: "/orders/detail", Name: "orders.get", Summary: "查订单"}
	if err := rt.Register(spec, func(http.ResponseWriter, *http.Request) error { return nil }); err != nil {
		t.Fatalf("Register：%v", err)
	}

	routes := rt.Routes()
	if len(routes) != 1 {
		t.Fatalf("Routes() 长度=%d，期望 1", len(routes))
	}
	if routes[0].Name != "orders.get" {
		t.Fatalf("Routes() 未保留 Name：%+v", routes[0])
	}

	got := rt.routeFor("GET", "/orders/detail")
	if got.Name != "orders.get" {
		t.Fatalf("routeFor 命中失败：%+v", got)
	}
	if missing := rt.routeFor("POST", "/nope"); missing.Name != "" || missing.Path != "" {
		t.Fatalf("routeFor 未登记项必须返回零值，实得 %+v", missing)
	}
}

// 未登记路径走 404 分支（含错误响应渲染），并确认 ServeHTTP 不会 panic。
func TestServeHTTPUnknownRoute(t *testing.T) {
	rt := newTestRouter(t, Config{})
	rr := httptest.NewRecorder()
	rt.ServeHTTP(rr, httptest.NewRequest("GET", "/definitely-not-registered", nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("未知路径状态码=%d，期望 404", rr.Code)
	}
}

// Serve 与 ListenAndServe 的正常收尾语义：Shutdown 后返回 http.ErrServerClosed。
func TestServerServeAndListenAndServe(t *testing.T) {
	rt := newTestRouter(t, Config{})
	if err := rt.Handle("GET", "/ping", func(w http.ResponseWriter, _ *http.Request) error {
		WriteJSON(w, "pong")
		return nil
	}); err != nil {
		t.Fatalf("Handle：%v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen：%v", err)
	}
	srv, err := NewServer(ServerConfig{Router: rt, Addr: ln.Addr().String()})
	if err != nil {
		t.Fatalf("NewServer：%v", err)
	}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()

	resp, err := http.Get("http://" + ln.Addr().String() + "/ping")
	if err != nil {
		t.Fatalf("GET /ping：%v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /ping 状态码=%d", resp.StatusCode)
	}
	if err := srv.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown：%v", err)
	}
	if err := <-served; !errors.Is(err, http.ErrServerClosed) {
		t.Fatalf("Serve 收尾应返回 ErrServerClosed，实得 %v", err)
	}

	// ListenAndServe：用随机端口起，立刻优雅停止（不等待真实流量）。
	srv2, err := NewServer(ServerConfig{Router: rt, Addr: "127.0.0.1:0"})
	if err != nil {
		t.Fatalf("NewServer(2)：%v", err)
	}
	done := make(chan error, 1)
	go func() { done <- srv2.ListenAndServe() }()
	if err := srv2.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown(2)：%v", err)
	}
	if err := <-done; !errors.Is(err, http.ErrServerClosed) {
		t.Fatalf("ListenAndServe 收尾应返回 ErrServerClosed，实得 %v", err)
	}
}
