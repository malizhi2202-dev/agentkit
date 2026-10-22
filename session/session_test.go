package session

import (
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/malizhi2202-dev/agentkit/agent"
	"github.com/malizhi2202-dev/agentkit/kitctx"
)

func item(value string) agent.MemoryItem {
	return agent.MemoryItem{Group: agent.GroupEntity, EntityType: "ip", Value: value,
		Source: agent.ToolSource("profile.query")}
}

func TestAssemble_NewSessionAndBrokenHandle(t *testing.T) {
	// 零值请求 = 新会话：空记忆、不虚报压缩。
	a, err := Assemble(Request{})
	if err != nil {
		t.Fatalf("新会话不该失败：%v", err)
	}
	if a.Memory == nil || len(a.Memory.Items) != 0 || a.Notice != "" {
		t.Fatalf("新会话应是空记忆且无压缩告知：%+v", a)
	}
	if a.Handle == nil {
		t.Fatal("应回显可携带的句柄（哪怕是空的）")
	}
	// 句柄载荷坏掉 ⇒ 明确失败，**不**静默当新会话（否则用户以为记忆被清空）。
	_, err = Assemble(Request{Handle: &kitctx.MemoryHandle{Revision: 3, Payload: json.RawMessage(`{`)}})
	if err == nil {
		t.Fatal("坏载荷必须失败")
	}
	if !errors.Is(err, agent.ErrMemoryPayload) {
		t.Fatalf("坏载荷应落到 agent 的载荷哨兵，实得 %v", err)
	}
}

func TestAssemble_OrderIsContract(t *testing.T) {
	first, err := Assemble(Request{NewItems: []agent.MemoryItem{item("1.2.3.4")}})
	if err != nil {
		t.Fatalf("新增：%v", err)
	}
	id := first.Memory.Items[0].ID
	// Clear 先发生 ⇒ 之后按 ID 定位必然落空。矛盾指令要显式报错，不许静默吞。
	_, err = Assemble(Request{Handle: first.Handle, Clear: true, Deletes: []string{id}})
	if !errors.Is(err, agent.ErrItemNotFound) {
		t.Fatalf("Clear→Deletes 的矛盾应报 ErrItemNotFound，实得 %v", err)
	}
	// 不 Clear 时同一条删除合法。
	if _, err := Assemble(Request{Handle: first.Handle, Deletes: []string{id}}); err != nil {
		t.Fatalf("正常删除应成功：%v", err)
	}
}

func TestAssemble_ClosedSetRejectsWithoutHalfResult(t *testing.T) {
	for name, bad := range map[string]agent.MemoryItem{
		"未知分组": {Group: "feelings", Value: "x", Source: agent.ToolSource("t")},
		"未知来源": {Group: agent.GroupEntity, Value: "x", Source: "guess:9"},
		"空内容":  {Group: agent.GroupEntity, Value: "   ", Source: agent.ToolSource("t")},
	} {
		good := item("1.2.3.4")
		a, err := Assemble(Request{NewItems: []agent.MemoryItem{good, bad}})
		if err == nil {
			t.Fatalf("%s：闭集外的值必须被拒", name)
		}
		// 关键断言：失败时**没有半成品**（调用方拿到的是明确失败，不是一份可疑记忆）。
		if a.Memory != nil || a.Handle != nil || a.Notice != "" || a.Conflicts != nil {
			t.Fatalf("%s：失败却返回了半装配结果：%+v", name, a)
		}
	}
}

func TestAssemble_EchoRoundTripIsDeterministic(t *testing.T) {
	req := Request{NewItems: []agent.MemoryItem{item("1.2.3.4"), item("5.6.7.8")},
		Used: 3, Window: 100}
	a1, err := Assemble(req)
	if err != nil {
		t.Fatalf("第一轮：%v", err)
	}
	a2, err := Assemble(req)
	if err != nil {
		t.Fatalf("同请求重发：%v", err)
	}
	if !jsonEqual(a1.Handle.Payload, a2.Handle.Payload) || a1.Handle.Revision != a2.Handle.Revision {
		t.Fatalf("同请求两次装配结果不同（本包无包级可变状态的要求落空）：rev %d vs %d",
			a1.Handle.Revision, a2.Handle.Revision)
	}
	// 带上一次句柄继续：条目累积、修订号不回退、纠正下一轮即生效。
	next := Request{Handle: a1.Handle, NewItems: []agent.MemoryItem{item("9.9.9.9")}}
	b, err := Assemble(next)
	if err != nil {
		t.Fatalf("第二轮：%v", err)
	}
	if len(b.Memory.Items) != 3 {
		t.Fatalf("应累积到 3 条，实得 %d", len(b.Memory.Items))
	}
	if b.Handle.Revision < a1.Handle.Revision {
		t.Fatalf("修订号回退：%d → %d", a1.Handle.Revision, b.Handle.Revision)
	}
	edited, err := Assemble(Request{Handle: b.Handle,
		Edits: []Edit{{ID: b.Memory.Items[0].ID, Value: "1.2.3.4（已纠正）"}}})
	if err != nil {
		t.Fatalf("纠正：%v", err)
	}
	if edited.Memory.Items[0].Value != "1.2.3.4（已纠正）" {
		t.Fatalf("纠正未生效：%q", edited.Memory.Items[0].Value)
	}
	// 空 Edit 值走 agent 的闭集哨兵。
	if _, err := Assemble(Request{Handle: b.Handle, Edits: []Edit{{ID: b.Memory.Items[0].ID, Value: "   "}}}); err == nil {
		t.Fatal("空纠正值必须被拒")
	}
}

func TestAssemble_UndeclaredWindowNeverClaimsCompression(t *testing.T) {
	// 未声明窗口（≤0）⇒ 绝不虚报"已压缩"（既有教训 · 契约 5）。
	for _, w := range []int{0, -1} {
		a, err := Assemble(Request{Used: 9999, Window: w})
		if err != nil {
			t.Fatalf("Window=%d：%v", w, err)
		}
		if a.Notice != "" {
			t.Fatalf("未声明窗口却虚报压缩告知（Window=%d）：%q", w, a.Notice)
		}
	}
	a, err := Assemble(Request{NewItems: []agent.MemoryItem{item("1.2.3.4")}, Used: 9999, Window: 10})
	if err != nil {
		t.Fatalf("超窗装配：%v", err)
	}
	if a.Notice != agent.CompressionNotice {
		t.Fatalf("超窗告知必须逐字为固定文案，实得 %q", a.Notice)
	}
}

func TestAssemble_IsPureUnderConcurrency(t *testing.T) {
	req := Request{NewItems: []agent.MemoryItem{item("1.2.3.4")}, Used: 5, Window: 100}
	want, err := Assemble(req)
	if err != nil {
		t.Fatalf("基准装配：%v", err)
	}
	var wg sync.WaitGroup
	same := make([]bool, 16)
	for i := range same {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			got, err := Assemble(req)
			same[i] = err == nil && got.Handle.Revision == want.Handle.Revision &&
				jsonEqual(got.Handle.Payload, want.Handle.Payload)
		}(i)
	}
	wg.Wait()
	for i, ok := range same {
		if !ok {
			t.Fatalf("第 %d 个并发装配与基准不同 ⇒ 本层藏了状态", i)
		}
	}
}

func jsonEqual(a, b json.RawMessage) bool {
	x, y := string(a), string(b)
	return x == y
}
