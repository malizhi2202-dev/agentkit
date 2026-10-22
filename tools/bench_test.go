package tools

import "testing"

// 唯一授权谓词的成本基准 —— NFR 里「每步治理开销」的可复现口径。
// 复现：go test -run '^$' -bench LookupAuthorized -benchmem -count=3 ./pkg/agentkit/tools/
// 为什么单独测：全框架的权限判定只走这一条谓词（框架裁决 规则 5），
// 它每次工具调用至少被问一次，所以它的成本就是治理税。

// benchReg 造一张 n 个工具的注册表，其中一半只允许 admin（用来测被拒路径）。
func benchReg(b *testing.B, n int) (*Registry, []string) {
	b.Helper()
	list := make([]Tool, 0, n)
	names := make([]string, 0, n)
	for i := 0; i < n; i++ {
		name := "t" + string(rune('a'+i%26)) + itoa(i/26)
		roles := []string{"support", "admin"}
		if i%2 == 0 {
			roles = []string{"admin"}
		}
		list = append(list, Tool{Name: name, Summary: name, RequiredRoles: roles, Run: fakeRun(name)})
		names = append(names, name)
	}
	reg, err := NewRegistry(list...)
	if err != nil {
		b.Fatalf("造注册表失败：%v", err)
	}
	return reg, names
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var bs []byte
	for i > 0 {
		bs = append([]byte{byte('0' + i%10)}, bs...)
		i /= 10
	}
	return string(bs)
}

// 允许路径：support 角色查「只允许 admin」的工具 ⇒ allowed=false（被裁，须留痕）。
// sink 承接返回值，防编译器把被测调用当空转消掉（否则基准数字是假的）。
var sink struct {
	tool       Tool
	registered bool
	allowed    bool
}

func benchLookup(b *testing.B, n int, role string) {
	reg, names := benchReg(b, n)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, name := range names {
			sink.tool, sink.registered, sink.allowed = reg.LookupAuthorized(name, role)
		}
	}
}

func BenchmarkLookupAuthorized_30Tools_v3(b *testing.B)         { benchLookup(b, 30, "support") }
func BenchmarkLookupAuthorized_300Tools_v3(b *testing.B)        { benchLookup(b, 300, "support") }
func BenchmarkLookupAuthorized_300Tools_EmptyRole(b *testing.B) { benchLookup(b, 300, "") }
