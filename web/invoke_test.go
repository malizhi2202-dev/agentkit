package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"agentkit/kitctx"
)

func TestWeb_InvokeCarriesArgsByMethod(t *testing.T) {
	rt := newTestRouter(t, Config{})
	if err := rt.Register(Route{Method: "GET", Path: "/search", Name: "search"},
		func(w http.ResponseWriter, r *http.Request) error {
			q := r.URL.Query()
			if got := q["tag"]; len(got) != 2 || got[0] != "a" || got[1] != "b" {
				return Errorf(ErrKindInvalidInput, "重复键未正确展开：%v", got)
			}
			WriteJSON(w, map[string]any{"ip": q.Get("ip"), "n": q.Get("n")})
			return nil
		}); err != nil {
		t.Fatalf("Register：%v", err)
	}
	if err := rt.Register(Route{Method: "POST", Path: "/echo", Name: "echo"},
		func(w http.ResponseWriter, r *http.Request) error {
			var in struct {
				IP      string `json:"ip"`
				Verbose bool   `json:"verbose"`
			}
			if err := DecodeJSON(r, &in); err != nil {
				return err
			}
			WriteJSON(w, in)
			return nil
		}); err != nil {
		t.Fatalf("Register：%v", err)
	}

	res, err := rt.Invoke(context.Background(), "search", map[string]any{"ip": "1.2.3.4", "n": float64(7), "tag": []any{"a", "b"}})
	if err != nil {
		t.Fatalf("Invoke search：%v（%s）", err, res)
	}
	if !res.OK() || res.ContentType == "" {
		t.Fatalf("直调应 200 且带 Content-Type：%s", res)
	}
	var out struct {
		IP string `json:"ip"`
		N  string `json:"n"`
	}
	if err := res.Decode(&out); err != nil {
		t.Fatalf("Decode：%v", err)
	}
	if out.IP != "1.2.3.4" || out.N != "7" {
		t.Fatalf("查询参数丢失或数字被写成 7.0：%+v", out)
	}

	res2, err := rt.Invoke(context.Background(), "echo", map[string]any{"ip": "5.6.7.8", "verbose": true})
	if err != nil {
		t.Fatalf("Invoke echo：%v", err)
	}
	var echoed struct {
		IP      string `json:"ip"`
		Verbose bool   `json:"verbose"`
	}
	if err := res2.Decode(&echoed); err != nil {
		t.Fatalf("Decode：%v", err)
	}
	if echoed.IP != "5.6.7.8" || !echoed.Verbose {
		t.Fatalf("POST 请求体未正确编码：%+v", echoed)
	}
	// 空参数表编成 {} 而不是 "null"（"null" 会被解成合法 JSON 的 null，让结构体全零却无报错）。
	// 注意：这里**不校验业务必填字段** —— `Invoke` 的语义就是"等价于一次 HTTP 请求"，
	// 请求里少字段是处理函数的事；模型侧的必填约束在工具边界由同一份 schema 强制（preset.Served）。
	res3, err := rt.Invoke(context.Background(), "echo", nil)
	if err != nil {
		t.Fatalf("空参数直调应可完成：%v（%s）", err, res3)
	}
	var zero struct {
		IP      string `json:"ip"`
		Verbose bool   `json:"verbose"`
	}
	if err := res3.Decode(&zero); err != nil {
		t.Fatalf("Decode：%v（%s）", err, res3)
	}
	if zero.IP != "" || zero.Verbose {
		t.Fatalf("空参数应解出零值：%+v", zero)
	}
	if !bytes.Contains(res3.Body, []byte(`"data":{`)) && !bytes.Contains(res3.Body, []byte(`"data": {`)) {
		t.Fatalf("空 body 未走 {} 路径：%s", res3.Body)
	}
}

func TestWeb_InvokeErrorsAndFailures(t *testing.T) {
	rt := newTestRouter(t, Config{})
	if err := rt.Register(Route{Method: "GET", Path: "/denied", Name: "denied"},
		func(http.ResponseWriter, *http.Request) error {
			return Errorf(ErrKindForbidden, "角色无权")
		}); err != nil {
		t.Fatalf("Register：%v", err)
	}
	res, err := rt.Invoke(context.Background(), "denied", nil)
	if !errors.Is(err, ErrKindForbidden) || res.Status != http.StatusForbidden || !strings.Contains(err.Error(), "角色无权") {
		t.Fatalf("非 2xx 应同时交出结果与带类别的错误：%d %v %s", res.Status, err, res)
	}
	if _, err := rt.Invoke(context.Background(), "nope", nil); !errors.Is(err, ErrRouteUnknown) {
		t.Fatalf("未知名称应判 ErrRouteUnknown，实得 %v", err)
	}
	if _, err := rt.Invoke(context.Background(), "  ", nil); !errors.Is(err, ErrRouteNameRequired) {
		t.Fatalf("空名称应判 ErrRouteNameRequired，实得 %v", err)
	}
	// GET 的查询串不接受复杂值（不静默丢弃）
	rt2 := newTestRouter(t, Config{})
	if err := rt2.Register(Route{Method: "GET", Path: "/x", Name: "x"}, okHandler("y")); err != nil {
		t.Fatalf("Register：%v", err)
	}
	if _, err := rt2.Invoke(context.Background(), "x", map[string]any{"nested": map[string]any{"a": 1}}); !errors.Is(err, ErrArgUnsupported) {
		t.Fatalf("复杂参数应被拒，实得 %v", err)
	}
	// 同名路由不得共存（直调会歧义）
	if err := rt2.Register(Route{Method: "POST", Path: "/x", Name: "x"}, okHandler("z")); !errors.Is(err, ErrDuplicateRouteName) {
		t.Fatalf("重名路由应被拒，实得 %v", err)
	}
}

// TestWeb_InvokeSkipsIdentityButRunsTheChain 是本片的核心判据：
// 直调不经过身份建立（那是网络边界的事），但中间件、请求体上限、panic 兜底一个都不能少。
func TestWeb_InvokeSkipsIdentityButRunsTheChain(t *testing.T) {
	var mu sync.Mutex
	var trace []string
	var identityCalls int
	rt := newTestRouter(t, Config{
		MaxBodyBytes: 64,
		Identity: func(*http.Request) (kitctx.RequestContext, error) {
			identityCalls++
			return kitctx.RequestContext{}, errors.New("不该被调用")
		},
	})
	if err := rt.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			trace = append(trace, "mw")
			mu.Unlock()
			next.ServeHTTP(w, r)
		})
	}); err != nil {
		t.Fatalf("Use：%v", err)
	}
	if err := rt.Register(Route{Method: "POST", Path: "/ask", Name: "ask"},
		func(w http.ResponseWriter, r *http.Request) error {
			role, ok := kitctx.RoleFrom(r.Context())
			if !ok {
				return Errorf(ErrKindUnauthenticated, "ctx 里没有角色")
			}
			var in struct {
				Q string `json:"q"`
			}
			if err := DecodeJSON(r, &in); err != nil {
				return err
			}
			mu.Lock()
			trace = append(trace, "handler")
			mu.Unlock()
			WriteJSON(w, string(role)+":"+in.Q)
			return nil
		}); err != nil {
		t.Fatalf("Register：%v", err)
	}
	ctx := kitctx.WithRequest(context.Background(), kitctx.RequestContext{UserID: "u", Role: "support", TraceID: "tr"})
	res, err := rt.Invoke(ctx, "ask", map[string]any{"q": "hi"})
	if err != nil {
		t.Fatalf("带 ctx 身份的直调应成功：%v（%s）", err, res)
	}
	var data string
	if err := json.Unmarshal(mustData(t, res.Body), &data); err != nil {
		t.Fatalf("解码 data：%v", err)
	}
	if data != "support:hi" {
		t.Fatalf("ctx 身份未透传到处理函数：%q", data)
	}
	if identityCalls != 0 {
		t.Fatalf("直调不该走网络身份建立（被调 %d 次）", identityCalls)
	}
	if len(trace) != 2 || trace[0] != "mw" || trace[1] != "handler" {
		t.Fatalf("中间件链在直调时被跳过：%v", trace)
	}
	// 没有 ctx 身份 ⇒ 处理函数自己判不可用（框架不造身份）
	if _, err := rt.Invoke(context.Background(), "ask", map[string]any{"q": "hi"}); !errors.Is(err, ErrKindUnauthenticated) {
		t.Fatalf("无 ctx 身份应判 unauthenticated，实得 %v", err)
	}
	// 请求体上限照旧生效
	if _, err := rt.Invoke(ctx, "ask", map[string]any{"q": strings.Repeat("x", 300)}); !errors.Is(err, ErrKindTooLarge) {
		t.Fatalf("直调也应受请求体上限约束，实得 %v", err)
	}

	// panic 兜底照旧生效
	rt3 := newTestRouter(t, Config{})
	var hookErr error
	rt3.cfg.Recovered = func(*http.Request, ErrKind, any) { hookErr = errors.New("recovered") }
	if err := rt3.Register(Route{Method: "GET", Path: "/boom", Name: "boom"},
		func(http.ResponseWriter, *http.Request) error { panic("x") }); err != nil {
		t.Fatalf("Register：%v", err)
	}
	if _, err := rt3.Invoke(context.Background(), "boom", nil); !errors.Is(err, ErrKindInternal) {
		t.Fatalf("直调里的 panic 应兜底为 internal，实得 %v", err)
	}
	if hookErr == nil {
		t.Fatal("Recovered 钩子未被直调路径触发")
	}
}

func TestWeb_InvokeHonoursLimitsAndTimeout(t *testing.T) {
	rt := newTestRouter(t, Config{})
	if err := rt.Register(Route{Method: "GET", Path: "/slow", Name: "slow"},
		func(w http.ResponseWriter, r *http.Request) error {
			select {
			case <-r.Context().Done():
				return Errorf(ErrKindUnavailable, "上游超时")
			case <-time.After(2 * time.Second):
				WriteJSON(w, "too late")
				return nil
			}
		}); err != nil {
		t.Fatalf("Register：%v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := rt.Invoke(ctx, "slow", nil)
	if !errors.Is(err, ErrKindUnavailable) {
		t.Fatalf("ctx 取消应传进处理函数，实得 %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("未尊重调用方 ctx 的截止")
	}
}

func mustData(t *testing.T, body []byte) []byte {
	t.Helper()
	var env Envelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("封套解码：%v", err)
	}
	raw, err := json.Marshal(env.Data)
	if err != nil {
		t.Fatalf("data 重编码：%v", err)
	}
	return raw
}

func TestWeb_InvokeResultSummaryIsBounded(t *testing.T) {
	// 摘要要进日志与轨迹：不得把整个响应体灌进去，但必须留下"被截断过"的痕迹。
	res := InvokeResult{Status: 200, Kind: ErrKindForbidden, Body: []byte(strings.Repeat("y", 500))}
	s := res.String()
	if len(s) > 260 {
		t.Fatalf("摘要未截断（%d 字符）：%q", len(s), s)
	}
	for _, want := range []string{"status=200", "kind=forbidden", "…"} {
		if !strings.Contains(s, want) {
			t.Fatalf("摘要缺少 %q：%q", want, s)
		}
	}
}
