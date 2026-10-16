package kitctx

import (
	"strings"
	"testing"
)

// TestKitctx_DigestIsSHA256Hex 钉住共享摘要口径：
// 轨迹摘要（agent）与幂等键（playbook）都走这一个函数，口径漂了必须在这里红，而不是在使用方各自红。
func TestKitctx_DigestIsSHA256Hex(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		{"", "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"},
		{"abc", "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"},
	}
	for _, c := range cases {
		got := Digest([]byte(c.raw))
		if got != c.want {
			t.Fatalf("Digest(%q) 应等于 NIST 已知向量 %s，实得 %s", c.raw, c.want, got)
		}
	}
	if got := Digest([]byte("abc")); got != Digest([]byte("abc")) {
		t.Fatal("同一输入两次摘要不同")
	}
	if Digest([]byte("abc")) == Digest([]byte("abd")) {
		t.Fatal("不同输入撞了同一个摘要")
	}
	if len(Digest(nil)) != 64 || strings.TrimRight(Digest(nil), "0123456789abcdef") != "" {
		t.Fatalf("摘要必须是 64 位十六进制：%q", Digest(nil))
	}
	// 短形态由使用方截断，不是第二份实现（这里只确认截出来仍是前缀）
	if got := Digest([]byte("abc"))[:12]; got != "ba7816bf8f01" {
		t.Fatalf("截断必须取前缀：%s", got)
	}
}
