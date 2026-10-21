package agent

import (
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/malizhi2202-dev/agentkit/kitctx"
	"github.com/malizhi2202-dev/agentkit/model"
	"github.com/malizhi2202-dev/agentkit/tools"
)

// ---------- 测试替身 ----------

// captureLogger 记录注入日志：断言「降级可识别（LOG:llm.error{reason}）」、「出域拒绝不伪装成模型不可用」
// 与「全序列落 LOG:engine.trail[]」。
type captureLogger struct {
	mu      sync.Mutex
	entries []logEntry
}

type logEntry struct {
	msg    string
	fields map[string]any
}

func (l *captureLogger) add(msg string, fields map[string]any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	cp := make(map[string]any, len(fields))
	for k, v := range fields {
		cp[k] = v
	}
	l.entries = append(l.entries, logEntry{msg: msg, fields: cp})
}

func (l *captureLogger) Debug(msg string, fields map[string]any) { l.add(msg, fields) }
func (l *captureLogger) Info(msg string, fields map[string]any)  { l.add(msg, fields) }
func (l *captureLogger) Warn(msg string, fields map[string]any)  { l.add(msg, fields) }
func (l *captureLogger) Error(msg string, fields map[string]any) { l.add(msg, fields) }

func (l *captureLogger) all() []logEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]logEntry, len(l.entries))
	copy(out, l.entries)
	return out
}

// count 返回某事件的条数。
func (l *captureLogger) count(msg string) int {
	n := 0
	for _, e := range l.all() {
		if e.msg == msg {
			n++
		}
	}
	return n
}

// find 取第一条该事件的日志。
func (l *captureLogger) find(msg string) (logEntry, bool) {
	for _, e := range l.all() {
		if e.msg == msg {
			return e, true
		}
	}
	return logEntry{}, false
}

// doerStep 是一次脚本化出域的产物：响应体 / 传输错误 / 阻塞到 ctx 结束（确定性复现「模型超时」）。
type doerStep struct {
	body   string
	err    error
	status int  // 0 ⇒ 200
	hang   bool // 阻塞到本次尝试 ctx 结束
}

// scriptDoer 是**位置脚本化**的出域传输：按调用次序吐出预置响应（脚本耗尽后重复最后一步），
// 并记录每次出域实际发送的 ChatRequest（断言工具声明按角色裁剪）。
type scriptDoer struct {
	mu    sync.Mutex
	steps []doerStep
	sent  []model.ChatRequest
}

func (d *scriptDoer) Do(req *http.Request) (*http.Response, error) {
	sent, err := decodeChatRequest(req)
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	idx := len(d.sent)
	d.sent = append(d.sent, sent)
	if len(d.steps) == 0 {
		d.mu.Unlock()
		return nil, errors.New("scriptDoer: 脚本为空")
	}
	if idx >= len(d.steps) {
		idx = len(d.steps) - 1
	}
	step := d.steps[idx]
	d.mu.Unlock()

	if step.hang {
		<-req.Context().Done()
		return nil, req.Context().Err()
	}
	if step.err != nil {
		return nil, step.err
	}
	return rawOK(step.status, step.body), nil
}

func (d *scriptDoer) calls() []model.ChatRequest {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]model.ChatRequest(nil), d.sent...)
}

func (d *scriptDoer) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.sent)
}

// roundDoer 是**内容驱动**的出域传输：本轮的 messages 里已有 tool 结果 ⇒ 给文本收口，否则请求一次工具调用。
// 因此它的行为只依赖「该次 Run 自己的消息序列」，与并发交错无关 —— 用于共享 Loop 的并发用例。
type roundDoer struct{ calls atomic.Int64 }

func (d *roundDoer) Do(req *http.Request) (*http.Response, error) {
	d.calls.Add(1)
	sent, err := decodeChatRequest(req)
	if err != nil {
		return nil, err
	}
	for _, msg := range sent.Messages {
		if msg.Role == "tool" {
			return rawOK(0, mustChatBody(model.ChatMessage{Role: "assistant", Content: "并发完成"})), nil
		}
	}
	return rawOK(0, mustChatBody(model.ChatMessage{
		Role:      "assistant",
		ToolCalls: []model.ToolCall{call("alpha", `{}`), callRaw("call-alpha-2", "alpha", `{}`)},
	})), nil
}

func (d *roundDoer) count() int64 { return d.calls.Load() }

func decodeChatRequest(req *http.Request) (model.ChatRequest, error) {
	var sent model.ChatRequest
	if req == nil || req.Body == nil {
		return sent, nil
	}
	raw, err := io.ReadAll(req.Body)
	if err != nil {
		return sent, err
	}
	_ = req.Body.Close()
	if err := json.Unmarshal(raw, &sent); err != nil {
		return sent, err
	}
	return sent, nil
}

func rawOK(status int, body string) *http.Response {
	if status == 0 {
		status = http.StatusOK
	}
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// ---------- 响应体构造 ----------

func mustChatBody(msg model.ChatMessage) string {
	raw, err := json.Marshal(model.ChatResponse{
		Choices: []model.ChatChoice{{Index: 0, Message: msg, FinishReason: "stop"}},
	})
	if err != nil {
		panic(err)
	}
	return string(raw)
}

func toolCallBody(t *testing.T, calls ...model.ToolCall) string {
	t.Helper()
	return mustChatBody(model.ChatMessage{Role: "assistant", ToolCalls: calls})
}

func textBody(t *testing.T, text string) string {
	t.Helper()
	return mustChatBody(model.ChatMessage{Role: "assistant", Content: text})
}

func call(name, arguments string) model.ToolCall {
	return callRaw("call-"+name, name, arguments)
}

func callRaw(id, name, arguments string) model.ToolCall {
	return model.ToolCall{
		ID:       id,
		Type:     "function",
		Function: model.ToolFunction{Name: name, Arguments: arguments},
	}
}

// ---------- 注册表 / 上下文 / 客户端构造 ----------

const (
	testTraceID  = "trace-loop-0001"
	testEndpoint = "http://10.0.0.1:8081/v1/chat/completions"
	testRole     = "analyst"
	// secretArg / secretValue 是「参数原文」与「结果原文」探针：轨迹里必须一个都找不到。
	secretArg   = "13800138000"
	secretValue = "SECRET-RAW-VALUE"
)

func testTool(name string, run tools.RunFunc) tools.Tool {
	return tools.Tool{Name: name, Params: map[string]any{"type": "object"}, Summary: "测试工具 " + name, Run: run}
}

func okTool(name string, value any) tools.Tool {
	return testTool(name, func(context.Context, map[string]any) (any, error) { return value, nil })
}

func testRegistry(t *testing.T, list ...tools.Tool) *tools.Registry {
	t.Helper()
	reg, err := tools.NewRegistry(list...)
	if err != nil {
		t.Fatalf("构造注册表失败：%v", err)
	}
	return reg
}

func loopRequest(traceID, role string) kitctx.RequestContext {
	return kitctx.RequestContext{UserID: "u-1", Role: kitctx.Role(role), TraceID: traceID}
}

func withLimits(rc kitctx.RequestContext, limits kitctx.Limits) kitctx.RequestContext {
	rc.Limits = limits
	return rc
}

func loopCtx(rc kitctx.RequestContext, logger kitctx.Logger) context.Context {
	return loopCtxWith(context.Background(), rc, logger)
}

func loopCtxWith(ctx context.Context, rc kitctx.RequestContext, logger kitctx.Logger) context.Context {
	rc.Injections.Logger = logger
	return kitctx.WithRequest(ctx, rc)
}

// newLoopClient 构造被测出域客户端：零配置策略（域名一律拒绝、私网/回环字面量可用）+ 单次尝试。
func newLoopClient(doer model.HTTPDoer, mutate func(*model.ClientConfig)) *model.Client {
	cfg := model.ClientConfig{Doer: doer, Policy: model.NewEndpointPolicy(model.EndpointPolicyConfig{}), MaxAttempts: 1}
	if mutate != nil {
		mutate(&cfg)
	}
	return model.NewClient(cfg)
}

func userMessages() []model.ChatMessage {
	return []model.ChatMessage{
		{Role: "system", Content: "你是宿主入口词助手"},
		{Role: "user", Content: "查一下 1.2.3.4"},
	}
}

func stepNames(steps []kitctx.Step) []string {
	out := make([]string, 0, len(steps))
	for _, s := range steps {
		out = append(out, s.Name)
	}
	return out
}

func toolNames(decls []model.ToolVisible) []string {
	out := make([]string, 0, len(decls))
	for _, d := range decls {
		out = append(out, d.Name)
	}
	return out
}

func toolResultMessage(t *testing.T, sent model.ChatRequest, callID string) model.ChatMessage {
	t.Helper()
	for _, msg := range sent.Messages {
		if msg.Role == "tool" && msg.ToolCallID == callID {
			return msg
		}
	}
	t.Fatalf("出域 payload 里缺少 tool_call_id=%q 的回填消息", callID)
	return model.ChatMessage{}
}

// ---------- ：模型自选工具（预定义路径不参与调度） ----------

// 模型给出的顺序刻意「不自然」且**重复调用同一工具**：轨迹必须逐位等于模型给出的调用序，
// 次数也由模型决定 —— 框架侧不存在任何预设序列。
func TestLoop_ModelChoosesTools(t *testing.T) {
	doer := &scriptDoer{steps: []doerStep{
		{body: toolCallBody(t, call("beta", `{}`), call("alpha", `{}`))},
		{body: toolCallBody(t, call("gamma", `{}`), call("beta", `{}`))},
		{body: textBody(t, "宿主入口词完成")},
	}}
	reg := testRegistry(t, okTool("alpha", "A"), okTool("beta", "B"), okTool("gamma", "C"))
	loop := NewLoop(LoopConfig{Registry: reg, Client: newLoopClient(doer, nil), Endpoint: testEndpoint})

	out, err := loop.Run(loopCtx(loopRequest(testTraceID, testRole), kitctx.NopLogger()), userMessages())
	if err != nil {
		t.Fatalf("模型自选工具的正常流程不得报错：%v", err)
	}

	want := []string{"beta", "alpha", "gamma", "beta"}
	if got := stepNames(out.Steps); !reflect.DeepEqual(got, want) {
		t.Fatalf("轨迹必须逐位等于模型给出的调用序\n got=%v\nwant=%v", got, want)
	}
	for i, step := range out.Steps {
		if step.Seq != i+1 {
			t.Fatalf("steps[%d].Seq 应为 %d（连续且按调用序），实际 %d", i, i+1, step.Seq)
		}
		if step.Status != kitctx.StepStatusDone {
			t.Fatalf("steps[%d] 应为 done，实际 %q", i, step.Status)
		}
		if step.TraceID != testTraceID {
			t.Fatalf("steps[%d].TraceID 应为 %q，实际 %q", i, testTraceID, step.TraceID)
		}
	}
	if out.Verdict != "宿主入口词完成" {
		t.Fatalf("模型不再请求工具时应取文本作为 verdict，实际 %q", out.Verdict)
	}
	if out.StepsLimited || out.Degraded {
		t.Fatalf("正常完成不得带 steps_limited / degraded（实际 limited=%v degraded=%v）", out.StepsLimited, out.Degraded)
	}
	if len(out.Gaps) != 0 || len(out.Rejected) != 0 {
		t.Fatalf("无失败/拒绝步骤：gaps=%v rejected=%v", out.Gaps, out.Rejected)
	}
	if got := doer.count(); got != 3 {
		t.Fatalf("三轮对话 ⇒ 三次出域，实际 %d 次", got)
	}
	if len(reg.Names()) != 3 {
		t.Fatalf("三个工具都在册，但只有模型点名的被执行（注册表不应被循环改写）")
	}
}

// 模型一个工具都不点 ⇒ 一个工具都不执行（不替模型「顺手」跑预定义路径）。
func TestLoop_ModelChoosesTools_NoToolCalls(t *testing.T) {
	var executed atomic.Int64
	reg := testRegistry(t, testTool("alpha", func(context.Context, map[string]any) (any, error) {
		executed.Add(1)
		return "A", nil
	}))
	doer := &scriptDoer{steps: []doerStep{{body: textBody(t, "直接回答")}}}
	loop := NewLoop(LoopConfig{Registry: reg, Client: newLoopClient(doer, nil), Endpoint: testEndpoint})

	out, err := loop.Run(loopCtx(loopRequest(testTraceID, testRole), kitctx.NopLogger()), userMessages())
	if err != nil {
		t.Fatalf("纯对话不得报错：%v", err)
	}
	if got := executed.Load(); got != 0 {
		t.Fatalf("模型没点名任何工具时不得执行任何工具，实际执行 %d 次", got)
	}
	if len(out.Steps) != 0 {
		t.Fatalf("无工具调用 ⇒ 轨迹为空，实际 %d 步", len(out.Steps))
	}
	if out.Verdict != "直接回答" {
		t.Fatalf("verdict 应为模型文本，实际 %q", out.Verdict)
	}
}

// ：工具声明在**本层**按角色裁剪后才出域（admin 专属工具根本不出现）。
func TestLoop_ToolsTrimmedByRole(t *testing.T) {
	doer := &scriptDoer{steps: []doerStep{{body: textBody(t, "ok")}}}
	reg := testRegistry(t,
		okTool("user.profile", "P"),
		tools.Tool{
			Name: "query_log", Params: map[string]any{"type": "object"}, Summary: "admin 专属日志问答",
			RequiredRoles: []string{"admin"},
			Run:           func(context.Context, map[string]any) (any, error) { return "L", nil },
		},
	)
	loop := NewLoop(LoopConfig{Registry: reg, Client: newLoopClient(doer, nil), Endpoint: testEndpoint})

	if _, err := loop.Run(loopCtx(loopRequest(testTraceID, testRole), kitctx.NopLogger()), userMessages()); err != nil {
		t.Fatalf("analyst 的正常对话不得报错：%v", err)
	}
	sent := doer.calls()[0]
	if got := toolNames(sent.Tools); !reflect.DeepEqual(got, []string{"user.profile"}) {
		t.Fatalf("analyst 只应看到 user.profile，实际 %v", got)
	}

	if _, err := loop.Run(loopCtx(loopRequest(testTraceID, "admin"), kitctx.NopLogger()), userMessages()); err != nil {
		t.Fatalf("admin 的对话不得报错：%v", err)
	}
	if got := toolNames(doer.calls()[1].Tools); !reflect.DeepEqual(got, []string{"user.profile", "query_log"}) {
		t.Fatalf("admin 应看到全部工具（注册顺序），实际 %v", got)
	}
}

// ---------- ：步数上限只来自 Limits；≤0 = 不设上限（框架不补默认值） ----------

func TestLoop_MaxStepsTerminates(t *testing.T) {
	t.Run("达到上限即终止并给出逐字文案", func(t *testing.T) {
		doer := &scriptDoer{steps: []doerStep{{body: toolCallBody(t, call("alpha", `{}`))}}}
		loop := NewLoop(LoopConfig{
			Registry: testRegistry(t, okTool("alpha", "A")),
			Client:   newLoopClient(doer, nil),
			Endpoint: testEndpoint,
		})
		logger := &captureLogger{}
		rc := withLimits(loopRequest(testTraceID, testRole), kitctx.Limits{MaxSteps: 2})

		out, err := loop.Run(loopCtx(rc, logger), userMessages())
		if err != nil {
			t.Fatalf("步数超限是正常收口，不得报错：%v", err)
		}
		if !out.StepsLimited {
			t.Fatal("达到上限必须置 steps_limited=true（契约点）")
		}
		if out.Notice != NoticeStepsLimited {
			t.Fatalf("文案必须逐字为 %q，实际 %q", NoticeStepsLimited, out.Notice)
		}
		if len(out.Steps) != 2 {
			t.Fatalf("上限 2 ⇒ 恰好执行 2 步，实际 %d 步", len(out.Steps))
		}
		if out.Verdict != "" {
			t.Fatalf("超限终止不产出模型段，实际 verdict=%q", out.Verdict)
		}
		if out.Degraded {
			t.Fatal("步数超限不是模型降级")
		}
		if got := doer.count(); got != 3 {
			t.Fatalf("第 3 轮模型请求发生时预算已耗尽 ⇒ 该轮调用不得执行（出域 3 次），实际 %d 次", got)
		}
	})

	t.Run("MaxSteps==0 表示不设上限（不补默认值）", func(t *testing.T) {
		doer := &scriptDoer{steps: []doerStep{
			{body: toolCallBody(t, call("alpha", `{}`))},
			{body: toolCallBody(t, call("alpha", `{}`))},
			{body: toolCallBody(t, call("alpha", `{}`))},
			{body: textBody(t, "结束")},
		}}
		loop := NewLoop(LoopConfig{
			Registry: testRegistry(t, okTool("alpha", "A")),
			Client:   newLoopClient(doer, nil),
			Endpoint: testEndpoint,
		})
		rc := withLimits(loopRequest(testTraceID, testRole), kitctx.Limits{MaxSteps: 0})

		out, err := loop.Run(loopCtx(rc, kitctx.NopLogger()), userMessages())
		if err != nil {
			t.Fatalf("不得报错：%v", err)
		}
		if out.StepsLimited {
			t.Fatal("MaxSteps<=0 表示「不设上限」——框架不得偷偷补一个默认上限")
		}
		if len(out.Steps) != 3 {
			t.Fatalf("模型要 3 轮工具就应有 3 步，实际 %d 步", len(out.Steps))
		}
		if out.Verdict != "结束" {
			t.Fatalf("verdict 应为 %q，实际 %q", "结束", out.Verdict)
		}
	})

	t.Run("负数同样视为不设上限", func(t *testing.T) {
		doer := &scriptDoer{steps: []doerStep{
			{body: toolCallBody(t, call("alpha", `{}`))},
			{body: toolCallBody(t, call("alpha", `{}`))},
			{body: textBody(t, "结束")},
		}}
		loop := NewLoop(LoopConfig{
			Registry: testRegistry(t, okTool("alpha", "A")),
			Client:   newLoopClient(doer, nil),
			Endpoint: testEndpoint,
		})
		rc := withLimits(loopRequest(testTraceID, testRole), kitctx.Limits{MaxSteps: -5})

		out, err := loop.Run(loopCtx(rc, kitctx.NopLogger()), userMessages())
		if err != nil {
			t.Fatalf("不得报错：%v", err)
		}
		if out.StepsLimited || len(out.Steps) != 2 {
			t.Fatalf("MaxSteps<0 ⇒ 不设上限：limited=%v steps=%d", out.StepsLimited, len(out.Steps))
		}
	})

	t.Run("单批请求超过剩余预算时只执行剩余预算", func(t *testing.T) {
		var gamma atomic.Int64
		reg := testRegistry(t,
			okTool("alpha", "A"),
			okTool("beta", "B"),
			testTool("gamma", func(context.Context, map[string]any) (any, error) { gamma.Add(1); return "C", nil }),
		)
		doer := &scriptDoer{steps: []doerStep{{body: toolCallBody(t,
			call("alpha", `{}`), call("beta", `{}`), call("gamma", `{}`))}}}
		loop := NewLoop(LoopConfig{Registry: reg, Client: newLoopClient(doer, nil), Endpoint: testEndpoint})
		rc := withLimits(loopRequest(testTraceID, testRole), kitctx.Limits{MaxSteps: 2})

		out, err := loop.Run(loopCtx(rc, kitctx.NopLogger()), userMessages())
		if err != nil {
			t.Fatalf("不得报错：%v", err)
		}
		if got := stepNames(out.Steps); !reflect.DeepEqual(got, []string{"alpha", "beta"}) {
			t.Fatalf("只应执行剩余预算内的前 2 个（按模型给出的顺序），实际 %v", got)
		}
		if !out.StepsLimited {
			t.Fatal("预算被单批请求吃满 ⇒ 必须置 steps_limited")
		}
		if gamma.Load() != 0 {
			t.Fatal("超出预算的调用不得被执行")
		}
		if got := doer.count(); got != 1 {
			t.Fatalf("应只出域 1 次，实际 %d 次", got)
		}
	})
}

// ---------- 预算裁剪规则（planBatch）：一步 = 一次工具调用；≤0 = 不设上限 ----------

func TestLoop_PlanBatchBudget(t *testing.T) {
	calls := []model.ToolCall{call("a", `{}`), call("b", `{}`), call("c", `{}`)}

	cases := []struct {
		name          string
		maxSteps      int
		remaining     int
		wantLen       int
		wantLimited   bool
		wantExhausted bool
	}{
		{"不设上限（MaxSteps==0）", 0, 0, 3, false, false},
		{"不设上限（MaxSteps<0）", -3, -3, 3, false, false},
		{"预算耗尽 ⇒ 本轮不执行", 2, 0, 0, false, true},
		{"单批超预算 ⇒ 截断后收口", 2, 2, 2, true, false},
		{"预算充足 ⇒ 全批执行", 5, 5, 3, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			batch, limited, exhausted := planBatch(calls, tc.maxSteps, tc.remaining)
			if len(batch) != tc.wantLen || limited != tc.wantLimited || exhausted != tc.wantExhausted {
				t.Fatalf("planBatch(max=%d,remaining=%d) = len %d, limited=%v, exhausted=%v；应为 len %d, limited=%v, exhausted=%v",
					tc.maxSteps, tc.remaining, len(batch), limited, exhausted, tc.wantLen, tc.wantLimited, tc.wantExhausted)
			}
		})
	}
}

// ---------- L-029 契约 5：预算未判定必须留可 grep 的安全信号（语义仍是不设上限） ----------

func TestLoop_UnboundedBudgetIsObservable(t *testing.T) {
	// 模型连续两轮点名工具后收口：证明「告警只与预算判定有关，与跑了几轮无关」。
	newDoer := func() *scriptDoer {
		return &scriptDoer{steps: []doerStep{
			{body: toolCallBody(t, call("alpha", `{}`))},
			{body: toolCallBody(t, call("alpha", `{}`))},
			{body: textBody(t, "收口")},
		}}
	}
	newUnboundedLoop := func() *Loop {
		return NewLoop(LoopConfig{
			Registry: testRegistry(t, okTool("alpha", "A")),
			Client:   newLoopClient(newDoer(), nil),
			Endpoint: testEndpoint,
		})
	}

	t.Run("MaxSteps<=0 ⇒ 记 llm.loop.unbounded 且语义仍为不设上限", func(t *testing.T) {
		for _, maxSteps := range []int{0, -5} {
			logger := &captureLogger{}
			rc := withLimits(loopRequest(testTraceID, testRole), kitctx.Limits{MaxSteps: maxSteps})
			out, err := newUnboundedLoop().Run(loopCtx(rc, logger), userMessages())
			if err != nil {
				t.Fatalf("MaxSteps=%d 不得报错：%v", maxSteps, err)
			}
			// ① 告警必须被记录：事件名 + 稳定 reason token + trace_id（每请求恰一次，不落原文）。
			if n := logger.count("llm.loop.unbounded"); n != 1 {
				t.Fatalf("MaxSteps=%d ⇒ 必须恰记 1 条 llm.loop.unbounded，实际 %d 条", maxSteps, n)
			}
			entry, ok := logger.find("llm.loop.unbounded")
			if !ok {
				t.Fatal("预算未判定必须留可 grep 的安全信号（禁止静默不限步）")
			}
			if got := entry.fields["reason"]; got != reasonMaxStepsUndetermined {
				t.Fatalf("reason 必须是稳定 token %q，实际 %v", reasonMaxStepsUndetermined, got)
			}
			if got := entry.fields["trace_id"]; got != testTraceID {
				t.Fatalf("告警必须带 trace_id 以便定位，实际 %v", got)
			}
			// ② 语义完全不变：仍不设上限（跑满两轮工具、不判 steps_limited、不产出 notice）。
			if out.StepsLimited || out.Notice != "" {
				t.Fatalf("MaxSteps<=0 仍须「不设上限」，实际 limited=%v notice=%q", out.StepsLimited, out.Notice)
			}
			if len(out.Steps) != 2 || out.Verdict != "收口" {
				t.Fatalf("MaxSteps<=0 仍须跑满两轮并收口，实际 steps=%d verdict=%q", len(out.Steps), out.Verdict)
			}
		}
	})

	t.Run("MaxSteps>0 ⇒ 不得记该告警", func(t *testing.T) {
		logger := &captureLogger{}
		rc := withLimits(loopRequest(testTraceID, testRole), kitctx.Limits{MaxSteps: 9})
		out, err := newUnboundedLoop().Run(loopCtx(rc, logger), userMessages())
		if err != nil {
			t.Fatalf("不得报错：%v", err)
		}
		if n := logger.count("llm.loop.unbounded"); n != 0 {
			entry, _ := logger.find("llm.loop.unbounded")
			t.Fatalf("预算已判定（MaxSteps=9）不得记 llm.loop.unbounded，实际 %d 条：%v", n, entry.fields)
		}
		if out.StepsLimited || len(out.Steps) != 2 {
			t.Fatalf("MaxSteps=9 应正常跑满两轮，实际 limited=%v steps=%d", out.StepsLimited, len(out.Steps))
		}
	})
}

// ---------- 分类闭集守卫：降级类别是手写枚举，必须被测试钉死两个方向 ----------
//
// 为什么需要这个测试：`isDegradable` 是「手写枚举 + return false 兜底」，因此两个方向都可能悄悄漂移 ——
// ① 将来 `model` 加一个新哨兵却忘了分类 ⇒ 静默落入「不降级」；
// ② 更危险的反方向：有人把兜底改成 `return true`（看着更「稳健」）⇒ **全部安全拒绝**（端点白名单/重定向）
//    立刻变成静默降级，`TestLoop_EgressRefusalNotDisguisedAsDegraded` 建立的关键区分消失。
// 本测试用「表驱动枚举 + AST 闭集完整性」把这个边界从注释升格为可执行断言。

// modelSentinels 用 AST 扫 `../model/*.go`（排除 `_test.go`），取出**全部导出的 `Err*` 包级变量名**。
// 用途：分类表必须是**闭集** —— 新增哨兵未分类时，完整性断言立即失败，不依赖人肉维护注释。
func modelSentinels(t *testing.T) map[string]bool {
	t.Helper()
	dir := filepath.Join("..", "model") // go test 的工作目录 = 本包源码目录
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("AST 守卫读不到 %s：%v", dir, err)
	}
	fset := token.NewFileSet()
	names := map[string]bool{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatalf("AST 守卫解析 %s 失败：%v", name, err)
		}
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.VAR {
				continue
			}
			for _, spec := range gen.Specs {
				valueSpec, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for _, ident := range valueSpec.Names {
					if ast.IsExported(ident.Name) && strings.HasPrefix(ident.Name, "Err") {
						names[ident.Name] = true
					}
				}
			}
		}
	}
	if len(names) == 0 {
		t.Fatal("AST 守卫必须扫到 model 包的导出 Err* 哨兵；0 命中说明路径或口径已失效（守卫空转）")
	}
	return names
}

func TestLoop_DegradableClassificationIsClosed(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		degradable bool
	}{
		// —— 模型侧故障 / 出域客户端未配置：**必须降级**——
		{"model.ErrClientNotConfigured", model.ErrClientNotConfigured, true},
		{"model.ErrModelUnavailable", model.ErrModelUnavailable, true},
		{"model.ErrModelTimeout", model.ErrModelTimeout, true},
		{"model.ErrModelCanceled", model.ErrModelCanceled, true},
		{"model.ErrModelStatus", model.ErrModelStatus, true},
		{"model.ErrModelMalformed", model.ErrModelMalformed, true},
		{"model.ErrModelTooLarge", model.ErrModelTooLarge, true},
		{"model.ErrModelNoChoices", model.ErrModelNoChoices, true},
		// —— 端点「未判定/未配置」：**降级**——
		{"model.ErrEndpointEmpty", model.ErrEndpointEmpty, true},
		// —— 端点「配置非法 / 策略拒绝」：**不降级**（：安全拒绝原样上抛，绝不伪装成模型抖动）——
		//    注意「未配置」（Empty ⇒ 降级）与「配错」（Malformed/Scheme ⇒ 中止）是**刻意区分**的：
		//    缺失 ≠ 非法，把非法配置静默降级会让真实故障消失。
		{"model.ErrEndpointMalformed", model.ErrEndpointMalformed, false},
		{"model.ErrEndpointScheme", model.ErrEndpointScheme, false},
		{"model.ErrEndpointNotAllowlisted", model.ErrEndpointNotAllowlisted, false},
		{"model.ErrEndpointPublic", model.ErrEndpointPublic, false},
		{"model.ErrEndpointUnresolvable", model.ErrEndpointUnresolvable, false},
		{"model.ErrRedirectNotAllowed", model.ErrRedirectNotAllowed, false},
		// —— 出域请求编码缺陷：**不降级**（程序缺陷，不是模型不可用）——
		{"model.ErrRequestEncode", model.ErrRequestEncode, false},
	}

	// ① 闭集完整性：分类表必须**恰好**覆盖 model 包全部导出 Err* 哨兵（AST 扫描，非人肉清单）。
	t.Run("闭集完整性（AST 覆盖 model 全部导出 Err*）", func(t *testing.T) {
		want := modelSentinels(t)
		got := make(map[string]bool, len(cases))
		for _, tc := range cases {
			got[strings.TrimPrefix(tc.name, "model.")] = true
		}
		for name := range want {
			if !got[name] {
				t.Errorf("model 包哨兵 %s 未被分类表覆盖 ⇒ 它现在静默落入「不降级」；请在 isDegradable 与分类表里各加一条（契约点/5 判定）", name)
			}
		}
		for name := range got {
			if !want[name] {
				t.Errorf("分类表列了 %s，但 model 包的非测试文件里没有这个导出哨兵（改名/删除后未同步）", name)
			}
		}
		if len(got) != len(want) {
			t.Errorf("分类表 %d 条 ≠ model 包哨兵 %d 个", len(got), len(want))
		}
	})

	// ② 逐条：`isDegradable` 与期望一致，且**经 `abort` 的可观察行为**也一致
	//    （只看布尔值不够 —— 真正要钉死的是「降级 vs 中止」这两条外部可见路径）。
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isDegradable(tc.err); got != tc.degradable {
				t.Fatalf("isDegradable(%s) = %v，应为 %v", tc.name, got, tc.degradable)
			}
			logger := &captureLogger{}
			out, err := (&Loop{}).abort(LoopOutcome{}, kitctx.NewTrace(testTraceID), logger, tc.err)
			if !errors.Is(out.Err, tc.err) {
				t.Fatalf("无论降级或中止，Outcome.Err 都必须保留原哨兵以便 errors.Is，实际 %v", out.Err)
			}
			if tc.degradable {
				if err != nil {
					t.Fatalf("降级不得返回 error（请求整体不阻塞），实际 %v", err)
				}
				if !out.Degraded || out.Notice != NoticeModelUnavailable {
					t.Fatalf("降级必须置 Degraded 且文案逐字为 %q，实际 degraded=%v notice=%q", NoticeModelUnavailable, out.Degraded, out.Notice)
				}
				if n := logger.count("llm.error"); n != 1 {
					t.Fatalf("降级必须记 1 条 llm.error，实际 %d 条", n)
				}
				if n := logger.count("llm.endpoint.rejected"); n != 0 {
					t.Fatalf("降级不得记 llm.endpoint.rejected（否则运维会去重试不可能成功的调用），实际 %d 条", n)
				}
				return
			}
			if err == nil {
				t.Fatal("安全拒绝/配置缺陷必须原样上抛 error，不得静默降级")
			}
			if !errors.Is(err, tc.err) {
				t.Fatalf("必须原样上抛同一哨兵（不得包装成模型不可用），实际 %v", err)
			}
			if out.Degraded || out.Notice != "" {
				t.Fatalf("不得降级：degraded=%v notice=%q", out.Degraded, out.Notice)
			}
			if n := logger.count("llm.error"); n != 0 {
				t.Fatalf("不得记 llm.error（它表示模型抖动），实际 %d 条", n)
			}
			// 口径：非降级中止统一记 llm.endpoint.rejected（编码缺陷亦落此事件，靠 reason token 区分）。
			if n := logger.count("llm.endpoint.rejected"); n != 1 {
				t.Fatalf("非降级中止必须记 1 条 llm.endpoint.rejected，实际 %d 条", n)
			}
		})
	}
}

// ---------- L-015 契约 3 / L-024 契约 1：每步超时来自 Limits.StepTimeout；≤0 = 不设 ----------

func TestLoop_StepTimeoutFromLimits(t *testing.T) {
	t.Run("StepTimeout>0 时超时步骤判失败并记缺口（整体继续）", func(t *testing.T) {
		hang := testTool("slow", func(ctx context.Context, _ map[string]any) (any, error) {
			<-ctx.Done()
			return secretValue, ctx.Err() // 执行体即便返回了值，也不得被当成成功（任务 契约）
		})
		doer := &scriptDoer{steps: []doerStep{
			{body: toolCallBody(t, call("slow", `{}`))},
			{body: textBody(t, "降级继续")},
		}}
		loop := NewLoop(LoopConfig{
			Registry: testRegistry(t, hang),
			Client:   newLoopClient(doer, nil),
			Endpoint: testEndpoint,
		})
		rc := withLimits(loopRequest(testTraceID, testRole), kitctx.Limits{MaxSteps: 4, StepTimeout: 20 * time.Millisecond})

		out, err := loop.Run(loopCtx(rc, kitctx.NopLogger()), userMessages())
		if err != nil {
			t.Fatalf("单步超时是「该步失败、整体继续」，不得报错：%v", err)
		}
		if len(out.Steps) != 1 || out.Steps[0].Status != kitctx.StepStatusFailed {
			t.Fatalf("超时步骤必须记为 failed，实际 %+v", out.Steps)
		}
		if len(out.Gaps) != 1 || out.Gaps[0].Source != "slow" || out.Gaps[0].Reason != reasonToolTimeout {
			t.Fatalf("超时必须产出缺口 {source:slow, reason:%s}，实际 %v", reasonToolTimeout, out.Gaps)
		}
		if out.Verdict != "降级继续" {
			t.Fatalf("单步失败后整体继续（应取到下一轮文本），实际 verdict=%q", out.Verdict)
		}
		if strings.Contains(out.Steps[0].ResultSummary, secretValue) {
			t.Fatalf("结果摘要不得落原文：%q", out.Steps[0].ResultSummary)
		}
	})

	t.Run("StepTimeout<=0 时不设超时（框架不补默认值）", func(t *testing.T) {
		slow := testTool("slow", func(context.Context, map[string]any) (any, error) {
			time.Sleep(30 * time.Millisecond)
			return "ok", nil
		})
		doer := &scriptDoer{steps: []doerStep{
			{body: toolCallBody(t, call("slow", `{}`))},
			{body: textBody(t, "完成")},
		}}
		loop := NewLoop(LoopConfig{
			Registry: testRegistry(t, slow),
			Client:   newLoopClient(doer, nil),
			Endpoint: testEndpoint,
		})
		rc := withLimits(loopRequest(testTraceID, testRole), kitctx.Limits{MaxSteps: 4, StepTimeout: 0})

		out, err := loop.Run(loopCtx(rc, kitctx.NopLogger()), userMessages())
		if err != nil {
			t.Fatalf("不得报错：%v", err)
		}
		if len(out.Steps) != 1 || out.Steps[0].Status != kitctx.StepStatusDone {
			t.Fatalf("StepTimeout<=0 ⇒ 不设独立超时（框架不得补默认值），实际 %+v", out.Steps)
		}
		if len(out.Gaps) != 0 {
			t.Fatalf("无失败步骤 ⇒ 无缺口，实际 %v", out.Gaps)
		}
	})
}

// ----------  / ：降级可识别、跳过 verdict、不阻塞 ----------

func TestLoop_ModelUnavailableDegrades(t *testing.T) {
	unavailable := errors.New("dial tcp 10.0.0.1:8081: connect: connection refused")

	cases := []struct {
		name          string
		steps         []doerStep
		mutate        func(*model.ClientConfig)
		noClient      bool // 不注入出域客户端
		emptyEndpoint bool // 端点未配置（零配置启动路径）
		wantErr       error
		wantReason    string
	}{
		{
			name:       "传输层不可用",
			steps:      []doerStep{{err: unavailable}},
			wantErr:    model.ErrModelUnavailable,
			wantReason: "model.unavailable",
		},
		{
			name:       "模型超时",
			steps:      []doerStep{{hang: true}},
			mutate:     func(c *model.ClientConfig) { c.Timeout = 20 * time.Millisecond },
			wantErr:    model.ErrModelTimeout,
			wantReason: "model.timeout",
		},
		{
			name:       "模型返回非成功状态",
			steps:      []doerStep{{status: http.StatusBadGateway, body: `{"error":"bad gateway"}`}},
			wantErr:    model.ErrModelStatus,
			wantReason: "model.status",
		},
		{
			name:       "未注入出域客户端",
			noClient:   true,
			wantErr:    model.ErrClientNotConfigured,
			wantReason: "client.not_configured",
		},
		{
			name:          "未配置端点（零配置启动路径）",
			steps:         []doerStep{{body: `{}`}},
			emptyEndpoint: true,
			wantErr:       model.ErrEndpointEmpty,
			wantReason:    "endpoint.empty",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doer := &scriptDoer{steps: tc.steps}
			var client *model.Client
			if !tc.noClient {
				client = newLoopClient(doer, tc.mutate)
			}
			endpoint := testEndpoint
			if tc.emptyEndpoint {
				endpoint = ""
			}
			loop := NewLoop(LoopConfig{Registry: testRegistry(t, okTool("alpha", "A")), Client: client, Endpoint: endpoint})
			logger := &captureLogger{}

			out, err := loop.Run(loopCtx(loopRequest(testTraceID, testRole), logger), userMessages())
			if err != nil {
				t.Fatalf("降级不得返回 error（请求整体不阻塞），实际 %v", err)
			}
			if !out.Degraded {
				t.Fatal("模型不可用/未配置必须置 degraded=true（契约点/5）")
			}
			if out.Notice != NoticeModelUnavailable {
				t.Fatalf("文案必须逐字为 %q，实际 %q", NoticeModelUnavailable, out.Notice)
			}
			if out.Verdict != "" {
				t.Fatalf("降级必须跳过 verdict（不产生模型段），实际 %q", out.Verdict)
			}
			if !errors.Is(out.Err, tc.wantErr) {
				t.Fatalf("降级原因必须可识别为 %v，实际 %v", tc.wantErr, out.Err)
			}
			entry, ok := logger.find("llm.error")
			if !ok {
				t.Fatal("降级必须落 LOG:llm.error")
			}
			if got := entry.fields["reason"]; got != tc.wantReason {
				t.Fatalf("日志 reason 应为稳定 token %q，实际 %v", tc.wantReason, got)
			}
			if logger.count("llm.endpoint.rejected") != 0 {
				t.Fatal("模型侧失败不得记成端点拒绝")
			}
		})
	}

	// 工具执行后模型才不可用 ⇒ 已发生的轨迹必须保留（不因降级丢证据）。
	t.Run("工具执行后模型不可用仍保留轨迹", func(t *testing.T) {
		doer := &scriptDoer{steps: []doerStep{
			{body: toolCallBody(t, call("alpha", `{}`))},
			{err: unavailable},
		}}
		loop := NewLoop(LoopConfig{
			Registry: testRegistry(t, okTool("alpha", "A")),
			Client:   newLoopClient(doer, nil),
			Endpoint: testEndpoint,
		})
		out, err := loop.Run(loopCtx(loopRequest(testTraceID, testRole), kitctx.NopLogger()), userMessages())
		if err != nil {
			t.Fatalf("降级不得返回 error：%v", err)
		}
		if !out.Degraded || len(out.Steps) != 1 || out.Steps[0].Name != "alpha" {
			t.Fatalf("降级时应保留已执行步骤，实际 degraded=%v steps=%v", out.Degraded, stepNames(out.Steps))
		}
	})
}

// ---------- ：出域策略拒绝不伪装成模型不可用 ----------

func TestLoop_EgressRefusalNotDisguisedAsDegraded(t *testing.T) {
	doer := &scriptDoer{steps: []doerStep{{body: textBody(t, "不该到达")}}}
	loop := NewLoop(LoopConfig{
		Registry: testRegistry(t, okTool("alpha", "A")),
		Client:   newLoopClient(doer, nil),
		Endpoint: "http://8.8.8.8/v1/chat/completions", // 公网 IP 字面量 ⇒ 加载期即拒
	})
	logger := &captureLogger{}

	out, err := loop.Run(loopCtx(loopRequest(testTraceID, testRole), logger), userMessages())
	if err == nil {
		t.Fatal("端点落在公网必须中止循环（fail-closed）")
	}
	if !errors.Is(err, model.ErrEndpointPublic) {
		t.Fatalf("必须原样上抛 任务 哨兵 ErrEndpointPublic，实际 %v", err)
	}
	if out.Degraded || out.Notice != "" {
		t.Fatalf("出域拒绝不是模型降级：degraded=%v notice=%q", out.Degraded, out.Notice)
	}
	if doer.count() != 0 {
		t.Fatalf("策略拒绝必须零出域，实际出域 %d 次", doer.count())
	}
	entry, ok := logger.find("llm.endpoint.rejected")
	if !ok {
		t.Fatal("出域拒绝必须记 LOG:llm.endpoint.rejected")
	}
	if got := entry.fields["reason"]; got != "endpoint.public" {
		t.Fatalf("reason 应为稳定 token endpoint.public，实际 %v", got)
	}
	if logger.count("llm.error") != 0 {
		t.Fatal("出域拒绝不得记成 llm.error（不得伪装成模型不可用）")
	}
}

// ---------- ：未注册工具被拒绝并记录 ----------

func TestLoop_UnregisteredToolRejected(t *testing.T) {
	doer := &scriptDoer{steps: []doerStep{
		{body: toolCallBody(t, call("ghost", `{}`), call("alpha", `{}`))},
		{body: textBody(t, "已按可用工具收口")},
	}}
	loop := NewLoop(LoopConfig{
		Registry: testRegistry(t, okTool("alpha", "A")),
		Client:   newLoopClient(doer, nil),
		Endpoint: testEndpoint,
	})
	logger := &captureLogger{}

	out, err := loop.Run(loopCtx(loopRequest(testTraceID, testRole), logger), userMessages())
	if err != nil {
		t.Fatalf("未注册工具只判该步失败，不得报错：%v", err)
	}
	if got := stepNames(out.Steps); !reflect.DeepEqual(got, []string{"ghost", "alpha"}) {
		t.Fatalf("被拒绝的步骤也必须按调用序记录，实际 %v", got)
	}
	if out.Steps[0].Status != kitctx.StepStatusFailed || out.Steps[1].Status != kitctx.StepStatusDone {
		t.Fatalf("状态应为 [failed, done]，实际 %v", []kitctx.StepStatus{out.Steps[0].Status, out.Steps[1].Status})
	}
	if !reflect.DeepEqual(out.Rejected, []string{"ghost"}) {
		t.Fatalf("被拒绝的工具名必须可机器读取，实际 %v", out.Rejected)
	}
	if len(out.Gaps) != 0 {
		t.Fatalf("被拒绝的步骤「记录」而非并入缺口（契约点 vs 契约点），实际 %v", out.Gaps)
	}
	entry, ok := logger.find("engine.tool.rejected")
	if !ok {
		t.Fatal("未注册工具必须留痕（LOG:engine.tool.rejected）")
	}
	if entry.fields["tool"] != "ghost" || entry.fields["rejected"] != true {
		t.Fatalf("拒绝日志字段不符：%v", entry.fields)
	}

	// 空工具名同样被拒绝且不得丢步（Trace 拒绝无名步骤 ⇒ 用占位名记录，不静默丢弃）。
	t.Run("空工具名同样被拒绝且不丢步", func(t *testing.T) {
		doer := &scriptDoer{steps: []doerStep{
			{body: toolCallBody(t, call("", `{}`))},
			{body: textBody(t, "收口")},
		}}
		loop := NewLoop(LoopConfig{
			Registry: testRegistry(t, okTool("alpha", "A")),
			Client:   newLoopClient(doer, nil),
			Endpoint: testEndpoint,
		})
		out, err := loop.Run(loopCtx(loopRequest(testTraceID, testRole), kitctx.NopLogger()), userMessages())
		if err != nil {
			t.Fatalf("不得报错：%v", err)
		}
		if len(out.Steps) != 1 || out.Steps[0].Name != unnamedTool {
			t.Fatalf("空名调用必须被记录（占位名 %q），实际 %v", unnamedTool, stepNames(out.Steps))
		}
		if out.Steps[0].Status != kitctx.StepStatusFailed {
			t.Fatalf("空工具名必须判失败，实际 %q", out.Steps[0].Status)
		}
	})
}

// ----------  /  执行侧：模型自发点名「已注册但无权」的工具 ⇒ 拒绝 ----------

// TestLoop_RegisteredButUnauthorizedToolRejected 覆盖「可见性裁剪**不是**唯一防线」的编排面：
// `query_log` 是 admin 链路工具（`RequiredRoles=["admin"]`），非 admin 会话里它**根本不在**
// 发往模型的可见集里；但模型可能自发输出该名字（幻觉 / prompt 注入诱导）⇒
// 该步必须被拒绝、执行体一次都不进、轨迹与日志要能把它与「未注册」区分开。
func TestLoop_RegisteredButUnauthorizedToolRejected(t *testing.T) {
	var adminRuns int32
	adminChain := tools.Tool{
		Name:          "query_log",
		Params:        map[string]any{"type": "object"},
		Summary:       "admin 链路 query_log",
		RequiredRoles: []string{"admin"},
		Run: func(context.Context, map[string]any) (any, error) {
			atomic.AddInt32(&adminRuns, 1)
			return "admin-only-data", nil
		},
	}
	doer := &scriptDoer{steps: []doerStep{
		{body: toolCallBody(t, call("query_log", `{"question":"昨天有哪些 500"}`))},
		{body: textBody(t, "已按可用工具收口")},
	}}
	loop := NewLoop(LoopConfig{
		Registry: testRegistry(t, okTool("alpha", "A"), adminChain),
		Client:   newLoopClient(doer, nil),
		Endpoint: testEndpoint,
	})
	logger := &captureLogger{}

	out, err := loop.Run(loopCtx(loopRequest(testTraceID, testRole), logger), userMessages())
	if err != nil {
		t.Fatalf("越权调用只判该步失败，不得中止循环：%v", err)
	}
	// ① 进模型前已裁剪：admin 工具不在可见集里。
	sent := doer.calls()
	if len(sent) == 0 {
		t.Fatal("本轮一次出域都没有发生，无法核对发往模型的工具声明")
	}
	if got := toolNames(sent[0].Tools); !reflect.DeepEqual(got, []string{"alpha"}) {
		t.Fatalf("非 admin 会话的可见工具集 = %v，期望 [alpha]", got)
	}
	// ② 即便模型自发点名，执行侧也必须拒绝，且执行体一次都不进。
	if got := atomic.LoadInt32(&adminRuns); got != 0 {
		t.Fatalf("越权工具执行体被调用 %d 次，期望 0（执行侧不得角色盲）", got)
	}
	if got := stepNames(out.Steps); !reflect.DeepEqual(got, []string{"query_log"}) {
		t.Fatalf("被拒绝的步骤也必须按调用序记录，实际 %v", got)
	}
	if out.Steps[0].Status != kitctx.StepStatusFailed {
		t.Fatalf("越权步骤状态 = %q，期望 failed", out.Steps[0].Status)
	}
	if !reflect.DeepEqual(out.Rejected, []string{"query_log"}) {
		t.Fatalf("被拒绝的工具名必须可机器读取，实际 %v", out.Rejected)
	}
	if len(out.Gaps) != 0 {
		t.Fatalf("被拒绝的步骤「记录」而非并入缺口，实际 %v", out.Gaps)
	}
	entry, ok := logger.find("engine.tool.rejected")
	if !ok {
		t.Fatal("越权调用必须留痕（LOG:engine.tool.rejected）")
	}
	if entry.fields["tool"] != "query_log" || entry.fields["rejected"] != true {
		t.Fatalf("拒绝日志字段不符：%v", entry.fields)
	}
	if got := entry.fields["reason"]; got != "tool.forbidden" {
		t.Fatalf("越权拒绝的日志 reason = %v，期望 tool.forbidden（与未注册可区分）", got)
	}
}

// ---------- ：单步失败记缺口、整体继续 ----------

func TestLoop_FailedStepBecomesGapAndContinues(t *testing.T) {
	bad := testTool("bad", func(context.Context, map[string]any) (any, error) {
		return nil, errors.New("下游 500")
	})
	doer := &scriptDoer{steps: []doerStep{
		{body: toolCallBody(t, call("bad", `{}`), call("good", `{}`))},
		{body: textBody(t, "继续完成")},
	}}
	loop := NewLoop(LoopConfig{
		Registry: testRegistry(t, bad, okTool("good", "G")),
		Client:   newLoopClient(doer, nil),
		Endpoint: testEndpoint,
	})

	out, err := loop.Run(loopCtx(loopRequest(testTraceID, testRole), kitctx.NopLogger()), userMessages())
	if err != nil {
		t.Fatalf("单步失败不得中止整轮：%v", err)
	}
	if got := stepNames(out.Steps); !reflect.DeepEqual(got, []string{"bad", "good"}) {
		t.Fatalf("失败步骤与其后步骤都要记录，实际 %v", got)
	}
	if out.Steps[0].Status != kitctx.StepStatusFailed || out.Steps[1].Status != kitctx.StepStatusDone {
		t.Fatal("一步失败不得影响同批其它步骤")
	}
	if !reflect.DeepEqual(out.Gaps, []GapEntry{{Source: "bad", Reason: reasonToolFailed}}) {
		t.Fatalf("失败步骤必须显式列缺口 {source:bad, reason:%s}，实际 %v", reasonToolFailed, out.Gaps)
	}
	if out.Verdict != "继续完成" {
		t.Fatalf("整体继续直到模型收口，实际 verdict=%q", out.Verdict)
	}
	// 失败结果回填给模型时必须显式标注失败 —— 不得让模型以为该步成功了。
	toolMsg := toolResultMessage(t, doer.calls()[1], "call-bad")
	if !strings.Contains(toolMsg.Content, `"status":"failed"`) {
		t.Fatalf("失败步骤回填必须带 status=failed，实际 %q", toolMsg.Content)
	}
	if strings.Contains(toolMsg.Content, "下游 500") {
		t.Fatalf("回填内容不得含下游错误原文（只落稳定 token），实际 %q", toolMsg.Content)
	}

	// 执行体 panic 只判该步失败，不拖垮循环（ 的隔离在编排侧同样成立）。
	t.Run("执行体 panic 只判该步失败", func(t *testing.T) {
		boom := testTool("boom", func(context.Context, map[string]any) (any, error) { panic("执行体炸了") })
		panicDoer := &scriptDoer{steps: []doerStep{
			{body: toolCallBody(t, call("boom", `{}`))},
			{body: textBody(t, "已隔离")},
		}}
		panicLoop := NewLoop(LoopConfig{
			Registry: testRegistry(t, boom),
			Client:   newLoopClient(panicDoer, nil),
			Endpoint: testEndpoint,
		})
		panicOut, panicErr := panicLoop.Run(loopCtx(loopRequest(testTraceID, testRole), kitctx.NopLogger()), userMessages())
		if panicErr != nil {
			t.Fatalf("panic 不得中止循环：%v", panicErr)
		}
		if len(panicOut.Steps) != 1 || panicOut.Steps[0].Status != kitctx.StepStatusFailed {
			t.Fatalf("panic 步骤必须记为 failed，实际 %+v", panicOut.Steps)
		}
		if !reflect.DeepEqual(panicOut.Gaps, []GapEntry{{Source: "boom", Reason: reasonToolPanic}}) {
			t.Fatalf("panic 必须归类为 %s，实际 %v", reasonToolPanic, panicOut.Gaps)
		}
		if panicOut.Verdict != "已隔离" {
			t.Fatalf("panic 后必须继续到收口，实际 verdict=%q", panicOut.Verdict)
		}
	})
}

// 参数摘要按「形态」归纳（不落取值）；坏参数与「送不出去的结果」都必须判失败并记缺口。
func TestLoop_TraceSummariesShapeAndUnserializable(t *testing.T) {
	unserializable := testTool("chan", func(context.Context, map[string]any) (any, error) {
		return make(chan int), nil // 执行体「成功」返回一个无法 JSON 序列化的值
	})
	doer := &scriptDoer{steps: []doerStep{
		{body: toolCallBody(t,
			callRaw("c1", "alpha", `{"s":"abc","n":1,"b":true,"z":null,"a":[1,2],"o":{"k":1},"f":1.5}`),
			callRaw("c2", "alpha", `{not json`),
			callRaw("c3", "chan", `{}`),
		)},
		{body: textBody(t, "收口")},
	}}
	loop := NewLoop(LoopConfig{
		Registry: testRegistry(t, okTool("alpha", "A"), unserializable),
		Client:   newLoopClient(doer, nil),
		Endpoint: testEndpoint,
	})

	out, err := loop.Run(loopCtx(loopRequest(testTraceID, testRole), kitctx.NopLogger()), userMessages())
	if err != nil {
		t.Fatalf("不得报错：%v", err)
	}
	if len(out.Steps) != 3 {
		t.Fatalf("三步都应被记录，实际 %d 步", len(out.Steps))
	}
	for _, want := range []string{`"string:3"`, `"number"`, `"bool"`, `"null"`, `"array:2"`, `"object:1"`, `"sha256"`} {
		if !strings.Contains(out.Steps[0].Params, want) {
			t.Fatalf("参数摘要应含形态 %s，实际 %q", want, out.Steps[0].Params)
		}
	}
	if out.Steps[1].Params != `{"invalid":true}` || out.Steps[1].Status != kitctx.StepStatusFailed {
		t.Fatalf("坏参数必须判失败且摘要标 invalid，实际 status=%q params=%q", out.Steps[1].Status, out.Steps[1].Params)
	}
	if out.Steps[2].Status != kitctx.StepStatusFailed {
		t.Fatalf("无法序列化的结果不得呈报 done，实际 %q", out.Steps[2].Status)
	}
	if !reflect.DeepEqual(out.Gaps, []GapEntry{
		{Source: "alpha", Reason: reasonToolFailed},
		{Source: "chan", Reason: reasonToolUnserializable},
	}) {
		t.Fatalf("两步失败都应显式记缺口，实际 %v", out.Gaps)
	}
	if out.Verdict != "收口" {
		t.Fatalf("失败步骤不得中止整体，实际 verdict=%q", out.Verdict)
	}
}

// ---------- ：全序列落轨迹 + 可按 traceid 回放 + 不落原文 ----------

func TestLoop_TraceReplayByTraceID(t *testing.T) {
	reg := testRegistry(t,
		okTool("alpha", secretValue),
		okTool("beta", "B"),
	)
	doer := &scriptDoer{steps: []doerStep{
		{body: toolCallBody(t, callRaw("c1", "alpha", `{"ip":"`+secretArg+`"}`))},
		{body: toolCallBody(t, callRaw("c2", "beta", `{}`))},
		{body: textBody(t, "完成")},
	}}
	loop := NewLoop(LoopConfig{Registry: reg, Client: newLoopClient(doer, nil), Endpoint: testEndpoint})
	logger := &captureLogger{}

	out, err := loop.Run(loopCtx(loopRequest(testTraceID, testRole), logger), userMessages())
	if err != nil {
		t.Fatalf("不得报错：%v", err)
	}
	if len(out.Steps) != 2 {
		t.Fatalf("应记录 2 步，实际 %d", len(out.Steps))
	}

	// 回放：只靠 traceid 即可重建完整序列。
	replayed, ok := kitctx.Replay(out.Steps, testTraceID)
	if !ok {
		t.Fatalf("同一 traceid 必须可回放（契约点）")
	}
	if !reflect.DeepEqual(replayed, out.Steps) {
		t.Fatalf("回放序列必须与原序列逐位一致\n got=%+v\nwant=%+v", replayed, out.Steps)
	}
	if _, ok := kitctx.Replay(out.Steps, "trace-other"); ok {
		t.Fatal("其它 traceid 不得回放成功")
	}

	// 参数/结果摘要：只落哈希与形态，不落任何取值原文。
	for i, step := range out.Steps {
		if strings.Contains(step.Params, secretArg) || strings.Contains(step.ResultSummary, secretValue) {
			t.Fatalf("steps[%d] 落入了原文：params=%q result=%q", i, step.Params, step.ResultSummary)
		}
		if !strings.Contains(step.Params, `"sha256"`) {
			t.Fatalf("参数摘要必须是哈希 + 形态，实际 %q", step.Params)
		}
		if !strings.Contains(step.ResultSummary, `"sha256"`) {
			t.Fatalf("结果摘要必须是哈希 + 形态，实际 %q", step.ResultSummary)
		}
		// 长度也要钉住：摘要口径是全框架共享的（`kitctx.Digest`），
		// 只查"有没有 sha256 字段"就等于没查——改成长一点短一点都不会红（P7b 的  探针实测逃过一次）。
		hashFieldIs(t, step.Params, 12, "参数")
		hashFieldIs(t, step.ResultSummary, 12, "结果")
	}

	// 落日志：全序列一条一条进 LOG:engine.trail（可按 traceid 过滤重建）。
	if got := logger.count("engine.trail"); got != 2 {
		t.Fatalf("轨迹应逐条落日志，实际 %d 条", got)
	}
	trail, _ := logger.find("engine.trail")
	if trail.fields["trace_id"] != testTraceID || trail.fields["seq"] != 1 {
		t.Fatalf("轨迹日志字段不符：%v", trail.fields)
	}
}

// L-026 裁决 1：并行之下 Seq 仍由**调用位置**决定（与完成序无关）。
// 栅栏：alpha 等 beta 关闭 bDone 才返回 ⇒ 完成序必然是 beta→alpha；而 Seq 必须 alpha=1、beta=2。
func TestLoop_TraceSeqDeterministicUnderParallel(t *testing.T) {
	bDone := make(chan struct{})
	var mu sync.Mutex
	var finished []string

	reg := testRegistry(t,
		testTool("alpha", func(ctx context.Context, _ map[string]any) (any, error) {
			select {
			case <-bDone:
			case <-ctx.Done():
			}
			mu.Lock()
			finished = append(finished, "alpha")
			mu.Unlock()
			return "A", nil
		}),
		testTool("beta", func(context.Context, map[string]any) (any, error) {
			mu.Lock()
			finished = append(finished, "beta")
			mu.Unlock()
			close(bDone)
			return "B", nil
		}),
	)
	doer := &scriptDoer{steps: []doerStep{
		{body: toolCallBody(t, call("alpha", `{}`), call("beta", `{}`))},
		{body: textBody(t, "完成")},
	}}
	loop := NewLoop(LoopConfig{Registry: reg, Client: newLoopClient(doer, nil), Endpoint: testEndpoint})
	rc := withLimits(loopRequest(testTraceID, testRole), kitctx.Limits{MaxSteps: 4})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := loop.Run(loopCtxWith(ctx, rc, kitctx.NopLogger()), userMessages())
	if err != nil {
		t.Fatalf("不得报错：%v", err)
	}

	mu.Lock()
	got := append([]string(nil), finished...)
	mu.Unlock()
	if !reflect.DeepEqual(got, []string{"beta", "alpha"}) {
		t.Fatalf("完成序应为 beta→alpha（证明并行且与调用序不同），实际 %v", got)
	}
	if names := stepNames(out.Steps); !reflect.DeepEqual(names, []string{"alpha", "beta"}) {
		t.Fatalf("Seq 必须按调用序（与完成序无关），实际 %v", names)
	}
	if out.Steps[0].Seq != 1 || out.Steps[1].Seq != 2 {
		t.Fatalf("Seq 应为 1,2，实际 %d,%d", out.Steps[0].Seq, out.Steps[1].Seq)
	}
}

// ----------  第三侧 +  前提：不可判定即拒绝，且零出域 ----------

func TestLoop_FailClosedWithoutRoleOrTraceID(t *testing.T) {
	doer := &scriptDoer{steps: []doerStep{{body: textBody(t, "不该到达")}}}
	loop := NewLoop(LoopConfig{
		Registry: testRegistry(t, okTool("alpha", "A")),
		Client:   newLoopClient(doer, nil),
		Endpoint: testEndpoint,
	})

	t.Run("未挂载请求上下文", func(t *testing.T) {
		out, err := loop.Run(context.Background(), userMessages())
		if !errors.Is(err, ErrRoleUnresolved) {
			t.Fatalf("必须 fail-closed 拒绝（ErrRoleUnresolved），实际 %v", err)
		}
		if out.Degraded {
			t.Fatal("角色不可判定不是模型降级")
		}
	})

	t.Run("角色为空", func(t *testing.T) {
		out, err := loop.Run(loopCtx(loopRequest(testTraceID, ""), kitctx.NopLogger()), userMessages())
		if !errors.Is(err, ErrRoleUnresolved) {
			t.Fatalf("必须 fail-closed 拒绝（ErrRoleUnresolved），实际 %v", err)
		}
		if out.Degraded {
			t.Fatal("角色不可判定不是模型降级")
		}
	})

	t.Run("缺 traceid", func(t *testing.T) {
		out, err := loop.Run(loopCtx(loopRequest("", testRole), kitctx.NopLogger()), userMessages())
		if !errors.Is(err, ErrTraceIDRequired) {
			t.Fatalf("缺 traceid 时轨迹不可回放 ⇒ 必须拒绝，实际 %v", err)
		}
		if out.Degraded {
			t.Fatal("缺 traceid 不是模型降级")
		}
	})

	if got := doer.count(); got != 0 {
		t.Fatalf("三种 fail-closed 情形都必须零出域，实际出域 %d 次", got)
	}
}

// ---------- 并发：共享同一个 Loop，每次 Run 的轨迹互不串号 ----------

func TestLoop_ConcurrentRunsIsolatedTraces(t *testing.T) {
	doer := &roundDoer{}
	loop := NewLoop(LoopConfig{
		Registry: testRegistry(t, okTool("alpha", "A")),
		Client:   newLoopClient(doer, nil),
		Endpoint: testEndpoint,
	})

	const runs = 8
	outs := make([]LoopOutcome, runs)
	errs := make([]error, runs)
	var wg sync.WaitGroup
	for i := 0; i < runs; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rc := loopRequest("trace-concurrent-"+strconv.Itoa(i), testRole)
			outs[i], errs[i] = loop.Run(loopCtx(rc, kitctx.NopLogger()), userMessages())
		}(i)
	}
	wg.Wait()

	for i := 0; i < runs; i++ {
		if errs[i] != nil {
			t.Fatalf("run %d 不得报错：%v", i, errs[i])
		}
		wantTrace := "trace-concurrent-" + strconv.Itoa(i)
		if outs[i].Verdict != "并发完成" {
			t.Fatalf("run %d verdict=%q", i, outs[i].Verdict)
		}
		if len(outs[i].Steps) != 2 {
			t.Fatalf("run %d 应有 2 步（模型点名的两个），实际 %d", i, len(outs[i].Steps))
		}
		for j, step := range outs[i].Steps {
			if step.TraceID != wantTrace {
				t.Fatalf("轨迹串号：run %d steps[%d].TraceID=%q，应为 %q", i, j, step.TraceID, wantTrace)
			}
			if step.Seq != j+1 {
				t.Fatalf("run %d steps[%d].Seq=%d，应为 %d", i, j, step.Seq, j+1)
			}
		}
	}
	if got := doer.count(); got != int64(2*runs) {
		t.Fatalf("共享客户端应被调用 %d 次，实际 %d 次", 2*runs, got)
	}
}

// hashFieldIs 断言 JSON 摘要里 "sha256" 的值恰为 want 位十六进制。
func hashFieldIs(t *testing.T, raw string, want int, what string) {
	t.Helper()
	const key = `"sha256":"`
	i := strings.Index(raw, key)
	if i < 0 {
		t.Fatalf("%s摘要里没有 sha256 字段：%q", what, raw)
	}
	rest := raw[i+len(key):]
	j := strings.IndexByte(rest, '"')
	if j < 0 {
		t.Fatalf("%s摘要的 sha256 字段没有收尾：%q", what, raw)
	}
	got := rest[:j]
	if len(got) != want {
		t.Fatalf("%s摘要长度应为 %d，实得 %d（%q）⇒ 共享摘要口径漂移", what, want, len(got), got)
	}
	if strings.TrimRight(got, "0123456789abcdef") != "" {
		t.Fatalf("%s摘要不是十六进制：%q", what, got)
	}
}
