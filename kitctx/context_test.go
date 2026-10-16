package kitctx

import (
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------- 测试替身：注入点 ----------

type fakeClock struct{ now time.Time }

func (c fakeClock) Now() time.Time { return c.now }

type logEntry struct {
	level  string
	msg    string
	fields map[string]any
}

type recordingLogger struct {
	mu      sync.Mutex
	entries []logEntry
}

func (l *recordingLogger) append(level, msg string, fields map[string]any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = append(l.entries, logEntry{level: level, msg: msg, fields: fields})
}

func (l *recordingLogger) Debug(msg string, fields map[string]any) { l.append("debug", msg, fields) }
func (l *recordingLogger) Info(msg string, fields map[string]any)  { l.append("info", msg, fields) }
func (l *recordingLogger) Warn(msg string, fields map[string]any)  { l.append("warn", msg, fields) }
func (l *recordingLogger) Error(msg string, fields map[string]any) { l.append("error", msg, fields) }

func (l *recordingLogger) snapshot() []logEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]logEntry, len(l.entries))
	copy(out, l.entries)
	return out
}

// ---------- ：上下文由请求显式携带 ----------

func TestRequestContext_ExplicitCarry(t *testing.T) {
	want := RequestContext{
		UserID:          "u-1",
		Role:            Role("support"),
		CallerID:        "implatform",
		RepresentedUser: "u-9",
		TraceID:         "trace-abc",
		Limits:          Limits{MaxSteps: 6, StepTimeout: 3 * time.Second},
		Memory:          &MemoryHandle{Revision: 2, Payload: json.RawMessage(`{"entity":["ip:1.2.3.4"]}`)},
		Injections: Injections{
			Clock:  fakeClock{now: time.Unix(1_700_000_000, 0)},
			Logger: &recordingLogger{},
		},
	}
	ctx := WithRequest(context.Background(), want)

	got, ok := RequestFrom(ctx)
	if !ok {
		t.Fatal("RequestFrom 未取到显式携带的上下文（契约点）")
	}
	if got.UserID != want.UserID || got.Role != want.Role || got.CallerID != want.CallerID ||
		got.RepresentedUser != want.RepresentedUser || got.TraceID != want.TraceID {
		t.Fatalf("身份字段未原样回显：got=%+v want=%+v", got, want)
	}
	if got.Limits != want.Limits {
		t.Fatalf("Limits 未原样回显：got=%+v want=%+v", got.Limits, want.Limits)
	}
	if got.Memory == nil || got.Memory.Revision != 2 ||
		string(got.Memory.Payload) != string(want.Memory.Payload) {
		t.Fatalf("记忆句柄未原样回显：got=%+v", got.Memory)
	}

	// 未挂载 / nil ctx 不得 panic，也不得凭空造出上下文
	if _, ok := RequestFrom(context.Background()); ok {
		t.Fatal("空 ctx 不应取到上下文")
	}
	if _, ok := RequestFrom(nil); ok {
		t.Fatal("nil ctx 不应取到上下文")
	}

	// 显式携带 ⇒ 可随请求序列化往返（$.data.context 回显）；注入点是活对象，不得进入回显
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("上下文必须可序列化（契约点 回显前提）：%v", err)
	}
	for _, forbidden := range []string{"injections", "clock", "Clock", "logger", "Logger"} {
		if strings.Contains(string(raw), forbidden) {
			t.Errorf("注入点泄漏进序列化结果（key=%q）：%s", forbidden, raw)
		}
	}
	var back RequestContext
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("上下文必须可反序列化：%v", err)
	}
	if back.UserID != want.UserID || back.Role != want.Role || back.TraceID != want.TraceID ||
		back.Limits != want.Limits {
		t.Fatalf("序列化往返后身份字段不一致：%+v", back)
	}
	if back.Injections.Clock != nil || back.Injections.Logger != nil {
		t.Fatal("反序列化后注入点必须为空（注入点不走请求体）")
	}
}

func TestRequestContext_NoImplicitStateBetweenRequests(t *testing.T) {
	a := WithRequest(context.Background(), RequestContext{UserID: "a", Role: Role("v1"), TraceID: "t-a"})
	b := WithRequest(context.Background(), RequestContext{UserID: "b", Role: Role("admin"), TraceID: "t-b"})

	ra, okA := RequestFrom(a)
	rb, okB := RequestFrom(b)
	if !okA || !okB || ra.UserID != "a" || rb.UserID != "b" || ra.TraceID != "t-a" || rb.TraceID != "t-b" {
		t.Fatalf("两次请求的上下文互相污染；引擎必须无隐含服务端状态（契约点）：a=%+v b=%+v", ra, rb)
	}
}

// ---------- ：角色只来自 ctx ----------

func TestRoleFrom_OnlyFromCtx(t *testing.T) {
	if r, ok := RoleFrom(context.Background()); ok || r != "" {
		t.Fatalf("角色不得脱离 ctx 存在，got=(%q,%v)", r, ok)
	}
	if r, ok := RoleFrom(nil); ok || r != "" {
		t.Fatalf("nil ctx 下不得返回角色，got=(%q,%v)", r, ok)
	}

	ctx := WithRequest(context.Background(), RequestContext{Role: Role("admin"), TraceID: "t-1"})
	r, ok := RoleFrom(ctx)
	if !ok || r != Role("admin") {
		t.Fatalf("角色必须能从 ctx 取出，got=(%q,%v)", r, ok)
	}

	// 空角色 = 不可判定 ⇒ ok=false，调用方须 fail-closed（不注册 / 拒绝），不得放行
	blank := WithRequest(context.Background(), RequestContext{TraceID: "t-2"})
	if r, ok := RoleFrom(blank); ok || r != "" {
		t.Fatalf("空角色必须判为不可判定，got=(%q,%v)", r, ok)
	}
}

// TestGuard_RoleIsNeverAFunctionParam 固化  的形态约束：
// 本包不得出现以 Role 为形参的函数（角色只允许从 ctx 取）。
func TestGuard_RoleIsNeverAFunctionParam(t *testing.T) {
	fset := token.NewFileSet()
	for _, name := range packageSources(t) {
		file, err := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if err != nil {
			t.Fatalf("解析 %s 失败：%v", name, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Type.Params == nil {
				continue
			}
			for _, field := range fn.Type.Params.List {
				if !isRoleType(field.Type) {
					continue
				}
				t.Errorf("%s:%d 函数 %s 以 Role 作为形参；角色只允许从 ctx 取（契约点）",
					name, fset.Position(fn.Pos()).Line, fn.Name.Name)
			}
		}
	}
}

// ---------- ：轨迹只靠 traceid 即可重建完整序列 ----------

func TestTrace_ReplayByTraceID(t *testing.T) {
	trA := NewTrace("trace-a")
	trB := NewTrace("trace-b")

	record := func(tr *Trace, name string, status StepStatus) Step {
		t.Helper()
		step, err := tr.Record(Step{
			Name:          name,
			Params:        "entity=ip:1.2.3.4",
			Status:        status,
			Elapsed:       12 * time.Millisecond,
			ResultSummary: name + " fetched",
		})
		if err != nil {
			t.Fatalf("Record(%s) 失败：%v", name, err)
		}
		return step
	}

	stepA1 := record(trA, "user.profile", StepStatusDone)
	stepB1 := record(trB, "correlate", StepStatusFailed)
	stepA2 := record(trA, "report", StepStatusDone)

	// 落日志的形态是扁平记录（LOG:engine.trail[]），且可能乱序 / 混多个 trace
	logged := []Step{stepA2, stepB1, stepA1}

	got, ok := Replay(logged, "trace-a")
	if !ok {
		t.Fatal("必须只靠 traceid 重建序列（契约点）")
	}
	if len(got) != 2 {
		t.Fatalf("重建序列步数错误：got=%d want=2（不得混入其它 traceid 的步骤）", len(got))
	}
	if got[0].Name != "user.profile" || got[1].Name != "report" {
		t.Fatalf("重建序列顺序错误（须按 Seq 升序）：%+v", got)
	}
	for i, step := range got {
		if step.Seq != i+1 || step.TraceID != "trace-a" {
			t.Fatalf("第 %d 步的 Seq/TraceID 错误：%+v", i, step)
		}
		if step.Params == "" || step.ResultSummary == "" || step.Elapsed <= 0 || !step.Status.Valid() {
			t.Fatalf("重建后的步骤必须保留参数摘要/状态/耗时/结果摘要：%+v", step)
		}
	}
	if got[0].Status != StepStatusDone || got[0].Elapsed != 12*time.Millisecond ||
		got[0].ResultSummary != "user.profile fetched" {
		t.Fatalf("重建后字段未被完整保留：%+v", got[0])
	}

	// 同一个 traceid 重复重建必须得到同一序列（可回放、确定性）
	again, _ := Replay(logged, "trace-a")
	if len(again) != len(got) || again[0] != got[0] || again[1] != got[1] {
		t.Fatalf("同一 traceid 两次重建结果不一致：%+v vs %+v", again, got)
	}

	// 未知 traceid / 空 traceid 必须显式无结果（不冒充成功）
	if _, ok := Replay(logged, "trace-x"); ok {
		t.Fatal("未知 traceid 不得重建出序列")
	}
	if _, ok := Replay(logged, ""); ok {
		t.Fatal("空 traceid 不得重建出序列")
	}
	if _, ok := Replay(nil, "trace-a"); ok {
		t.Fatal("空记录不得重建出序列")
	}
}

func TestTrace_RecordStampsTraceIDAndSeq(t *testing.T) {
	tr := NewTrace("t-1")
	first, err := tr.Record(Step{Name: "user.profile", Status: StepStatusRunning})
	if err != nil {
		t.Fatalf("Record 失败：%v", err)
	}
	if first.TraceID != "t-1" || first.Seq != 1 {
		t.Fatalf("Record 必须补齐 TraceID 与 Seq：%+v", first)
	}

	// 误传别的 traceid ⇒ 拒绝，不静默混入（否则回放会串号）
	if _, err := tr.Record(Step{TraceID: "t-2", Name: "x", Status: StepStatusDone}); !errors.Is(err, ErrTraceIDMismatch) {
		t.Fatalf("跨 traceid 记录必须被拒绝，got err=%v", err)
	}
	// 半成品记录一律拒绝
	if _, err := tr.Record(Step{Status: StepStatusDone}); !errors.Is(err, ErrStepNameRequired) {
		t.Fatalf("缺 name 必须被拒绝，got err=%v", err)
	}
	if _, err := tr.Record(Step{Name: "x", Status: StepStatus("bogus")}); !errors.Is(err, ErrStepStatusInvalid) {
		t.Fatalf("非法状态必须被拒绝，got err=%v", err)
	}
	if _, err := NewTrace("").Record(Step{Name: "x", Status: StepStatusDone}); !errors.Is(err, ErrTraceIDRequired) {
		t.Fatalf("空 traceid 必须被拒绝，got err=%v", err)
	}

	// 被拒绝的步骤不得占用序号、不得改变已有序列
	records := tr.Records()
	if len(records) != 1 || records[0].Seq != 1 {
		t.Fatalf("被拒绝的记录污染了轨迹：%+v", records)
	}

	// Records 返回副本：外部改动不得影响轨迹本身的回放结果
	records[0].Name = "tampered"
	if tr.Records()[0].Name != "user.profile" {
		t.Fatal("Records 必须返回副本，否则回放不可信")
	}
}

func TestTrace_ConcurrentRecordKeepsReplayableSequence(t *testing.T) {
	const n = 32
	tr := NewTrace("trace-c")

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := tr.Record(Step{Name: "correlate", Status: StepStatusDone}); err != nil {
				t.Errorf("并发 Record 失败：%v", err)
			}
		}()
	}
	wg.Wait()

	records := tr.Records()
	if len(records) != n {
		t.Fatalf("并发记录丢失：got=%d want=%d", len(records), n)
	}
	replayed, ok := Replay(records, "trace-c")
	if !ok || len(replayed) != n {
		t.Fatalf("并发后仍须可按 traceid 重建完整序列：ok=%v got=%d", ok, len(replayed))
	}
	seen := make([]int, 0, n)
	for _, step := range replayed {
		seen = append(seen, step.Seq)
	}
	sort.Ints(seen)
	for i, seq := range seen {
		if seq != i+1 {
			t.Fatalf("并发记录的 Seq 必须唯一且连续：%v", seen)
		}
	}
}

// ---------- ADR-004：注入点默认值 / 签名只用 stdlib context ----------

func TestInjections_ClockAndLoggerDefaults(t *testing.T) {
	base := context.Background()

	// 未注入也不得 panic、不得返回 nil（框架内各处直接调用）
	if ClockFrom(base) == nil || ClockFrom(nil) == nil {
		t.Fatal("未注入时钟时必须返回系统时钟，绝不返回 nil")
	}
	if LoggerFrom(base) == nil || LoggerFrom(nil) == nil {
		t.Fatal("未注入日志时必须返回丢弃实现，绝不返回 nil")
	}
	LoggerFrom(base).Info("noop must not panic", map[string]any{"k": "v"})
	if ClockFrom(base).Now().IsZero() {
		t.Fatal("系统时钟必须返回真实时间")
	}
	// 默认实现自身也必须是可用契约（宿主/测试可显式取用）
	if SystemClock() == nil || SystemClock().Now().IsZero() {
		t.Fatal("SystemClock 必须返回可用的系统时钟")
	}
	if NopLogger() == nil {
		t.Fatal("NopLogger 必须返回可用的丢弃实现")
	}
	NopLogger().Error("discard", map[string]any{"k": "v"})

	// 注入后原样返回
	fixed := time.Unix(1_700_000_000, 0)
	rec := &recordingLogger{}
	ctx := WithRequest(base, RequestContext{
		TraceID:    "t-inject",
		Injections: Injections{Clock: fakeClock{now: fixed}, Logger: rec},
	})
	if got := ClockFrom(ctx).Now(); !got.Equal(fixed) {
		t.Fatalf("注入时钟未被采用：got=%v want=%v", got, fixed)
	}
	LoggerFrom(ctx).Warn("degraded", map[string]any{"reason": "model_unavailable"})

	entries := rec.snapshot()
	if len(entries) != 1 || entries[0].level != "warn" || entries[0].msg != "degraded" ||
		entries[0].fields["reason"] != "model_unavailable" {
		t.Fatalf("注入日志未收到记录：%+v", entries)
	}
}

// TestGuard_KitctxImportsStdlibOnly 固化  的签名约束：
// 本包只允许依赖 Go 标准库 —— 既保证「只允许 stdlib context.Context」（不出现 magdefined.Context），
// 也保证本包可整目录搬到另一个 module（ADR-004）。
//
// 判据按 **import 路径**而非源码文本：文档里提到被禁止的类型名属契约登记，不算违规。
func TestGuard_KitctxImportsStdlibOnly(t *testing.T) {
	fset := token.NewFileSet()
	for _, name := range packageSources(t) {
		file, err := parser.ParseFile(fset, filepath.Join(".", name), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("解析 %s 失败：%v", name, err)
		}
		for _, imp := range file.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				t.Fatalf("解析 %s 的 import 路径失败：%v", name, err)
			}
			if strings.Contains(path, "magdefined") || strings.Contains(path, "magweb") ||
				strings.Contains(path, "宿主侧") {
				t.Errorf("%s 依赖 %q：注入接口只允许 stdlib context.Context（ADR-004）", name, path)
			}
			// 标准库路径的首段不含 "."；含点即第三方或本项目内部包，一律不允许。
			if strings.Contains(strings.SplitN(path, "/", 2)[0], ".") {
				t.Errorf("%s 依赖非标准库包 %q：本包必须零宿主依赖（ADR-004）", name, path)
			}
		}
	}
}

// ---------- 测试辅助 ----------

// packageSources 返回本包的非测试源文件名（AST/文本守卫扫描用）。
func packageSources(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("读取包目录失败：%v", err)
	}
	var out []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	if len(out) == 0 {
		t.Fatal("未扫描到任何源文件，守卫失效")
	}
	return out
}

func isRoleType(expr ast.Expr) bool {
	switch v := expr.(type) {
	case *ast.Ident:
		return v.Name == "Role"
	case *ast.StarExpr:
		return isRoleType(v.X)
	case *ast.ArrayType:
		return isRoleType(v.Elt)
	}
	return false
}
