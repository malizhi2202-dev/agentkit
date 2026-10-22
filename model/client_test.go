package model

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// doerCall 记录一次可见的尝试事实：方法与 URL（证明端点未被改写）、Content-Type、
// 实际 payload（证明重试 payload 逐字节一致，供  的 HMAC 交叉核验）、
// 以及本次尝试的 ctx deadline（证明注入的**单次尝试超时**真的进了请求 ctx）。
type doerCall struct {
	method      string
	url         string
	contentType string
	body        []byte
	hasDeadline bool
	deadline    time.Time
}

// egressStep 是一次脚本化尝试的结果。
type egressStep struct {
	resp *http.Response
	err  error
	// hang 为真时阻塞到本次尝试的 ctx 结束 —— 用注入的传输确定性复现「模型超时」，不打真实网络。
	hang bool
}

// scriptedDoer 是 TASK  要求的可注入 fake HTTPDoer：按脚本依次复现
// 「不可用 / 超时 / 正常」三态；脚本耗尽后重复最后一步（便于表达「一直不可用」）。
type scriptedDoer struct {
	mu    sync.Mutex
	steps []egressStep
	calls []doerCall
}

func (d *scriptedDoer) Do(req *http.Request) (*http.Response, error) {
	call := doerCall{method: req.Method, url: req.URL.String(), contentType: req.Header.Get("Content-Type")}
	if deadline, ok := req.Context().Deadline(); ok {
		call.hasDeadline, call.deadline = true, deadline
	}
	if req.Body != nil {
		raw, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		_ = req.Body.Close()
		call.body = raw
	}

	d.mu.Lock()
	d.calls = append(d.calls, call)
	idx := len(d.calls) - 1
	if idx >= len(d.steps) {
		idx = len(d.steps) - 1
	}
	if idx < 0 {
		d.mu.Unlock()
		return nil, errors.New("scriptedDoer: 脚本为空")
	}
	step := d.steps[idx]
	d.mu.Unlock()

	if step.hang {
		<-req.Context().Done()
		return nil, req.Context().Err()
	}
	return step.resp, step.err
}

func (d *scriptedDoer) snapshot() []doerCall {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]doerCall(nil), d.calls...)
}

// countingDoer 统计真实 http.Client 的调用次数（用于「策略拒绝不重试」的断言）。
type countingDoer struct {
	inner HTTPDoer
	calls atomic.Int64
}

func (d *countingDoer) Do(req *http.Request) (*http.Response, error) {
	d.calls.Add(1)
	return d.inner.Do(req)
}

// fastDoer 无共享可写状态，用于并发用例（-race 证明 Client 只读、可并发复用）。
type fastDoer struct {
	calls atomic.Int64
}

func (d *fastDoer) Do(*http.Request) (*http.Response, error) {
	d.calls.Add(1)
	return rawJSONResponse(http.StatusOK, `{"choices":[{"index":0,"message":{"role":"assistant","content":"ok"}}]}`), nil
}

func rawJSONResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func jsonResponse(t *testing.T, body string) *http.Response {
	t.Helper()
	return rawJSONResponse(http.StatusOK, body)
}

const okChatBody = `{"choices":[{"index":0,"message":{"role":"assistant","content":"宿主入口词完成"},"finish_reason":"stop"}]}`

func userRequest() ChatRequest {
	return ChatRequest{Messages: []ChatMessage{{Role: "user", Content: "查一下 1.2.3.4"}}}
}

// newTestClient 用零配置策略（域名一律拒绝、RFC1918/回环字面量放行）构造被测客户端。
func newTestClient(doer HTTPDoer, mutate func(*ClientConfig)) *Client {
	cfg := ClientConfig{Doer: doer, Policy: NewEndpointPolicy(EndpointPolicyConfig{}), MaxAttempts: 1}
	if mutate != nil {
		mutate(&cfg)
	}
	return NewClient(cfg)
}

// 正常态：一次调用完成 POST + JSON 编码 + 响应解析，且不改写调用方给定端点。
func TestClient_CompleteJSONRoundTrip(t *testing.T) {
	doer := &scriptedDoer{steps: []egressStep{{resp: jsonResponse(t, okChatBody)}}}
	client := newTestClient(doer, nil)

	out, err := client.Complete(context.Background(), "http://10.0.0.1:8081/v1/chat/completions", userRequest())
	if err != nil {
		t.Fatalf("正常态不应报错: %v", err)
	}
	if len(out.Choices) != 1 || out.Choices[0].Message.Content != "宿主入口词完成" {
		t.Fatalf("响应解析不符: %+v", out)
	}

	calls := doer.snapshot()
	if len(calls) != 1 {
		t.Fatalf("应恰好出域 1 次，实际 %d 次", len(calls))
	}
	if calls[0].method != http.MethodPost || calls[0].url != "http://10.0.0.1:8081/v1/chat/completions" {
		t.Fatalf("请求方法与端点不得被改写: %+v", calls[0])
	}
	if calls[0].contentType != "application/json" {
		t.Fatalf("Content-Type 应为 application/json，实际 %q", calls[0].contentType)
	}
	var sent ChatRequest
	if err := json.Unmarshal(calls[0].body, &sent); err != nil {
		t.Fatalf("出域 payload 不是 ChatRequest JSON: %v", err)
	}
	if len(sent.Messages) != 1 || sent.Messages[0].Content != "查一下 1.2.3.4" {
		t.Fatalf("出域 payload 与请求不符: %+v", sent)
	}
}

// 不可用态：传输错误必须**可识别**（供上层转 degraded=true + LOG:llm.error），并按重试预算用满预算。
func TestClient_UnavailableDegradesIdentifiably(t *testing.T) {
	doer := &scriptedDoer{steps: []egressStep{{err: errors.New("dial tcp 10.0.0.1:8081: connect: connection refused")}}}
	client := newTestClient(doer, func(c *ClientConfig) { c.MaxAttempts = 3 })

	_, err := client.Complete(context.Background(), "http://10.0.0.1:8081/v1/chat/completions", userRequest())
	if err == nil {
		t.Fatal("模型不可用必须返回可识别错误，而不是成功")
	}
	if !errors.Is(err, ErrModelUnavailable) {
		t.Fatalf("错误须可识别为 ErrModelUnavailable，实际: %v", err)
	}
	if !strings.HasPrefix(err.Error(), "model.unavailable") {
		t.Fatalf("错误文本须以稳定标识 model.unavailable 开头（供 LOG reason），实际 %q", err)
	}
	if got := len(doer.snapshot()); got != 3 {
		t.Fatalf("重试预算 3 应产生 3 次尝试，实际 %d 次", got)
	}
	if !strings.Contains(err.Error(), "共尝试 3 次") {
		t.Fatalf("耗尽预算的错误须写明尝试次数，实际 %q", err)
	}
}

// 超时态：注入的 Timeout 必须是**每次尝试独立**的预算（不是整体一次），超时错误可识别。
func TestClient_TimeoutIsIdentifiableAndPerAttempt(t *testing.T) {
	doer := &scriptedDoer{steps: []egressStep{{hang: true}}}
	client := newTestClient(doer, func(c *ClientConfig) {
		c.Timeout = 20 * time.Millisecond
		c.MaxAttempts = 2
	})

	_, err := client.Complete(context.Background(), "http://10.0.0.1:8081/v1/chat/completions", userRequest())
	if !errors.Is(err, ErrModelTimeout) {
		t.Fatalf("超时必须可识别为 ErrModelTimeout，实际: %v", err)
	}
	if !strings.HasPrefix(err.Error(), "model.timeout") {
		t.Fatalf("错误文本须以稳定标识 model.timeout 开头，实际 %q", err)
	}

	calls := doer.snapshot()
	if len(calls) != 2 {
		t.Fatalf("超时应消耗重试预算（2 次尝试），实际 %d 次", len(calls))
	}
	for i, call := range calls {
		if !call.hasDeadline {
			t.Fatalf("第 %d 次尝试未携带注入的超时预算", i+1)
		}
	}
	if !calls[1].deadline.After(calls[0].deadline) {
		t.Fatalf("两次尝试必须各自独立计时（第 2 次的 deadline 应晚于第 1 次）: %v / %v", calls[0].deadline, calls[1].deadline)
	}
}

// 调用方 ctx 已结束 ⇒ 不再出域、不再重试（fail-fast，且不把调用方取消误报成模型不可用）。
func TestClient_CanceledContextFailsFast(t *testing.T) {
	doer := &scriptedDoer{steps: []egressStep{{resp: jsonResponse(t, okChatBody)}}}
	client := newTestClient(doer, func(c *ClientConfig) { c.MaxAttempts = 3 })

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := client.Complete(ctx, "http://10.0.0.1:8081/v1/chat/completions", userRequest())
	if !errors.Is(err, ErrModelCanceled) {
		t.Fatalf("调用方取消须可识别为 ErrModelCanceled，实际: %v", err)
	}
	if got := len(doer.snapshot()); got != 0 {
		t.Fatalf("ctx 已取消时不得出域，实际出域 %d 次", got)
	}
}

// 重试间隔是注入预算：等待期间调用方取消 ⇒ 立即收口，不继续重试、不真实等待。
func TestClient_CancelDuringBackoffStopsRetrying(t *testing.T) {
	doer := &scriptedDoer{steps: []egressStep{{err: errors.New("connection reset by peer")}}}
	client := newTestClient(doer, func(c *ClientConfig) {
		c.MaxAttempts = 3
		c.RetryBackoff = 25 * time.Millisecond
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var slept []time.Duration
	client.sleep = func(ctx context.Context, d time.Duration) error {
		slept = append(slept, d)
		cancel()
		return ctx.Err()
	}

	_, err := client.Complete(ctx, "http://10.0.0.1:8081/v1/chat/completions", userRequest())
	if !errors.Is(err, ErrModelCanceled) {
		t.Fatalf("等待期取消须可识别为 ErrModelCanceled，实际: %v", err)
	}
	if got := len(doer.snapshot()); got != 1 {
		t.Fatalf("取消后不得再出域，实际尝试 %d 次", got)
	}
	if len(slept) != 1 || slept[0] != 25*time.Millisecond {
		t.Fatalf("重试间隔须等于注入的 RetryBackoff，实际 %v", slept)
	}
}

// 状态码分类：仅 429/5xx 可重试；4xx 与「不成功但可解析」的失败不重试。
func TestClient_StatusRetryClassification(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		attempts  int
		wantCalls int
	}{
		{"服务端 500 可重试", http.StatusInternalServerError, 3, 3},
		{"限流 429 可重试", http.StatusTooManyRequests, 2, 2},
		{"请求错误 400 不重试", http.StatusBadRequest, 3, 1},
		{"未认证 401 不重试", http.StatusUnauthorized, 3, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doer := &scriptedDoer{steps: []egressStep{{resp: rawJSONResponse(tc.status, `{"error":"boom"}`)}}}
			client := newTestClient(doer, func(c *ClientConfig) { c.MaxAttempts = tc.attempts })

			_, err := client.Complete(context.Background(), "http://10.0.0.1:8081/v1/chat/completions", userRequest())
			if !errors.Is(err, ErrModelStatus) {
				t.Fatalf("非成功状态须可识别为 ErrModelStatus，实际: %v", err)
			}
			if !strings.HasPrefix(err.Error(), "model.status") {
				t.Fatalf("错误文本须以稳定标识 model.status 开头，实际 %q", err)
			}
			if got := len(doer.snapshot()); got != tc.wantCalls {
				t.Fatalf("HTTP %d 的尝试次数应为 %d，实际 %d", tc.status, tc.wantCalls, got)
			}
		})
	}
}

// 审计不落原文（D7 / ）：模型错误体可能回显 prompt，错误文本里不得带响应体。
func TestClient_StatusErrorDoesNotLeakResponseBody(t *testing.T) {
	const secret = "SECRET-PROMPT-ECHO-IMEI-123456789012345"
	doer := &scriptedDoer{steps: []egressStep{{resp: rawJSONResponse(http.StatusBadGateway, `{"error":"`+secret+`"}`)}}}
	client := newTestClient(doer, nil)

	_, err := client.Complete(context.Background(), "http://10.0.0.1:8081/v1/chat/completions", userRequest())
	if err == nil {
		t.Fatal("502 必须失败")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("错误文本泄露了模型响应原文: %q", err)
	}
	if !strings.Contains(err.Error(), "502") {
		t.Fatalf("错误须保留可诊断的状态码，实际 %q", err)
	}
}

// 可解析但不可用的响应：非法 JSON / 无候选回复都必须可识别且不重试（否则会把空结论伪装成成功）。
func TestClient_MalformedResponseIsIdentifiableAndNotRetried(t *testing.T) {
	cases := []struct {
		name string
		body string
		want error
	}{
		{"非法 JSON", `{"choices":`, ErrModelMalformed},
		{"无候选回复", `{"choices":[]}`, ErrModelNoChoices},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doer := &scriptedDoer{steps: []egressStep{{resp: jsonResponse(t, tc.body)}}}
			client := newTestClient(doer, func(c *ClientConfig) { c.MaxAttempts = 3 })

			_, err := client.Complete(context.Background(), "http://10.0.0.1:8081/v1/chat/completions", userRequest())
			if !errors.Is(err, tc.want) {
				t.Fatalf("须可识别为 %v，实际: %v", tc.want, err)
			}
			if got := len(doer.snapshot()); got != 1 {
				t.Fatalf("不可重试的响应失败只应出域 1 次，实际 %d 次", got)
			}
		})
	}
}

// 端点白名单的运行期二次校验由  策略强制：不在清单内的公网端点**在出域前**即被拒（doer 零调用）。
func TestClient_EndpointPolicyRejectionSkipsNetwork(t *testing.T) {
	cases := []struct {
		name     string
		endpoint string
		want     error
	}{
		{"公网域名不在清单内", "https://api.openai.com/v1/chat/completions", ErrEndpointNotAllowlisted},
		{"公网 IP 字面量", "https://8.8.8.8/v1/chat/completions", ErrEndpointPublic},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doer := &scriptedDoer{steps: []egressStep{{resp: jsonResponse(t, okChatBody)}}}
			client := newTestClient(doer, func(c *ClientConfig) { c.MaxAttempts = 3 })

			_, err := client.Complete(context.Background(), tc.endpoint, userRequest())
			if !errors.Is(err, tc.want) {
				t.Fatalf("端点 %q 须被 任务 策略拒绝为 %v，实际: %v", tc.endpoint, tc.want, err)
			}
			if got := len(doer.snapshot()); got != 0 {
				t.Fatalf("策略拒绝不得出域（doer 应 0 调用），实际 %d 次", got)
			}
		})
	}
}

// 重定向（ 策略强制）：真实 http.Client 挂 CheckRedirect ⇒ 302 目标一次都不会被访问，
// 且该策略拒绝**不重试**（把 fail-closed 的拒绝伪装成「模型不可用」会误导降级判断）。
func TestClient_RedirectRejectedByPolicy(t *testing.T) {
	policy := NewEndpointPolicy(EndpointPolicyConfig{})

	var hits atomic.Int32
	dst := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer dst.Close()
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, dst.URL, http.StatusFound)
	}))
	defer src.Close()

	doer := &countingDoer{inner: &http.Client{CheckRedirect: policy.CheckRedirect}}
	client := NewClient(ClientConfig{Doer: doer, Policy: policy, MaxAttempts: 3})

	_, err := client.Complete(context.Background(), src.URL+"/v1/chat/completions", userRequest())
	if !errors.Is(err, ErrRedirectNotAllowed) {
		t.Fatalf("重定向须被拒为 ErrRedirectNotAllowed，实际: %v", err)
	}
	if hits.Load() != 0 {
		t.Fatalf("重定向目标被访问 %d 次，应为 0（禁跟随出网）", hits.Load())
	}
	if got := doer.calls.Load(); got != 1 {
		t.Fatalf("重定向拒绝属策略拒绝，不得重试，实际尝试 %d 次", got)
	}
}

// 兜底二次判定：注入的 doer 若**未接** CheckRedirect 而自行跟随了跳转，
// 响应上会留下「实际请求已偏离端点」的痕迹 ⇒ Client 必须识别并拒绝（防绕过  策略）。
func TestClient_FollowedRedirectByInjectedDoerIsCaught(t *testing.T) {
	followed := jsonResponse(t, okChatBody)
	followed.Request = mustRequest(t, "http://10.0.0.1:8081/redirected")
	doer := &scriptedDoer{steps: []egressStep{{resp: followed}}}
	client := newTestClient(doer, func(c *ClientConfig) { c.MaxAttempts = 3 })

	_, err := client.Complete(context.Background(), "http://10.0.0.1:8081/v1/chat/completions", userRequest())
	if !errors.Is(err, ErrRedirectNotAllowed) {
		t.Fatalf("被跟随的跳转须被拒为 ErrRedirectNotAllowed，实际: %v", err)
	}
	if got := len(doer.snapshot()); got != 1 {
		t.Fatalf("重定向拒绝不得重试，实际尝试 %d 次", got)
	}
}

// 注入链缺失即 fail-closed：没有 HTTPDoer 或没有端点策略时**不出域**，且不 panic。
func TestClient_NotConfiguredFailsClosed(t *testing.T) {
	policy := NewEndpointPolicy(EndpointPolicyConfig{})
	cases := []struct {
		name string
		cfg  ClientConfig
	}{
		{"两者皆缺", ClientConfig{}},
		{"缺策略", ClientConfig{Doer: &fastDoer{}}},
		{"缺 doer", ClientConfig{Policy: policy}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := NewClient(tc.cfg)
			_, err := client.Complete(context.Background(), "http://10.0.0.1:8081/v1/chat/completions", userRequest())
			if !errors.Is(err, ErrClientNotConfigured) {
				t.Fatalf("未配置须 fail-closed 为 ErrClientNotConfigured，实际: %v", err)
			}
			if !strings.HasPrefix(err.Error(), "client.not_configured") {
				t.Fatalf("错误文本须以稳定标识 client.not_configured 开头，实际 %q", err)
			}
		})
	}
}

// 重试 payload 逐字节一致：审计侧用注入 doer 捕获实际 payload 复算 sent_hmac 的前提
// （：证据必须与实际 payload 一致，重试不得改变出域内容）。
func TestClient_RetryPayloadIsByteIdentical(t *testing.T) {
	doer := &scriptedDoer{steps: []egressStep{
		{resp: rawJSONResponse(http.StatusInternalServerError, `{"error":"boom"}`)},
		{err: errors.New("connection reset by peer")},
		{resp: jsonResponse(t, okChatBody)},
	}}
	client := newTestClient(doer, func(c *ClientConfig) { c.MaxAttempts = 3 })

	if _, err := client.Complete(context.Background(), "http://10.0.0.1:8081/v1/chat/completions", userRequest()); err != nil {
		t.Fatalf("第 3 次尝试成功，整体应成功: %v", err)
	}
	calls := doer.snapshot()
	if len(calls) != 3 {
		t.Fatalf("应尝试 3 次，实际 %d 次", len(calls))
	}
	for i := 1; i < len(calls); i++ {
		if !bytes.Equal(calls[0].body, calls[i].body) {
			t.Fatalf("第 %d 次尝试的 payload 与首次不一致（审计无法复算 HMAC）", i+1)
		}
	}
}

// Client 只读、可被多 goroutine 并发复用（-race 下验证无共享可写状态）。
func TestClient_ConcurrentUseIsRaceFree(t *testing.T) {
	doer := &fastDoer{}
	client := newTestClient(doer, func(c *ClientConfig) { c.Timeout = time.Second })

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := client.Complete(context.Background(), "http://10.0.0.1:8081/v1/chat/completions", userRequest()); err != nil {
				t.Errorf("并发调用不应失败: %v", err)
			}
		}()
	}
	wg.Wait()
	if got := doer.calls.Load(); got != 8 {
		t.Fatalf("应出域 8 次，实际 %d 次", got)
	}
}

// 编码失败（不可 JSON 序列化的工具参数）⇒ 可识别且**不出域**：不能把调用方传参错误发成请求。
func TestClient_EncodeFailureIsIdentifiableAndNotRetried(t *testing.T) {
	doer := &scriptedDoer{steps: []egressStep{{resp: jsonResponse(t, okChatBody)}}}
	client := newTestClient(doer, func(c *ClientConfig) { c.MaxAttempts = 3 })

	req := ChatRequest{
		Messages: []ChatMessage{{Role: "user", Content: "查一下 1.2.3.4"}},
		Tools:    []ToolVisible{{Name: "user.profile", Params: make(chan int), Summary: "IP 档案", Role: "admin"}},
	}
	_, err := client.Complete(context.Background(), "http://10.0.0.1:8081/v1/chat/completions", req)
	if !errors.Is(err, ErrRequestEncode) {
		t.Fatalf("编码失败须可识别为 ErrRequestEncode，实际: %v", err)
	}
	if got := len(doer.snapshot()); got != 0 {
		t.Fatalf("编码失败不得出域，实际出域 %d 次", got)
	}
}

// errReader 的 body 在读取时立刻失败，用于复现「响应体读到一半中断」这一传输层失败。
type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("read: connection reset by peer") }

// errBodyResponse 返回一个「有 body 但读不动」的响应。
func errBodyResponse() *http.Response {
	resp := rawJSONResponse(http.StatusOK, "")
	resp.Body = io.NopCloser(errReader{})
	return resp
}

// 传输/响应缺陷的 fail-closed 收口：无响应、无 body、端点串非法都必须是可识别错误而不是 panic。
func TestClient_TransportDefectsAreIdentifiable(t *testing.T) {
	noBody := rawJSONResponse(http.StatusOK, "")
	_ = noBody.Body.Close()
	noBody.Body = nil

	cases := []struct {
		name     string
		endpoint string
		step     egressStep
		want     error
	}{
		{"doer 返回空响应", "http://10.0.0.1:8081/v1/chat/completions", egressStep{}, ErrModelUnavailable},
		{"200 但无 body", "http://10.0.0.1:8081/v1/chat/completions", egressStep{resp: noBody}, ErrModelMalformed},
		{"端点串非法", "http://10.0.0.1/%zz", egressStep{resp: jsonResponse(t, okChatBody)}, ErrRequestEncode},
		{"传输上报取消", "http://10.0.0.1:8081/v1/chat/completions", egressStep{err: context.Canceled}, ErrModelCanceled},
		{"响应体读取中断", "http://10.0.0.1:8081/v1/chat/completions", egressStep{resp: errBodyResponse()}, ErrModelUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doer := &scriptedDoer{steps: []egressStep{tc.step}}
			client := newTestClient(doer, nil)

			_, err := client.Complete(context.Background(), tc.endpoint, userRequest())
			if !errors.Is(err, tc.want) {
				t.Fatalf("须可识别为 %v，实际: %v", tc.want, err)
			}
		})
	}
}

// 调用方 ctx 的**整体预算**先于单次尝试超时耗尽 ⇒ 归为超时且立即收口（不再重试）。
func TestClient_ParentDeadlineDuringTransport(t *testing.T) {
	doer := &scriptedDoer{steps: []egressStep{{hang: true}}}
	client := newTestClient(doer, func(c *ClientConfig) { c.MaxAttempts = 3 })

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	_, err := client.Complete(ctx, "http://10.0.0.1:8081/v1/chat/completions", userRequest())
	if !errors.Is(err, ErrModelTimeout) {
		t.Fatalf("调用方整体超时须可识别为 ErrModelTimeout，实际: %v", err)
	}
	if got := len(doer.snapshot()); got != 1 {
		t.Fatalf("整体预算耗尽后不得重试，实际尝试 %d 次", got)
	}
}

// 未注入 sleep 时走真实定时器：重试间隔真的被等待（不空转重放）。
func TestClient_BackoffUsesRealTimer(t *testing.T) {
	doer := &scriptedDoer{steps: []egressStep{
		{resp: rawJSONResponse(http.StatusInternalServerError, `{"error":"boom"}`)},
		{resp: jsonResponse(t, okChatBody)},
	}}
	client := newTestClient(doer, func(c *ClientConfig) {
		c.MaxAttempts = 2
		c.RetryBackoff = 5 * time.Millisecond
	})

	start := time.Now()
	if _, err := client.Complete(context.Background(), "http://10.0.0.1:8081/v1/chat/completions", userRequest()); err != nil {
		t.Fatalf("第 2 次尝试成功，整体应成功: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 5*time.Millisecond {
		t.Fatalf("重试间隔未被真实等待，实际耗时 %v", elapsed)
	}
	if got := len(doer.snapshot()); got != 2 {
		t.Fatalf("应尝试 2 次，实际 %d 次", got)
	}
}

// 真实定时器等待期间被取消 ⇒ 立刻收口（不把剩余预算睡完）。
func TestClient_CancelDuringRealBackoff(t *testing.T) {
	doer := &scriptedDoer{steps: []egressStep{{err: errors.New("connection reset by peer")}}}
	client := newTestClient(doer, func(c *ClientConfig) {
		c.MaxAttempts = 3
		c.RetryBackoff = 5 * time.Second
	})

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()
	defer cancel()

	start := time.Now()
	_, err := client.Complete(ctx, "http://10.0.0.1:8081/v1/chat/completions", userRequest())
	if !errors.Is(err, ErrModelCanceled) {
		t.Fatalf("等待期取消须可识别为 ErrModelCanceled，实际: %v", err)
	}
	if elapsed := time.Since(start); elapsed >= time.Second {
		t.Fatalf("取消后不应把 5s 退避睡完，实际耗时 %v", elapsed)
	}
	if got := len(doer.snapshot()); got != 1 {
		t.Fatalf("取消后不得再出域，实际尝试 %d 次", got)
	}
}

// 响应体上限（1 MiB）：超限必须可识别地拒绝，**不得**静默截断成「看似成功的响应」；
// 上限内即使正好贴边也必须照常成功（防把宽裕上限做成误伤）。
func TestClient_OversizedResponseIsRejectedNotTruncated(t *testing.T) {
	const head = `{"choices":[{"index":0,"message":{"role":"assistant","content":"`
	const tail = `"}}]}`
	// 贴边构造：filler 长度 = 上限 − 前后缀 ⇒ 整体恰好等于上限。
	atLimit := head + strings.Repeat("a", maxResponseBytes-len(head)-len(tail)) + tail
	if len(atLimit) != maxResponseBytes {
		t.Fatalf("测试构造有误：贴边体 %d 字节，应为 %d", len(atLimit), maxResponseBytes)
	}
	overLimit := head + strings.Repeat("a", maxResponseBytes-len(head)-len(tail)+1) + tail

	t.Run("超出上限 1 字节即拒绝", func(t *testing.T) {
		doer := &scriptedDoer{steps: []egressStep{{resp: jsonResponse(t, overLimit)}}}
		client := newTestClient(doer, func(c *ClientConfig) { c.MaxAttempts = 3 })

		_, err := client.Complete(context.Background(), "http://10.0.0.1:8081/v1/chat/completions", userRequest())
		if !errors.Is(err, ErrModelTooLarge) {
			t.Fatalf("超限须可识别为 ErrModelTooLarge，实际: %v", err)
		}
		if !strings.HasPrefix(err.Error(), "model.too_large") {
			t.Fatalf("错误文本须以稳定标识 model.too_large 开头（供 LOG reason），实际 %q", err)
		}
		if !strings.Contains(err.Error(), strconv.Itoa(maxResponseBytes)) {
			t.Fatalf("错误文本须写明上限值，实际 %q", err)
		}
		if got := len(doer.snapshot()); got != 1 {
			t.Fatalf("超限响应不得重试（再发一次仍会超限），实际尝试 %d 次", got)
		}
	})

	t.Run("正好等于上限仍然成功", func(t *testing.T) {
		doer := &scriptedDoer{steps: []egressStep{{resp: jsonResponse(t, atLimit)}}}
		client := newTestClient(doer, nil)

		out, err := client.Complete(context.Background(), "http://10.0.0.1:8081/v1/chat/completions", userRequest())
		if err != nil {
			t.Fatalf("上限内的响应不应失败: %v", err)
		}
		if len(out.Choices) != 1 || len(out.Choices[0].Message.Content) != maxResponseBytes-len(head)-len(tail) {
			t.Fatalf("贴边响应解析不符: %+v", out)
		}
	})
}

//	的框架侧前提：出域客户端零 宿主依赖，只用 stdlib（D1/D4）。
//
// 只扫描本任务产出的 client.go，避免约束同包其它任务新增的文件。
func TestGuard_ClientStdlibOnlyImports(t *testing.T) {
	const file = "client.go"
	allowed := map[string]bool{
		"bytes":         true,
		"context":       true,
		"encoding/json": true,
		"errors":        true,
		"fmt":           true,
		"io":            true,
		"net":           true,
		"net/http":      true,
		"time":          true,
	}

	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, file, nil, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	for _, spec := range parsed.Imports {
		path := strings.Trim(spec.Path.Value, `"`)
		// 白名单是唯一判据：它只列 stdlib 与本包兄弟包 ⇒ 任何落在宿主 module 其它子树里的
		// import 都会被这一条拦下（不再单列"禁宿主 module 名"，那既冗余又把宿主标识写进测试文件）。
		if !allowed[path] {
			t.Fatalf("%s 引入了非 stdlib 白名单依赖 %q（契约点: 本包零依赖）", file, path)
		}
	}
}
