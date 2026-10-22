package audit

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/malizhi2202-dev/agentkit/kitctx"
	"github.com/malizhi2202-dev/agentkit/security"
)

// countingSink 记录收到的条目，可注入失败与关闭错误。
type countingSink struct {
	got    []Entry
	err    error
	closes int
	closeE error
}

func (s *countingSink) Emit(_ context.Context, e Entry) error {
	if s.err != nil {
		return s.err
	}
	s.got = append(s.got, e.Clone())
	return nil
}

func (s *countingSink) Close() error {
	s.closes++
	return s.closeE
}

func entry() Entry {
	return Entry{Action: "agent.ask", Outcome: OutcomeOK, Fields: map[string]any{"ip": "1.2.3.4", "imei": "86999"}}
}

func TestAudit_ConstructionFailsClosed(t *testing.T) {
	if _, err := New(nil, nil, &countingSink{}); !errors.Is(err, ErrMaskerRequired) {
		t.Fatalf("无 Masker 必须拒绝构造，实得 %v", err)
	}
	if _, err := New(BlacklistMasker(), nil); !errors.Is(err, ErrNoSink) {
		t.Fatalf("空 sink 集必须拒绝（等于静默丢弃），实得 %v", err)
	}
	if _, err := New(BlacklistMasker(), nil, &countingSink{}, nil); !errors.Is(err, ErrSinkNil) {
		t.Fatalf("nil sink 必须拒绝，实得 %v", err)
	}
	f, err := New(BlacklistMasker(), kitctx.SystemClock(), Nop())
	if err != nil {
		t.Fatalf("合法构造失败：%v", err)
	}
	if f.Sinks() != 1 {
		t.Fatalf("Sinks()=%d", f.Sinks())
	}
}

// TestAudit_RawEntryCannotReachSink 是本包的核心安全属性：
// "未脱敏"这条事实**无法从外部伪造**，也无法靠字段声明绕开。
func TestAudit_RawEntryCannotReachSink(t *testing.T) {
	rec := &countingSink{}
	f, err := New(BlacklistMasker(), nil, rec)
	if err != nil {
		t.Fatalf("New：%v", err)
	}
	// 外部构造的条目（哪怕字段全填好）一律无效。
	raw := entry()
	if err := raw.Valid(); !errors.Is(err, ErrNotMasked) {
		t.Fatalf("未脱敏条目必须被判 ErrNotMasked，实得 %v", err)
	}
	if raw.Sanitized() || raw.Masked() {
		t.Fatal("外部构造的条目不得自称已脱敏")
	}
	// 用 RequireMasked 包住的 sink 必须拒绝它。
	if err := RequireMasked(rec).Emit(context.Background(), raw); !errors.Is(err, ErrNotMasked) {
		t.Fatalf("RequireMasked 未拦住原样条目：%v", err)
	}
	if len(rec.got) != 0 {
		t.Fatal("sink 竟然收到了未脱敏条目")
	}
	// 唯一能产出有效条目的路径是 Fanout.Emit。
	if err := f.Emit(context.Background(), raw); err != nil {
		t.Fatalf("Fanout.Emit 失败：%v", err)
	}
	if len(rec.got) != 1 {
		t.Fatalf("sink 收到 %d 条", len(rec.got))
	}
	if err := rec.got[0].Valid(); err != nil {
		t.Fatalf("走过 Fanout 的条目应有效：%v", err)
	}
	// 凭据不可导出：Entry 必须存在未导出字段，且没有任何导出的"盖章"API。
	typ := reflect.TypeOf(Entry{})
	unexported := 0
	for i := 0; i < typ.NumField(); i++ {
		if ft := typ.Field(i); ft.PkgPath != "" {
			unexported++
		}
	}
	if unexported == 0 {
		t.Fatal("Entry 没有未导出字段 ⇒ 脱敏凭据可被外部伪造")
	}
	// ⚠️ 反射只看得到"挂在被扫类型上的"方法：指针接收者的方法**不在值类型的方法集里**
	// （LESSONS 既有教训 第 7 条）。两个类型都扫，否则"加一个 func (e *Entry) MarkMasked"
	// 这种最直接的伪造面会静默逃过检测。
	scanned := 0
	for _, tv := range []reflect.Type{typ, reflect.TypeOf((*Entry)(nil))} {
		for i := 0; i < tv.NumMethod(); i++ {
			scanned++
			name := strings.ToLower(tv.Method(i).Name)
			for _, bad := range []string{"mark", "setproof", "force", "stamp", "unmask"} {
				if strings.Contains(name, bad) {
					t.Fatalf("出现可伪造脱敏凭据的导出方法 %q", tv.Method(i).Name)
				}
			}
		}
	}
	if scanned < 8 {
		t.Fatalf("方法集扫描疑似空转（只扫到 %d 个方法）⇒ 判据失效", scanned)
	}
}

func TestAudit_MaskedBeforeSink(t *testing.T) {
	rec := &Recalling{}
	f, err := New(BlacklistMasker(), nil, rec)
	if err != nil {
		t.Fatalf("New：%v", err)
	}
	if err := f.Emit(context.Background(), entry()); err != nil {
		t.Fatalf("Emit：%v", err)
	}
	got := rec.Entries()
	if len(got) != 1 {
		t.Fatalf("收集到 %d 条", len(got))
	}
	if _, ok := got[0].Fields["imei"]; ok {
		t.Fatalf("imei 原样入了 sink（黑名单失效）：%v", got[0].Fields)
	}
	if got[0].Fields["ip"] != "1.2.3.4" {
		t.Fatalf("非敏感字段被误删：%v", got[0].Fields)
	}
	if !reflect.DeepEqual(got[0].Applied, []string{"imei"}) {
		t.Fatalf("applied 证据应为实际被移除的字段，实得 %v", got[0].Applied)
	}
	if !got[0].Sanitized() {
		t.Fatal("确实改写过 ⇒ Sanitized 必须为 true")
	}
	if got[0].Action != "agent.ask" || got[0].Sign != "agent.ask" {
		t.Fatalf("Action/Sign 丢失：%+v", got[0])
	}
	if got[0].Time.IsZero() {
		t.Fatal("Time 必须由注入时钟补齐")
	}
}

// TestAudit_EmptyAppliedIsNotClaimedSanitized 钉住规则 3：
// 走过脱敏通道 ≠ 真的脱掉了字段。空转必须表现为空 applied，且不得被报成"已脱敏"。
func TestAudit_EmptyAppliedIsNotClaimedSanitized(t *testing.T) {
	rec := &Recalling{}
	f, err := New(BlacklistMasker(), nil, rec)
	if err != nil {
		t.Fatalf("New：%v", err)
	}
	e := entry()
	e.Fields = map[string]any{"ip": "1.2.3.4"}
	if err := f.Emit(context.Background(), e); err != nil {
		t.Fatalf("Emit：%v", err)
	}
	got := rec.Entries()[0]
	if len(got.Applied) != 0 {
		t.Fatalf("什么都没改却带着证据（过程量污染）：%v", got.Applied)
	}
	if !got.Masked() {
		t.Fatal("走过脱敏通道 ⇒ Masked 应为 true")
	}
	if got.Sanitized() {
		t.Fatal("没有任何字段被改写 ⇒ 不得声称已脱敏")
	}
}

func TestAudit_MaskerFailureBlocksEverySink(t *testing.T) {
	boom := errors.New("属性规则读不到")
	mask := func(context.Context, string, any) (any, []string, error) {
		return nil, nil, boom
	}
	a, b := &countingSink{}, &countingSink{}
	f, err := New(mask, nil, a, b)
	if err != nil {
		t.Fatalf("New：%v", err)
	}
	if err := f.Emit(context.Background(), entry()); !errors.Is(err, ErrMaskFailed) {
		t.Fatalf("脱敏失败必须上报 ErrMaskFailed，实得 %v", err)
	}
	if len(a.got)+len(b.got) != 0 {
		t.Fatal("脱敏失败却仍有 sink 收到条目 ⇒ 原样数据落盘（fail-open）")
	}
}

func TestAudit_EverySinkAttempted(t *testing.T) {
	first := &countingSink{err: errors.New("磁盘满")}
	second := &countingSink{}
	f, err := New(BlacklistMasker(), nil, first, second)
	if err != nil {
		t.Fatalf("New：%v", err)
	}
	err = f.Emit(context.Background(), entry())
	if err == nil || !strings.Contains(err.Error(), "磁盘满") {
		t.Fatalf("sink 失败必须被报告：%v", err)
	}
	if len(second.got) != 1 {
		t.Fatalf("前一个 sink 失败导致后续漏记（%d 条）⇒ 多播必须各自尝试", len(second.got))
	}
}

func TestAudit_ContextIdentityAndExplicitWins(t *testing.T) {
	rec := &Recalling{}
	f, err := New(BlacklistMasker(), nil, rec)
	if err != nil {
		t.Fatalf("New：%v", err)
	}
	ctx := kitctx.WithRequest(context.Background(), kitctx.RequestContext{
		UserID: "u1", Role: "support", TraceID: "t1",
	})
	e := entry()
	e.Role = "admin" // 显式值优先，不被 ctx 覆盖
	if err := f.Emit(ctx, e); err != nil {
		t.Fatalf("Emit：%v", err)
	}
	got := rec.Entries()[0]
	if got.UserID != "u1" || got.TraceID != "t1" {
		t.Fatalf("身份未从 ctx 补齐：%+v", got)
	}
	if got.Role != "admin" {
		t.Fatalf("显式 Role 被覆盖为 %q", got.Role)
	}
	// 无 ctx 也可投递（审计不该因为缺上下文而丢事件）。
	if err := f.Emit(context.Background(), entry()); err != nil {
		t.Fatalf("无 ctx 投递失败：%v", err)
	}
	if rec.Entries()[1].TraceID != "" {
		t.Fatal("无 ctx 时不该凭空造 trace id")
	}
}

func TestAudit_OutcomeClosedSet(t *testing.T) {
	f, err := New(BlacklistMasker(), nil, &countingSink{})
	if err != nil {
		t.Fatalf("New：%v", err)
	}
	for _, bad := range []Outcome{"", "OK", "ok ", "success"} {
		e := entry()
		e.Outcome = bad
		if err := f.Emit(context.Background(), e); !errors.Is(err, ErrOutcomeUnknown) {
			t.Fatalf("outcome %q 不在闭集内却未被拒：%v", bad, err)
		}
	}
	for _, good := range []Outcome{OutcomeOK, OutcomeDenied, OutcomeError} {
		e := entry()
		e.Outcome = good
		if err := f.Emit(context.Background(), e); err != nil {
			t.Fatalf("闭集值 %q 被拒：%v", good, err)
		}
	}
	e := entry()
	e.Action = "  "
	if err := f.Emit(context.Background(), e); !errors.Is(err, ErrActionRequired) {
		t.Fatalf("无 action 必须拒绝（否则无法 grep/告警映射），实得 %v", err)
	}
}

// TestAudit_MaskerCannotReturnArbitraryShape：Masker 交回非 map 载荷时不得"照原样投递"，
// 否则 Fields 里的敏感值就绕过了比对。
func TestAudit_MaskerCannotReturnArbitraryShape(t *testing.T) {
	mask := func(context.Context, string, any) (any, []string, error) {
		return "原样字符串", []string{"imei"}, nil
	}
	rec := &countingSink{}
	f, err := New(mask, nil, rec)
	if err != nil {
		t.Fatalf("New：%v", err)
	}
	if err := f.Emit(context.Background(), entry()); !errors.Is(err, ErrMaskFailed) {
		t.Fatalf("非 map 载荷必须拒绝投递，实得 %v", err)
	}
	if len(rec.got) != 0 {
		t.Fatal("载荷形状不可确认却仍入 sink")
	}
	// Fields 为 nil 时不涉及比对，允许投递。
	f2, err := New(BlacklistMasker("token"), nil, rec)
	if err != nil {
		t.Fatalf("New：%v", err)
	}
	e := entry()
	e.Fields = nil
	if err := f2.Emit(context.Background(), e); err != nil {
		t.Fatalf("空载荷应可投递：%v", err)
	}
	if len(rec.got) != 1 {
		t.Fatalf("sink 收到 %d 条", len(rec.got))
	}
}

// rawSink 存条目而**不克隆**：载荷隔离的责任只能在 Fanout 身上。
// （既有 countingSink 自己会 Clone ⇒ 用它做隔离断言会变成"替身替实现挡了一刀"，
//
//	变异探针把 Fanout 的 Clone 去掉照样绿 —— 这一条是探针教出来的。）
type rawSink struct{ got []Entry }

func (r *rawSink) Emit(_ context.Context, e Entry) error {
	r.got = append(r.got, e)
	return nil
}

func TestAudit_SinkPayloadIsolation(t *testing.T) {
	a, b := &rawSink{}, &rawSink{}
	f, err := New(BlacklistMasker(), nil, a, b)
	if err != nil {
		t.Fatalf("New：%v", err)
	}
	if err := f.Emit(context.Background(), entry()); err != nil {
		t.Fatalf("Emit：%v", err)
	}
	if len(a.got) != 1 || len(b.got) != 1 {
		t.Fatalf("got=%d/%d", len(a.got), len(b.got))
	}
	delete(a.got[0].Fields, "ip")
	if _, ok := b.got[0].Fields["ip"]; !ok {
		t.Fatal("两个 sink 共享同一份载荷 ⇒ 一个 sink 的改写会污染另一条审计事实")
	}
}

func TestAudit_SinksIsolatedAndClosed(t *testing.T) {
	a, b := &countingSink{}, &countingSink{}
	f, err := New(BlacklistMasker(), nil, RequireMasked(a), RequireMasked(b))
	if err != nil {
		t.Fatalf("New：%v", err)
	}
	e := entry()
	if err := f.Emit(context.Background(), e); err != nil {
		t.Fatalf("Emit：%v", err)
	}
	// 一个 sink 改了载荷，不得影响另一个已收到的事实。
	delete(a.got[0].Fields, "ip")
	if _, ok := b.got[0].Fields["ip"]; !ok {
		t.Fatal("sink 之间共享了可变载荷 ⇒ 审计事实可被互相改写")
	}
	a.closeE = errors.New("flush 失败")
	if err := f.Close(); err == nil || !strings.Contains(err.Error(), "flush 失败") {
		t.Fatalf("Close 必须聚合 sink 错误：%v", err)
	}
	if a.closes != 1 || b.closes != 1 {
		t.Fatalf("closes=%d/%d，应各关一次", a.closes, b.closes)
	}
}

// TestAudit_BlacklistExtraIsAdditiveOnly 把"追加不替换"这条 security 侧不变量
// 在本包的调用点上复验一次（本包传参方式若写错，黑名单就悄悄失效了）。
func TestAudit_BlacklistExtraIsAdditiveOnly(t *testing.T) {
	rec := &Recalling{}
	f, err := New(BlacklistMasker("clientid"), nil, rec)
	if err != nil {
		t.Fatalf("New：%v", err)
	}
	e := entry()
	e.Fields = map[string]any{"imei": "1", "clientid": "2", "ip": "3"}
	if err := f.Emit(context.Background(), e); err != nil {
		t.Fatalf("Emit：%v", err)
	}
	got := rec.Entries()[0].Fields
	if _, ok := got["imei"]; ok {
		t.Fatal("传了 extra 反而丢了内建黑名单项")
	}
	if _, ok := got["clientid"]; ok {
		t.Fatal("extra 未生效")
	}
	if len(rec.Entries()[0].Applied) != 2 {
		t.Fatalf("applied=%v", rec.Entries()[0].Applied)
	}
	var _ security.Masker = BlacklistMasker()
}
