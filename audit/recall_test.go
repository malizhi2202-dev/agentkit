package audit

import (
	"context"
	"errors"
	"testing"
)

// Recalling 是测试与管理面回看的唯一入口；Len/Reset 若失效，回看语义就不可信。
func TestRecallingLifecycle(t *testing.T) {
	var r Recalling
	ctx := context.Background()

	if got := r.Len(); got != 0 {
		t.Fatalf("零值 Len()=%d，期望 0", got)
	}
	for _, action := range []string{"agent.ask", "tool.call"} {
		e := entry()
		e.Action = action
		if err := r.Emit(ctx, e); err != nil {
			t.Fatalf("Emit(%s)：%v", action, err)
		}
	}
	if got := r.Len(); got != 2 {
		t.Fatalf("Len()=%d，期望 2", got)
	}
	if got := r.Entries(); len(got) != 2 || got[1].Action != "tool.call" {
		t.Fatalf("Entries() 顺序或内容不符：%+v", got)
	}

	r.Reset()
	if got := r.Len(); got != 0 {
		t.Fatalf("Reset 后 Len()=%d，期望 0", got)
	}
}

// Nop 与 SinkFunc 是宿主一行的接入面：Nop 必须恒成功，SinkFunc 必须真的转发。
func TestNopAndSinkFunc(t *testing.T) {
	ctx := context.Background()
	if err := Nop().Emit(ctx, entry()); err != nil {
		t.Fatalf("Nop().Emit 必须恒成功，实得 %v", err)
	}

	var got string
	f := SinkFunc(func(_ context.Context, e Entry) error {
		got = e.Action
		return nil
	})
	if err := f.Emit(ctx, entry()); err != nil {
		t.Fatalf("SinkFunc.Emit：%v", err)
	}
	if got != entry().Action {
		t.Fatalf("SinkFunc 未转发条目：%q", got)
	}
}

type closeCountingSink struct{ closes int }

func (c *closeCountingSink) Emit(context.Context, Entry) error { return nil }
func (c *closeCountingSink) Close() error                     { c.closes++; return nil }

// RequireMasked 的两条属性：未盖章条目被拒；Close 透传到内层（否则缓冲 sink 永不 flush）。
func TestRequireMaskedRejectsUnmaskedAndPreservesCloser(t *testing.T) {
	ctx := context.Background()

	if err := RequireMasked(Nop()).Emit(ctx, entry()); !errors.Is(err, ErrNotMasked) {
		t.Fatalf("未盖章条目必须被 RequireMasked 拒绝，实得 %v", err)
	}

	inner := &closeCountingSink{}
	guarded := RequireMasked(inner)
	f, err := New(BlacklistMasker(), nil, guarded)
	if err != nil {
		t.Fatalf("New：%v", err)
	}
	if err := f.Emit(ctx, entry()); err != nil {
		t.Fatalf("经 Fanout 盖章后应放行，实得 %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close：%v", err)
	}
	if inner.closes != 1 {
		t.Fatalf("内层 sink Close 被调用 %d 次，期望 1", inner.closes)
	}
}
