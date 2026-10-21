package tools

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/malizhi2202-dev/agentkit/kitctx"
	"github.com/malizhi2202-dev/agentkit/model"
)

// ---------- 测试替身 ----------

// captureLogger 记录注入日志，用于断言  的「被拒绝并记录」。
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

func (l *captureLogger) find(msg string) (logEntry, bool) {
	for _, e := range l.all() {
		if e.msg == msg {
			return e, true
		}
	}
	return logEntry{}, false
}

// fixedClock 是固定时刻的注入时钟：用于证明「耗时取自注入 Clock」而非直接 time.Now。
type fixedClock struct{ at time.Time }

func (c fixedClock) Now() time.Time { return c.at }

// ---------- 构造辅助 ----------

func simpleTool(name string, run RunFunc) Tool {
	return Tool{Name: name, Params: stringParamSchema(), Summary: "测试工具 " + name, Run: run}
}

func newTestRegistry(t *testing.T, list ...Tool) *Registry {
	t.Helper()
	reg, err := NewRegistry(list...)
	if err != nil {
		t.Fatalf("构造注册表失败：%v", err)
	}
	return reg
}

func callOf(name, arguments string) model.ToolCall {
	return model.ToolCall{
		ID:       "call-" + name,
		Type:     "function",
		Function: model.ToolFunction{Name: name, Arguments: arguments},
	}
}

// runnerRoleAnalyst 是本包用例使用的「非 admin」角色。
//
// 本包不得依赖 宿主侧 业务实现（宿主侧目录），故用字面量而非 `宿主既有实现` 的枚举；
// 取值与 宿主侧 的普通分析角色同名，便于与 `既有实现/tools` 的用例对照。
const runnerRoleAnalyst = "analyst"

func requestCtx(stepTimeout time.Duration, logger kitctx.Logger, clock kitctx.Clock) context.Context {
	rc := kitctx.RequestContext{
		TraceID:    "trace-runner-test",
		Role:       kitctx.Role(runnerRoleAnalyst),
		Limits:     kitctx.Limits{StepTimeout: stepTimeout},
		Injections: kitctx.Injections{Logger: logger, Clock: clock},
	}
	return kitctx.WithRequest(context.Background(), rc)
}

// roleCtx 返回「角色已判定、不注入 Logger/Clock」的请求上下文：
// 用于验证未注入注入点时的回落路径（系统时钟 / NopLogger），同时保证角色可判定；
// 也用于「换一个角色再跑同一批调用」的越权用例。
func roleCtx(role string) context.Context {
	return kitctx.WithRequest(context.Background(), kitctx.RequestContext{
		TraceID: "trace-runner-test",
		Role:    kitctx.Role(role),
	})
}

func okRun(value string) RunFunc {
	return func(ctx context.Context, args map[string]any) (any, error) { return value, nil }
}

func resultOf(t *testing.T, results []Result, name string) Result {
	t.Helper()
	for _, r := range results {
		if r.Call.Function.Name == name {
			return r
		}
	}
	t.Fatalf("结果里缺少工具 %q 的记录（共 %d 条）", name, len(results))
	return Result{}
}

// parseGoStmts 解析一份 Go 源码，返回其中的 `go` 语句数量与 import 集合。
func parseGoStmts(src []byte) (int, map[string]bool, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "src.go", src, 0)
	if err != nil {
		return 0, nil, err
	}
	imports := make(map[string]bool, len(file.Imports))
	for _, imp := range file.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			return 0, nil, err
		}
		imports[path] = true
	}
	count := 0
	ast.Inspect(file, func(n ast.Node) bool {
		if _, ok := n.(*ast.GoStmt); ok {
			count++
		}
		return true
	})
	return count, imports, nil
}

// ---------- ：errgroup 并行调度 ----------

// TestRunner_ParallelDispatch 用「栅栏」证明同批调用真的并发进入执行体：
// 每个执行体先报告已进入、再等全部进入后才放行。若调度是串行的，第一个执行体会一直等，
// 本用例以超时报错（而不是依赖墙钟耗时做概率断言）。
func TestRunner_ParallelDispatch(t *testing.T) {
	const n = 4
	entered := make(chan struct{}, n)
	release := make(chan struct{})
	run := func(ctx context.Context, args map[string]any) (any, error) {
		entered <- struct{}{}
		select {
		case <-release:
			return "ok", nil
		case <-time.After(3 * time.Second):
			return nil, errors.New("同批其它调用未并行进入执行体")
		}
	}
	list := make([]Tool, 0, n)
	calls := make([]model.ToolCall, 0, n)
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("tool%d", i)
		list = append(list, simpleTool(name, run))
		calls = append(calls, callOf(name, "{}"))
	}
	reg := newTestRegistry(t, list...)

	done := make(chan []Result, 1)
	go func() {
		done <- NewRunner(reg).Run(requestCtx(0, kitctx.NopLogger(), nil), calls)
	}()

	for i := 0; i < n; i++ {
		select {
		case <-entered:
		case <-time.After(3 * time.Second):
			t.Fatalf("仅有 %d/%d 个调用进入执行体 ⇒ 未并行调度（契约点）", i, n)
		}
	}
	close(release)

	results := <-done
	if len(results) != n {
		t.Fatalf("结果条数 = %d，期望 %d", len(results), n)
	}
	for _, r := range results {
		if r.Status != kitctx.StepStatusDone {
			t.Errorf("%s 状态 = %q，期望 done（err=%v）", r.Call.Function.Name, r.Status, r.Err)
		}
	}
}

// ---------- ：未注册工具 ⇒ 拒绝并记录，不静默忽略 ----------

func TestRunner_UnregisteredToolRejected(t *testing.T) {
	var invoked int32
	logger := &captureLogger{}
	reg := newTestRegistry(t, simpleTool("user.profile", func(ctx context.Context, args map[string]any) (any, error) {
		atomic.AddInt32(&invoked, 1)
		return "profile", nil
	}))

	results := NewRunner(reg).Run(requestCtx(0, logger, nil), []model.ToolCall{
		callOf("user.profile", `{"value":"1.2.3.4"}`),
		callOf("ghost.tool", "{}"),
		callOf("", "{}"),
	})

	registered := resultOf(t, results, "user.profile")
	if registered.Status != kitctx.StepStatusDone {
		t.Errorf("已注册工具状态 = %q，期望 done（err=%v）", registered.Status, registered.Err)
	}
	if got := atomic.LoadInt32(&invoked); got != 1 {
		t.Errorf("已注册工具执行次数 = %d，期望 1", got)
	}

	for _, name := range []string{"ghost.tool", ""} {
		bad := resultOf(t, results, name)
		if !bad.Rejected {
			t.Errorf("%q 未被标记 rejected", name)
		}
		if bad.Status != kitctx.StepStatusFailed {
			t.Errorf("%q 状态 = %q，期望 failed（契约点）", name, bad.Status)
		}
		if !errors.Is(bad.Err, ErrToolNotRegistered) {
			t.Errorf("%q 错误 = %v，期望 errors.Is ErrToolNotRegistered", name, bad.Err)
		}
		if bad.Value != nil {
			t.Errorf("%q 未被拒绝：仍返回 Value=%v（未注册工具不得被执行或被静默忽略）", name, bad.Value)
		}
	}

	entry, found := logger.find("engine.tool.rejected")
	if !found {
		t.Fatalf("未记录拒绝事件（契约点 要求「被拒绝并记录」）：日志=%v", logger.all())
	}
	if entry.fields["rejected"] != true {
		t.Errorf("拒绝日志字段 rejected = %v，期望 true", entry.fields["rejected"])
	}
	if entry.fields["tool"] != "ghost.tool" {
		t.Errorf("拒绝日志字段 tool = %v，期望 ghost.tool", entry.fields["tool"])
	}
	if entry.fields["reason"] != "tool.not_registered" {
		t.Errorf("拒绝日志字段 reason = %v，期望 tool.not_registered", entry.fields["reason"])
	}
}

// TestRunner_RegisteredButUnauthorizedRejected 钉住「可见性裁剪**不是**唯一防线」：
// 模型**自发**输出（幻觉 / prompt 注入诱导）的无权工具名，即便根本不在 `VisibleByRole`
// 的裁剪结果里，执行侧也必须拒绝，且**绝不进入执行体**（用计数 + 副作用断言，不是只断字段）。
//
// 负例用的是本 change 的 **admin 三链路真实工具名**，
// 调用者角色是非 admin 的 `runnerRoleAnalyst`。
func TestRunner_RegisteredButUnauthorizedRejected(t *testing.T) {
	var adminRuns, researchRuns int32
	list := []Tool{simpleTool("correlate", func(context.Context, map[string]any) (any, error) {
		atomic.AddInt32(&researchRuns, 1)
		return "correlated", nil
	})}
	for _, name := range adminToolNames {
		toolName := name
		list = append(list, Tool{
			Name:          toolName,
			Params:        stringParamSchema(),
			Summary:       "admin 链路 " + toolName,
			RequiredRoles: []string{"admin"},
			Run: func(context.Context, map[string]any) (any, error) {
				atomic.AddInt32(&adminRuns, 1)
				return map[string]any{"tag": toolName}, nil
			},
		})
	}
	reg := newTestRegistry(t, list...)

	calls := []model.ToolCall{callOf("correlate", `{"value":"9.9.9.9"}`)}
	for _, name := range adminToolNames {
		calls = append(calls, callOf(name, `{"question":"谁负责 /x 接口"}`))
	}
	calls = append(calls, callOf("ghost.tool", "{}"))

	logger := &captureLogger{}
	results := NewRunner(reg).Run(requestCtx(0, logger, nil), calls) // requestCtx 的角色 = 非 admin

	// 正例：有权的入口命令工具照常执行（收紧授权不得误伤）。
	good := resultOf(t, results, "correlate")
	if good.Status != kitctx.StepStatusDone {
		t.Errorf("有权的工具被误拒：status=%q err=%v", good.Status, good.Err)
	}
	if got := atomic.LoadInt32(&researchRuns); got != 1 {
		t.Errorf("有权工具执行次数 = %d，期望 1", got)
	}

	// 负例：已注册但调用者角色无权 ⇒ 与「未注册」同样拒绝。
	for _, name := range adminToolNames {
		bad := resultOf(t, results, name)
		if !bad.Rejected {
			t.Errorf("%q 已注册但调用者角色无权，却未被标记 rejected（执行侧角色盲）", name)
		}
		if bad.Status != kitctx.StepStatusFailed {
			t.Errorf("%q 状态 = %q，期望 failed", name, bad.Status)
		}
		if bad.Value != nil {
			t.Errorf("%q 越权调用被执行并返回 Value=%v", name, bad.Value)
		}
		if !errors.Is(bad.Err, ErrToolForbidden) {
			t.Errorf("%q 错误 = %v，期望 errors.Is ErrToolForbidden", name, bad.Err)
		}
		if errors.Is(bad.Err, ErrToolNotRegistered) {
			t.Errorf("%q 被误判为「未注册」：错误 = %v（两类拒绝必须可区分）", name, bad.Err)
		}
	}
	if got := atomic.LoadInt32(&adminRuns); got != 0 {
		t.Errorf("admin 工具执行体被调用 %d 次，期望 0（越权调用不得进入执行体）", got)
	}
	if got := atomic.LoadInt32(&researchRuns); got != 1 {
		t.Errorf("同批调用被越权拒绝牵连：宿主入口词工具执行次数 = %d，期望 1（单步拒绝不得中止同批其它调用）", got)
	}

	// 「已注册但无权」与「未注册」必须在留痕上**可区分**：两个稳定 reason token 各自出现。
	reasons := map[string]string{}
	for _, entry := range logger.all() {
		if entry.msg != "engine.tool.rejected" {
			continue
		}
		if tool, ok := entry.fields["tool"].(string); ok {
			reason, _ := entry.fields["reason"].(string)
			reasons[tool] = reason
		}
	}
	if got := reasons[adminToolNames[0]]; got != "tool.forbidden" {
		t.Errorf("越权拒绝的日志 reason = %q，期望 tool.forbidden（与未注册可区分）", got)
	}
	if got := reasons["ghost.tool"]; got != "tool.not_registered" {
		t.Errorf("未注册拒绝的日志 reason = %q，期望 tool.not_registered", got)
	}
}

// ----------  / ：每步独立超时 ----------

func TestRunner_IndependentStepTimeout(t *testing.T) {
	t.Run("设置步超时：超时步失败、兄弟步不受影响", func(t *testing.T) {
		slow := func(ctx context.Context, args map[string]any) (any, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		fast := func(ctx context.Context, args map[string]any) (any, error) {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(5 * time.Millisecond):
				return "fast", nil
			}
		}
		reg := newTestRegistry(t, simpleTool("slow", slow), simpleTool("fast", fast))
		results := NewRunner(reg).Run(
			requestCtx(40*time.Millisecond, kitctx.NopLogger(), nil),
			[]model.ToolCall{callOf("slow", "{}"), callOf("fast", "{}")},
		)

		got := resultOf(t, results, "slow")
		if got.Status != kitctx.StepStatusFailed {
			t.Errorf("超时步状态 = %q，期望 failed", got.Status)
		}
		if !errors.Is(got.Err, ErrToolTimeout) {
			t.Errorf("超时步错误 = %v，期望 errors.Is ErrToolTimeout", got.Err)
		}
		if !errors.Is(got.Err, context.DeadlineExceeded) {
			t.Errorf("超时步错误 = %v，期望同时可 errors.Is context.DeadlineExceeded", got.Err)
		}
		if sib := resultOf(t, results, "fast"); sib.Status != kitctx.StepStatusDone {
			t.Errorf("兄弟步状态 = %q，期望 done（单步超时不得误伤同批其它调用）", sib.Status)
		}
	})

	t.Run("未设置步超时：框架不补默认值（L-015 契约 3）", func(t *testing.T) {
		reg := newTestRegistry(t, simpleTool("slowish", func(ctx context.Context, args map[string]any) (any, error) {
			time.Sleep(30 * time.Millisecond)
			return "ok", nil
		}))
		got := resultOf(t, NewRunner(reg).Run(
			requestCtx(0, kitctx.NopLogger(), nil), []model.ToolCall{callOf("slowish", "{}")}), "slowish")
		if got.Status != kitctx.StepStatusDone {
			t.Errorf("StepTimeout<=0 时被框架补了默认超时：状态 = %q（err=%v）", got.Status, got.Err)
		}
	})
}

// TestRunner_TimeoutNeverReportedAsSuccess 覆盖「工具无视 ctx 超时仍返回成功」的形态：
// 返回时已越过步超时 ⇒ 一律判失败，不得把超时当成功（TASK task 的硬约束）。
func TestRunner_TimeoutNeverReportedAsSuccess(t *testing.T) {
	stubborn := func(ctx context.Context, args map[string]any) (any, error) {
		time.Sleep(80 * time.Millisecond) // 无视 ctx：返回时已越过步超时
		return "stale-result", nil
	}
	reg := newTestRegistry(t, simpleTool("stubborn", stubborn))
	got := resultOf(t, NewRunner(reg).Run(
		requestCtx(20*time.Millisecond, kitctx.NopLogger(), nil),
		[]model.ToolCall{callOf("stubborn", "{}")}), "stubborn")

	if got.Status != kitctx.StepStatusFailed {
		t.Fatalf("超时步状态 = %q，期望 failed（不得把超时当成功）", got.Status)
	}
	if !errors.Is(got.Err, ErrToolTimeout) {
		t.Errorf("错误 = %v，期望 errors.Is ErrToolTimeout", got.Err)
	}
	if got.Value != nil {
		t.Errorf("超时步仍返回了 Value=%v（不得把超时结果当成功结果）", got.Value)
	}
}

// ---------- ：单步失败 ⇒ 整体继续 ----------

func TestRunner_SingleStepFailureContinues(t *testing.T) {
	var ran int32
	reg := newTestRegistry(t,
		simpleTool("bad", func(ctx context.Context, args map[string]any) (any, error) {
			return nil, errors.New("下游 5xx")
		}),
		simpleTool("good", func(ctx context.Context, args map[string]any) (any, error) {
			atomic.AddInt32(&ran, 1)
			return "good", nil
		}),
	)
	results := NewRunner(reg).Run(requestCtx(0, kitctx.NopLogger(), nil),
		[]model.ToolCall{callOf("bad", "{}"), callOf("good", "{}")})

	if len(results) != 2 {
		t.Fatalf("失败步导致整体中止：结果条数 = %d，期望 2", len(results))
	}
	bad := resultOf(t, results, "bad")
	if bad.Status != kitctx.StepStatusFailed || bad.Rejected {
		t.Errorf("失败步：status=%q rejected=%v，期望 failed + 非 rejected", bad.Status, bad.Rejected)
	}
	if bad.Err == nil || !strings.Contains(bad.Err.Error(), "下游 5xx") {
		t.Errorf("失败步未保留原始错误：%v", bad.Err)
	}
	if atomic.LoadInt32(&ran) != 1 {
		t.Errorf("失败步阻断了同批其它调用：good 执行次数 = %d，期望 1", ran)
	}
	if good := resultOf(t, results, "good"); good.Status != kitctx.StepStatusDone {
		t.Errorf("兄弟步状态 = %q，期望 done", good.Status)
	}
}

// TestRunner_PanicIsolatedToStep 证明执行体 panic 只会让该步失败，不拖垮整批、不拖垮进程。
func TestRunner_PanicIsolatedToStep(t *testing.T) {
	reg := newTestRegistry(t,
		simpleTool("boom", func(ctx context.Context, args map[string]any) (any, error) {
			panic("tool exploded")
		}),
		simpleTool("calm", okRun("calm")),
	)
	results := NewRunner(reg).Run(requestCtx(0, kitctx.NopLogger(), nil),
		[]model.ToolCall{callOf("boom", "{}"), callOf("calm", "{}")})

	got := resultOf(t, results, "boom")
	if got.Status != kitctx.StepStatusFailed || !errors.Is(got.Err, ErrToolPanic) {
		t.Errorf("panic 步：status=%q err=%v，期望 failed + errors.Is ErrToolPanic", got.Status, got.Err)
	}
	if got.Value != nil {
		t.Errorf("panic 步返回了 Value=%v", got.Value)
	}
	if calm := resultOf(t, results, "calm"); calm.Status != kitctx.StepStatusDone {
		t.Errorf("panic 步拖垮了同批其它调用：calm 状态 = %q", calm.Status)
	}
}

// TestRunner_InvalidArgumentsFailedNotExecuted 固化「模型给了坏参数」的处置：
// 该步失败（而不是被当成「未注册」），且工具执行体绝不进入。
func TestRunner_InvalidArgumentsFailedNotExecuted(t *testing.T) {
	var ran int32
	reg := newTestRegistry(t, simpleTool("t", func(ctx context.Context, args map[string]any) (any, error) {
		atomic.AddInt32(&ran, 1)
		return "ok", nil
	}))
	got := resultOf(t, NewRunner(reg).Run(requestCtx(0, kitctx.NopLogger(), nil),
		[]model.ToolCall{callOf("t", "{不是 JSON")}), "t")

	if got.Status != kitctx.StepStatusFailed {
		t.Errorf("坏参数步状态 = %q，期望 failed", got.Status)
	}
	if got.Rejected {
		t.Errorf("坏参数不是「未注册」：rejected 应为 false（契约点 只针对未注册工具）")
	}
	if atomic.LoadInt32(&ran) != 0 {
		t.Errorf("坏参数仍执行了工具：执行次数 = %d，期望 0", ran)
	}
}

// TestRunner_ResultOrderMatchesCalls 固化「结果与入参同序」：并行完成顺序不改变输出序
// （轨迹 Seq 与结果装配都依赖这一保证）。
func TestRunner_ResultOrderMatchesCalls(t *testing.T) {
	reg := newTestRegistry(t,
		simpleTool("first", func(ctx context.Context, args map[string]any) (any, error) {
			time.Sleep(30 * time.Millisecond) // 故意让先发的调用后完成
			return "first", nil
		}),
		simpleTool("second", okRun("second")),
	)
	calls := []model.ToolCall{callOf("first", "{}"), callOf("second", "{}")}
	results := NewRunner(reg).Run(requestCtx(0, kitctx.NopLogger(), nil), calls)

	if len(results) != len(calls) {
		t.Fatalf("结果条数 = %d，期望 %d", len(results), len(calls))
	}
	for i, r := range results {
		if r.Call.ID != calls[i].ID || r.Call.Function.Name != calls[i].Function.Name {
			t.Errorf("位次 %d 的结果与入参不对齐：got %q/%q want %q/%q",
				i, r.Call.ID, r.Call.Function.Name, calls[i].ID, calls[i].Function.Name)
		}
	}
}

// TestRunner_ElapsedUsesInjectedClock 固化「耗时一律经注入 Clock 取值」（kitctx 契约）：
// 固定时钟下真实 sleep 也不产生耗时；未注入时回落系统时钟、耗时为真。
func TestRunner_ElapsedUsesInjectedClock(t *testing.T) {
	sleepy := func(ctx context.Context, args map[string]any) (any, error) {
		time.Sleep(15 * time.Millisecond)
		return "ok", nil
	}
	reg := newTestRegistry(t, simpleTool("sleepy", sleepy))
	r := NewRunner(reg)

	fixed := fixedClock{at: time.Unix(1700000000, 0)}
	withFixed := resultOf(t, r.Run(requestCtx(0, kitctx.NopLogger(), fixed),
		[]model.ToolCall{callOf("sleepy", "{}")}), "sleepy")
	if withFixed.Elapsed != 0 {
		t.Errorf("耗时未取自注入 Clock：固定时钟下 elapsed = %v，期望 0", withFixed.Elapsed)
	}

	withSystem := resultOf(t, r.Run(roleCtx(runnerRoleAnalyst),
		[]model.ToolCall{callOf("sleepy", "{}")}), "sleepy")
	if withSystem.Elapsed <= 0 {
		t.Errorf("未注入时钟时耗时应取自系统时钟且为正，got %v", withSystem.Elapsed)
	}
}

// TestRunner_ConcurrentRunAndTrim 在 -race 下并发跑 Run 与注册表裁剪/查询：
// 证明「并行执行 + 并发只读注册表」无数据竞争。
func TestRunner_ConcurrentRunAndTrim(t *testing.T) {
	reg := sampleRegistry(t) // 复用 registry_test.go 的「宿主入口词 + admin 三链路」注册表
	r := NewRunner(reg)
	calls := []model.ToolCall{
		callOf("user.profile", "{}"), callOf("correlate", "{}"), callOf("report", "{}"),
	}
	ctx := requestCtx(0, kitctx.NopLogger(), nil)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				results := r.Run(ctx, calls)
				if len(results) != len(calls) {
					t.Errorf("并发 Run 结果条数 = %d，期望 %d", len(results), len(calls))
					return
				}
				for _, res := range results {
					if res.Status != kitctx.StepStatusDone {
						t.Errorf("并发 Run 中 %s 状态 = %q（err=%v）", res.Call.Function.Name, res.Status, res.Err)
						return
					}
				}
				_ = reg.TrimByRole("admin")
				if _, ok := reg.Lookup("user.profile"); !ok {
					t.Errorf("并发 Run 期间注册表读丢失")
					return
				}
			}
		}()
	}
	wg.Wait()
}

// TestRunner_NoGoroutineLeak 给「并行回收」运行时证据：反复跑「快 + 超时」混合批次后，
// 协程数必须回落到基线附近 —— errgroup.Wait 保证任务已回收，每步的 cancel 保证超时定时器被释放。
func TestRunner_NoGoroutineLeak(t *testing.T) {
	reg := newTestRegistry(t,
		simpleTool("fast", okRun("fast")),
		simpleTool("slow", func(ctx context.Context, args map[string]any) (any, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}),
	)
	r := NewRunner(reg)
	calls := []model.ToolCall{callOf("fast", "{}"), callOf("slow", "{}")}
	ctx := requestCtx(20*time.Millisecond, kitctx.NopLogger(), nil)

	_ = r.Run(ctx, calls) // 预热：让运行期协程/定时器先建起来
	time.Sleep(50 * time.Millisecond)
	base := runtime.NumGoroutine()

	for i := 0; i < 20; i++ {
		results := r.Run(ctx, calls)
		if len(results) != len(calls) {
			t.Fatalf("第 %d 批结果条数 = %d，期望 %d", i, len(results), len(calls))
		}
	}

	deadline := time.Now().Add(3 * time.Second)
	for {
		now := runtime.NumGoroutine()
		if now <= base {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("协程未回收：基线 %d，当前 %d（20 批并行调用后仍高于基线 ⇒ 泄漏）", base, now)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestRunner_NilRegistryFailClosed 固化「注册表缺失 ⇒ 全部拒绝」：不 panic、不静默放行。
func TestRunner_NilRegistryFailClosed(t *testing.T) {
	t.Run("注册表为 nil", func(t *testing.T) {
		results := NewRunner(nil).Run(requestCtx(0, kitctx.NopLogger(), nil),
			[]model.ToolCall{callOf("user.profile", "{}")})
		if len(results) != 1 {
			t.Fatalf("结果条数 = %d，期望 1", len(results))
		}
		if !results[0].Rejected || results[0].Status != kitctx.StepStatusFailed {
			t.Errorf("注册表缺失时必须 fail-closed：rejected=%v status=%q", results[0].Rejected, results[0].Status)
		}
	})

	t.Run("nil Runner", func(t *testing.T) {
		var nilRunner *Runner
		results := nilRunner.Run(requestCtx(0, kitctx.NopLogger(), nil), []model.ToolCall{callOf("user.profile", "{}")})
		if len(results) != 1 || !results[0].Rejected {
			t.Errorf("nil Runner 必须 fail-closed 而不 panic：%+v", results)
		}
	})

	t.Run("空调用列表", func(t *testing.T) {
		if got := NewRunner(nil).Run(requestCtx(0, kitctx.NopLogger(), nil), nil); len(got) != 0 {
			t.Errorf("空调用列表应返回 0 条结果，got %d", len(got))
		}
	})
}

// ----------  守卫：runner.go 不得出现裸 `go` 语句 ----------

// TestRunner_NoBareGoroutine 用 AST 扫描生产源码 runner.go：
//
//	① 必须 import errgroup（并行调度走  指定的实现）；
//	② `go` 语句数必须为 0（按 errgroup 的 Go/Wait 管理生命周期 ⇒ 不存在无回收的裸 goroutine）。
//
// 并内建 L-020 的探针实证：同一判定式在违规样例上必须数得出 1 处，否则守卫是空转。
func TestRunner_NoBareGoroutine(t *testing.T) {
	src, err := os.ReadFile("runner.go")
	if err != nil {
		t.Fatalf("读取 runner.go 失败：%v", err)
	}
	count, imports, err := parseGoStmts(src)
	if err != nil {
		t.Fatalf("解析 runner.go 失败：%v", err)
	}
	if !imports["golang.org/x/sync/errgroup"] {
		t.Errorf("runner.go 未 import errgroup ⇒ 并行调度未走 契约点 指定的实现")
	}
	if count != 0 {
		t.Errorf("runner.go 含 %d 处裸 `go` 语句 ⇒ 违反 契约点（并行只能经 errgroup）", count)
	}

	probe := []byte("package p\n\nfunc f() { go func() {}() }\n")
	probeCount, _, err := parseGoStmts(probe)
	if err != nil {
		t.Fatalf("探针源码解析失败：%v", err)
	}
	if probeCount != 1 {
		t.Fatalf("探针实证失败：含 1 处裸 `go` 的样例被判为 %d 处 ⇒ 守卫判定式无效（L-020）", probeCount)
	}
}
