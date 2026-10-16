package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// 进程内直调（route↔tool 同源的执行侧）。
//
// 为什么必须有它：模型选中的"工具"如果通过 HTTP 回环调用自己，就要在框架里造一个
// 出域客户端（违反 ADR-012 的方向基线：出域只在 `model`），还要处理端口、超时、TLS。
// 正确做法是**同一条处理链、不经过网络**：中间件、请求体上限、panic 兜底、错误渲染全部照旧生效，
// 少掉的只有 TCP。于是"接口能被 agent 调用"不是把接口重实现一遍。
//
// 身份口径（重要）：`Invoke` **不调用** `Config.Identity`。
// 身份必须由调用方放进 ctx（通常是 `kitctx.WithRequest`），因为进程内调用没有"请求头里的会话"可解析；
// 框架也不在这里造身份 —— 授权判定仍只有 `tools.allows` 一条（ADR-011 规则 5）。

// InvokeResult 是一次进程内调用的结果。
//
// 字段刻意不含任何 `net/http` 类型：消费方（`preset` / 宿主工具）拿到的是纯值，
// 否则它们会因引用入域符号而被方向守卫判红。
type InvokeResult struct {
	Status      int
	Kind        ErrKind // 非 2xx 时的稳定类别；2xx 为空
	Msg         string
	Body        []byte
	ContentType string
}

// OK 报告是否 2xx。
func (r InvokeResult) OK() bool { return r.Status >= 200 && r.Status < 300 }

// Decode 按默认封套取出 data 并解进 dst（非默认封套时用 Body 自行处理）。
func (r InvokeResult) Decode(dst any) error {
	var env Envelope
	if err := json.Unmarshal(r.Body, &env); err != nil {
		return fmt.Errorf("web: 响应不是默认封套：%w", err)
	}
	raw, err := json.Marshal(env.Data)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, dst)
}

// String 给日志/轨迹摘要用（截断，避免把整个响应体灌进日志）。
func (r InvokeResult) String() string {
	b := r.Body
	if len(b) > 200 {
		b = append(append([]byte(nil), b[:200]...), "…"...)
	}
	return fmt.Sprintf("status=%d kind=%s body=%s", r.Status, string(r.Kind), b)
}

// 直调相关失败。
var (
	// ErrRouteUnknown 表示按名字找不到可调用路由（`Route.Name` 必须在注册时给出）。
	ErrRouteUnknown = errors.New("web: 找不到该名称的路由（注册时必须给出 Name）")
	// ErrRouteNameRequired 表示想直调却没登记名字。
	ErrRouteNameRequired = errors.New("web: 直调要求路由有稳定名称")
	// ErrArgUnsupported 表示参数里有 schema 之外的复杂形状。
	ErrArgUnsupported = errors.New("web: 直调参数只支持标量与标量数组")
)

// invokedKey 标记"这是进程内调用"，用于跳过身份建立。
type invokedKey struct{}

// WithInvoked 给 ctx 打上进程内调用标记（`Invoke` 内部使用，宿主一般不需要）。
func WithInvoked(ctx context.Context) context.Context {
	return context.WithValue(ctx, invokedKey{}, true)
}

func isInvoked(ctx context.Context) bool {
	v, _ := ctx.Value(invokedKey{}).(bool)
	return v
}

// Invoke 按 `Route.Name` 进程内调用一条路由。
//
// args 会按方法形状转换：GET/HEAD/DELETE/OPTIONS ⇒ 查询参数（数组 ⇒ 重复键），
// POST/PUT/PATCH ⇒ JSON 请求体。非 2xx 时同时返回结果与错误（错误里带稳定类别），
// 让上层既能记账也能据类别记缺口。
func (rt *Router) Invoke(ctx context.Context, name string, args map[string]any) (InvokeResult, error) {
	if strings.TrimSpace(name) == "" {
		return InvokeResult{}, ErrRouteNameRequired
	}
	rt.mu.Lock()
	key, ok := rt.byName[name]
	rt.mu.Unlock()
	if !ok {
		return InvokeResult{}, fmt.Errorf("%w：%q", ErrRouteUnknown, name)
	}
	method, path := splitKey(key)
	r, err := buildRequest(ctx, method, path, args)
	if err != nil {
		return InvokeResult{}, err
	}
	h, err := rt.build()
	if err != nil {
		return InvokeResult{}, err
	}
	rec := &recorder{hdr: http.Header{}}
	h.ServeHTTP(rec, r)
	res := InvokeResult{
		Status:      rec.StatusCode(),
		Body:        append([]byte(nil), rec.body.Bytes()...),
		ContentType: rec.hdr.Get("Content-Type"),
	}
	var env Envelope
	if json.Unmarshal(res.Body, &env) == nil && env.Code != "" && env.Code != CodeOK {
		res.Kind = ErrKind(env.Code)
		res.Msg = env.Msg
	}
	if !res.OK() {
		if res.Kind == "" {
			res.Kind = ErrKindInternal
		}
		if res.Msg == "" {
			res.Msg = http.StatusText(res.Status)
		}
		return res, NewError(res.Kind, res.Msg, nil)
	}
	return res, nil
}

// buildRequest 手工构造请求：`http.NewRequestWithContext` 属**出域**构造（它服务于客户端），
// 在本包出现会被方向守卫判红，进程内调用也不需要它。
func buildRequest(ctx context.Context, method, path string, args map[string]any) (*http.Request, error) {
	u := &url.URL{Path: path}
	var body io.Reader
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch:
		raw, err := json.Marshal(args)
		if err != nil {
			return nil, fmt.Errorf("web: 直调参数无法编码为 JSON：%w", err)
		}
		// 不特判 "null"：`json.Unmarshal([]byte("null"), &struct)` 与 "{}" 效果相同（全零值），
		// 为它写分支等于留一条测不出差别的路径。
		body = bytes.NewReader(raw)
	default:
		q, err := queryOf(args)
		if err != nil {
			return nil, err
		}
		u.RawQuery = q
	}
	r := &http.Request{
		Method: method,
		URL:    u,
		Header: http.Header{},
	}
	if body != nil {
		buf := bytes.NewBuffer(nil)
		if _, err := buf.ReadFrom(body); err != nil {
			return nil, err
		}
		raw := buf.Bytes()
		r.Body = io.NopCloser(bytes.NewReader(raw))
		r.ContentLength = int64(len(raw))
		r.Header.Set("Content-Type", "application/json")
	}
	return r.WithContext(WithInvoked(ctx)), nil
}

// queryOf 把参数表编成查询串（键名字典序，保证同样的参数生成同样的 URL）。
func queryOf(args map[string]any) (string, error) {
	if len(args) == 0 {
		return "", nil
	}
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	vals := url.Values{}
	for _, k := range keys {
		switch v := args[k].(type) {
		case string:
			vals.Add(k, v)
		case bool:
			vals.Add(k, strconv.FormatBool(v))
		case float64:
			vals.Add(k, formatNum(v))
		case int:
			vals.Add(k, strconv.Itoa(v))
		case json.Number:
			vals.Add(k, v.String())
		case []any:
			for _, item := range v {
				s, err := scalarString(item)
				if err != nil {
					return "", fmt.Errorf("%w：字段 %s 的数组元素：%w", ErrArgUnsupported, k, err)
				}
				vals.Add(k, s)
			}
		case nil:
			continue
		default:
			return "", fmt.Errorf("%w：字段 %s 是 %T", ErrArgUnsupported, k, v)
		}
	}
	return vals.Encode(), nil
}

func scalarString(v any) (string, error) {
	switch t := v.(type) {
	case string:
		return t, nil
	case bool:
		return strconv.FormatBool(t), nil
	case float64:
		return formatNum(t), nil
	case int:
		return strconv.Itoa(t), nil
	case json.Number:
		return t.String(), nil
	}
	return "", fmt.Errorf("不支持的元素类型 %T", v)
}

func formatNum(f float64) string {
	if f == float64(int64(f)) {
		return strconv.FormatInt(int64(f), 10)
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

// recorder 是最小的 http.ResponseWriter 实现（进程内调用不落网络，也不需要 Flush/Hijack）。
type recorder struct {
	code        int
	hdr         http.Header
	body        bytes.Buffer
	wroteHeader bool
}

func (r *recorder) Header() http.Header {
	if r.hdr == nil {
		r.hdr = http.Header{}
	}
	return r.hdr
}

func (r *recorder) Write(p []byte) (int, error) {
	if !r.wroteHeader {
		r.code = http.StatusOK
		r.wroteHeader = true
	}
	return r.body.Write(p)
}

func (r *recorder) WriteHeader(code int) {
	if r.wroteHeader {
		return
	}
	r.code = code
	r.wroteHeader = true
}

// StatusCode 报告实际状态码（从未写过响应 ⇒ 200 空响应）。
func (r *recorder) StatusCode() int {
	if r.code == 0 {
		return http.StatusOK
	}
	return r.code
}

func splitKey(key string) (method, path string) {
	i := strings.IndexByte(key, ' ')
	if i < 0 {
		return key, "/"
	}
	return key[:i], key[i+1:]
}
