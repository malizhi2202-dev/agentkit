package session

import (
	"fmt"
	"testing"

	"github.com/malizhi2202-dev/agentkit/agent"
	"github.com/malizhi2202-dev/agentkit/kitctx"
)

// 这组基准为 NFR 提供可复现的实测口径（结论要有证据，不靠估）。
// 复现：go test -run '^$' -bench Assemble -benchmem -count=5 ./pkg/agentkit/session/
// 关注点：装配是纯函数 ⇒ 每请求成本只随条目数线性增长，无锁、无 IO、无落库。

// seed 预装 n 条记忆并返回携带句柄（装配前的准备，不计入被测区间）。
func seed(b *testing.B, n int) *kitctx.MemoryHandle {
	b.Helper()
	items := make([]agent.MemoryItem, 0, n)
	for i := 0; i < n; i++ {
		items = append(items, item(fmt.Sprintf("10.%d.%d.%d", i/65536%256, i/256%256, i%256)))
	}
	a, err := Assemble(Request{NewItems: items})
	if err != nil {
		b.Fatalf("seed %d 条装配失败：%v", n, err)
	}
	return a.Handle
}

// benchOne 测「带 n 条历史 + 本轮 1 条新增 + 1 条改写 + 未超窗」的单请求装配成本。
func benchOne(b *testing.B, n int) {
	h := seed(b, n)
	req := Request{
		Handle:   h,
		NewItems: []agent.MemoryItem{item("192.168.0.1")},
		Used:     n * 40,
		Window:   n * 80, // 不触发压缩，测纯装配路径
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := Assemble(req); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkAssemble_10Items(b *testing.B)   { benchOne(b, 10) }
func BenchmarkAssemble_100Items(b *testing.B)  { benchOne(b, 100) }
func BenchmarkAssemble_1000Items(b *testing.B) { benchOne(b, 1000) }

// 压缩路径单独测：超窗时多走一次摘要，成本口径不同，不能与纯装配混报。
func BenchmarkAssemble_100ItemsCompressed(b *testing.B) {
	req := Request{
		Handle:   seed(b, 100),
		NewItems: []agent.MemoryItem{item("192.168.0.1")},
		Used:     100000,
		Window:   1, // 必然超窗 ⇒ 走压缩
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		a, err := Assemble(req)
		if err != nil {
			b.Fatal(err)
		}
		if a.Notice == "" { // 防止测了个不干活的路径
			b.Fatal("声明超窗却未压缩，基准测了空转")
		}
	}
}
