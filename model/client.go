package model

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// 出域客户端的可识别错误。
//
// 每个错误的文本都以**稳定标识**（`model.unavailable` 等）开头，使调用方既能用 errors.Is 做程序判定
// （不可用/超时 ⇒ 转 `degraded=true`），也能把 err 直接作为 `LOG:llm.error` 的 reason 落日志。
// 端点/重定向类拒绝**复用  的哨兵**（ErrEndpointPublic / ErrRedirectNotAllowed …），不另立一份口径。
var (
	// ErrClientNotConfigured 注入链缺失（HTTPDoer 或端点策略未注入）⇒ 按 fail-closed 拒绝出域。
	ErrClientNotConfigured = errors.New("client.not_configured: 出域客户端未注入 HTTPDoer 或端点策略，按 fail-closed 拒绝")
	// ErrRequestEncode 出域请求无法编码（通常是调用方传了不可 JSON 序列化的工具参数）⇒ 不重试。
	ErrRequestEncode = errors.New("request.encode: 出域请求无法编码")
	// ErrModelUnavailable 模型服务不可达（传输层失败，非策略拒绝）。
	ErrModelUnavailable = errors.New("model.unavailable: 模型服务不可达")
	// ErrModelTimeout 单次尝试超出注入的超时预算。
	ErrModelTimeout = errors.New("model.timeout: 模型服务超时")
	// ErrModelCanceled 调用方 ctx 被取消/超时 ⇒ 立即收口，不算模型故障。
	ErrModelCanceled = errors.New("model.canceled: 模型调用被调用方取消")
	// ErrModelStatus 模型服务返回非成功 HTTP 状态。
	ErrModelStatus = errors.New("model.status: 模型服务返回非成功状态")
	// ErrModelMalformed 响应体不是合法 ChatResponse。
	ErrModelMalformed = errors.New("model.malformed: 模型响应不是合法 ChatResponse")
	// ErrModelTooLarge 响应体超出框架硬上限（防无界读取造成内存耗尽）。
	ErrModelTooLarge = errors.New("model.too_large: 模型响应体超出框架上限，拒绝解析")
	// ErrModelNoChoices 响应可解析但没有任何候选回复（不能把空结论伪装成成功）。
	ErrModelNoChoices = errors.New("model.no_choices: 模型响应不含任何候选回复")
)

// maxResponseBytes 是单次模型响应的**硬上限**（1 MiB）。
//
// 理由：从外部服务无界读取响应体是真实的**内存耗尽面** —— 对端异常或被投毒时可无限吐字节，
// 而框架侧读多少就占多少。宽裕值的选取依据：模型响应是文本（聊天补全 + 工具调用参数 JSON），
// KB 级；7 段报告的完整 JSON 亦远小于 100 KB ⇒ 1 MiB 留了约一个数量级的余量，不会误伤正常响应。
//
// 超限**不静默截断**：截断只会得到「看似成功的半截 JSON」或「看似成功的空结论」，
// 因此一律返回可识别的 ErrModelTooLarge（fail-closed，且不重试 —— 同一请求再发一次仍会超限）。
const maxResponseBytes = 1 << 20

// ClientConfig 是**调用方注入**的出域传输与预算（框架内不读任何 宿主侧 配置， / D1）。
//
// 三处预算的 ≤0 语义刻意**不补默认值**（补默认值属配置层职责，同 kitctx 的 Limits 口径）：
// Timeout ≤0 表示除调用方 ctx 外不设单次尝试超时；MaxAttempts ≤0 表示不重试（即只尝试 1 次）；
// RetryBackoff ≤0 表示两次尝试之间不额外等待。
type ClientConfig struct {
	// Doer 是传输注入点（stdlib *http.Client 直接满足）。**必填**：nil ⇒ fail-closed。
	Doer HTTPDoer
	// Policy 是端点白名单策略。**必填**：nil ⇒ fail-closed；每次尝试前重跑运行期二次校验。
	Policy *EndpointPolicy
	// Timeout 是**单次尝试**的超时预算（每次尝试独立计时，不是整体调用预算）。
	Timeout time.Duration
	// MaxAttempts 是重试预算，**含首次尝试**（3 ⇒ 最多 3 次尝试）。
	MaxAttempts int
	// RetryBackoff 是两次尝试之间的固定等待。
	RetryBackoff time.Duration
}

// Client 是框架内**唯一**发起模型出域请求的组件（ 的框架侧一半：
// 业务侧「唯一出口 既有模型层」由 / 的守卫测试固化）。
//
// 出域职责链在框架侧只覆盖三段：**端点运行期二次校验 → 传输 → 降级错误**。
// 数据最小化 / 脱敏 / 永不外发黑名单 / 不可信数据围栏 / 审计由调用方在调用 Complete 之前完成
// （框架不复制这些口径，见 ADR-001 决策 4/5/7）。
//
// 构造后**只读**：无任何写操作，可被多 goroutine 并发复用（同 EndpointPolicy）。
type Client struct {
	doer        HTTPDoer
	policy      *EndpointPolicy
	timeout     time.Duration
	maxAttempts int
	backoff     time.Duration
	// sleep 是重试间隔的唯一时间来源，测试可替换为确定性实现（生产为真实定时器）。
	sleep func(ctx context.Context, d time.Duration) error
}

// NewClient 固化注入的传输与预算。doer / policy 缺失时不 panic：Complete 会返回
// ErrClientNotConfigured（fail-closed 且可识别）。
func NewClient(cfg ClientConfig) *Client {
	client := &Client{
		doer:        cfg.Doer,
		policy:      cfg.Policy,
		timeout:     cfg.Timeout,
		maxAttempts: cfg.MaxAttempts,
		backoff:     cfg.RetryBackoff,
	}
	if client.maxAttempts <= 0 {
		client.maxAttempts = 1
	}
	return client
}

// Complete 执行一次模型出域调用：编码 → 逐次尝试（端点校验 + 传输 + 解析）→ 降级错误。
//
// 失败语义（供上层转 `degraded=true` + `LOG:llm.error`）：
//   - 传输失败/超时 ⇒ ErrModelUnavailable / ErrModelTimeout（可重试）；
//   - 429/5xx ⇒ ErrModelStatus（可重试），其余 4xx ⇒ ErrModelStatus（不重试）；
//   - 非法 JSON / 无候选回复 ⇒ ErrModelMalformed / ErrModelNoChoices（不重试）；
//   - 端点白名单或禁跟随重定向被触发 ⇒ 原样返回  哨兵（**不重试、不伪装成模型不可用**）；
//   - 调用方 ctx 结束 ⇒ ErrModelCanceled / ErrModelTimeout（立即收口）。
func (c *Client) Complete(ctx context.Context, endpoint string, req ChatRequest) (*ChatResponse, error) {
	if err := c.ready(); err != nil {
		return nil, err
	}
	// payload 在重试之前编码一次：重试必须逐字节重放同一份内容，
	// 否则审计侧用注入 doer 复算 sent_hmac 会与首次口径不符。
	payload, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrRequestEncode, err)
	}

	var lastErr error
	attempts := 0
	for attempts < c.maxAttempts {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, abandon(ctxErr, lastErr, attempts)
		}
		attempts++
		out, err := c.attempt(ctx, endpoint, payload)
		if err == nil {
			return out, nil
		}
		lastErr = err
		if attempts >= c.maxAttempts || !isRetryable(err) {
			break
		}
		if waitErr := c.wait(ctx, c.backoff); waitErr != nil {
			return nil, abandon(waitErr, lastErr, attempts)
		}
	}
	return nil, fmt.Errorf("%w（共尝试 %d 次）", lastErr, attempts)
}

// ready 检查注入链完整性：任一环节缺失即 fail-closed 拒绝出域（不因「没配就没限制」而放行）。
func (c *Client) ready() error {
	if c.doer == nil {
		return fmt.Errorf("%w: HTTPDoer 未注入", ErrClientNotConfigured)
	}
	if c.policy == nil {
		return fmt.Errorf("%w: EndpointPolicy 未注入", ErrClientNotConfigured)
	}
	return nil
}

// attempt 执行一次尝试：单次尝试超时 → 构造请求 → 运行期二次校验 → 传输 → 解析。
func (c *Client) attempt(ctx context.Context, endpoint string, payload []byte) (*ChatResponse, error) {
	attemptCtx := ctx
	if c.timeout > 0 {
		var cancel context.CancelFunc
		attemptCtx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}

	httpReq, err := http.NewRequestWithContext(attemptCtx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		// 端点串在加载期已由 ValidateEndpoint 规范化，此处失败属调用方传参错误 ⇒ 不重试。
		return nil, fmt.Errorf("%w: %v", ErrRequestEncode, err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")

	// 运行期二次校验（ 策略）：每次尝试前重跑，失败即不出域且**不重试**。
	if err := c.policy.ValidateRequest(attemptCtx, httpReq); err != nil {
		return nil, err
	}

	resp, err := c.doer.Do(httpReq)
	if err != nil {
		return nil, classifyTransportErr(ctx, err)
	}
	return readChatResponse(httpReq, resp)
}

// readChatResponse 校验并解析响应：禁跟随重定向的兜底判定 → 状态码 → **有界读取（上限）** → JSON → 候选非空。
func readChatResponse(httpReq *http.Request, resp *http.Response) (*ChatResponse, error) {
	if resp == nil {
		return nil, fmt.Errorf("%w: 传输未返回响应", ErrModelUnavailable)
	}
	if resp.Body != nil {
		defer resp.Body.Close()
	}
	// 禁跟随重定向（ CheckRedirect）的兜底：注入的 doer 若未接 CheckRedirect 而自行跳转，
	// 响应会带着「实际请求已偏离原端点」的痕迹回来 ⇒ 在此拒绝，防绕过白名单。
	if resp.Request != nil && resp.Request.URL != nil && resp.Request.URL.String() != httpReq.URL.String() {
		return nil, fmt.Errorf("%w: 实际请求 %s 已偏离端点 %s",
			ErrRedirectNotAllowed, resp.Request.URL.Redacted(), httpReq.URL.Redacted())
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode > 299 {
		// 刻意不带响应体：模型错误体可能回显 prompt，审计/日志不得落原文（D7）。
		return nil, &statusError{code: resp.StatusCode}
	}
	if resp.Body == nil {
		return nil, fmt.Errorf("%w: 响应无 body", ErrModelMalformed)
	}
	// 有界读取：LimitReader 多读 1 字节即可判定「是否超限」，且内存占用封顶在 maxResponseBytes+1。
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		// 读到一半失败属传输层问题 ⇒ 归为可重试的不可用（不是「响应非法」）。
		return nil, fmt.Errorf("%w: 读取响应体失败: %v", ErrModelUnavailable, err)
	}
	if len(raw) > maxResponseBytes {
		return nil, fmt.Errorf("%w: 已读 %d 字节，超过上限 %d 字节", ErrModelTooLarge, len(raw), maxResponseBytes)
	}
	var out ChatResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrModelMalformed, err)
	}
	if len(out.Choices) == 0 {
		return nil, ErrModelNoChoices
	}
	return &out, nil
}

// statusError 承载非成功状态码，使「哪些状态可重试」可判定，同时不把响应体原文带进错误。
type statusError struct {
	code int
}

func (e *statusError) Error() string {
	return fmt.Sprintf("%s: HTTP %d", ErrModelStatus.Error(), e.code)
}

func (e *statusError) Unwrap() error { return ErrModelStatus }

// classifyTransportErr 把传输层错误归类成可识别错误（ErrModelUnavailable / Timeout / Canceled）。
//
// 顺序刻意固定：① 策略拒绝原样上抛（不得包装成可重试的「模型不可用」）；
// ② 调用方 ctx 已结束 ⇒ 立即收口；③ 其余超时信号 ⇒ Timeout；④ 兜底 ⇒ Unavailable。
func classifyTransportErr(ctx context.Context, err error) error {
	if isPolicyRejection(err) {
		return err
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("%w: %v", ctxClassification(ctxErr), err)
	}
	if errors.Is(err, context.DeadlineExceeded) || isTimeout(err) {
		return fmt.Errorf("%w: %v", ErrModelTimeout, err)
	}
	if errors.Is(err, context.Canceled) {
		return fmt.Errorf("%w: %v", ErrModelCanceled, err)
	}
	return fmt.Errorf("%w: %v", ErrModelUnavailable, err)
}

// abandon 在调用方 ctx 结束（取消/超时）时收口：不再重试，把原因与已尝试次数一并交回。
func abandon(ctxErr error, lastErr error, attempts int) error {
	reason := fmt.Errorf("%w: %v", ctxClassification(ctxErr), ctxErr)
	if lastErr == nil {
		return fmt.Errorf("%w（尚未发起尝试）", reason)
	}
	return fmt.Errorf("%w（共尝试 %d 次，末次: %v）", reason, attempts, lastErr)
}

// ctxClassification 把调用方 ctx 的结束原因映射成可识别错误。
func ctxClassification(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return ErrModelTimeout
	}
	return ErrModelCanceled
}

// isRetryable 判定错误可否重试：仅传输类失败与 429/5xx 可重试。
// 策略拒绝（端点/重定向）、非法编码、非法响应体一律不重试 ——
// 重试策略拒绝既无意义，又会把 fail-closed 的「拒绝」伪装成「模型不可用」。
func isRetryable(err error) bool {
	switch {
	case errors.Is(err, ErrModelTimeout), errors.Is(err, ErrModelUnavailable):
		return true
	case errors.Is(err, ErrModelStatus):
		var se *statusError
		return errors.As(err, &se) && se.retryable()
	default:
		return false
	}
}

func (e *statusError) retryable() bool {
	return e.code == http.StatusTooManyRequests || e.code >= http.StatusInternalServerError
}

// policyRejections 是  端点策略的全部哨兵（复用其口径，不另立一份）。
var policyRejections = []error{
	ErrRedirectNotAllowed,
	ErrEndpointEmpty,
	ErrEndpointMalformed,
	ErrEndpointScheme,
	ErrEndpointNotAllowlisted,
	ErrEndpointPublic,
	ErrEndpointUnresolvable,
}

// isPolicyRejection 判定传输层错误是否其实是  端点策略的拒绝。
func isPolicyRejection(err error) bool {
	for _, sentinel := range policyRejections {
		if errors.Is(err, sentinel) {
			return true
		}
	}
	return false
}

// isTimeout 识别 net.Error 超时（连接/读写超时，未必来自 context）。
func isTimeout(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// wait 是重试间隔的唯一时间来源；ctx 已结束或无需等待时立即返回。
func (c *Client) wait(ctx context.Context, d time.Duration) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if d <= 0 {
		return nil
	}
	if c.sleep != nil {
		return c.sleep(ctx, d)
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
