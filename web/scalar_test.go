package web

import (
	"encoding/json"
	"testing"
)

// scalarString 把查询串里的标量原样搬进 tool 参数：每个分支都是一种真实入参形状。
func TestScalarStringBranches(t *testing.T) {
	cases := []struct {
		in   any
		want string
	}{
		{"abc", "abc"},
		{true, "true"},
		{false, "false"},
		{float64(3), "3"},     // 整值浮点：不能出现 ".0"
		{float64(1.5), "1.5"}, // 非整值浮点：走 'g' 格式
		{7, "7"},
		{json.Number("42"), "42"},
	}
	for _, c := range cases {
		got, err := scalarString(c.in)
		if err != nil {
			t.Fatalf("scalarString(%#v) 报错：%v", c.in, err)
		}
		if got != c.want {
			t.Fatalf("scalarString(%#v)=%q，期望 %q", c.in, got, c.want)
		}
	}

	if _, err := scalarString(struct{ X int }{1}); err == nil {
		t.Fatal("不支持的类型必须报错，否则会被静默塞进工具参数")
	}
}

// recorder 模拟进程内 ResponseWriter：重复写头必须被忽略（与 net/http 一致），否则状态码会被后写覆盖。
func TestRecorderWriteHeaderSemantics(t *testing.T) {
	var r recorder
	if got := r.StatusCode(); got != 200 {
		t.Fatalf("未写响应时 StatusCode()=%d，期望 200", got)
	}
	r.Header().Set("X-Test", "1")
	if r.Header().Get("X-Test") != "1" {
		t.Fatal("Header() 必须返回可写的同一份 header")
	}
	r.WriteHeader(201)
	r.WriteHeader(500) // 必须被忽略
	if got := r.StatusCode(); got != 201 {
		t.Fatalf("重复写头后 StatusCode()=%d，期望 201", got)
	}

	var r2 recorder
	if _, err := r2.Write([]byte("hi")); err != nil {
		t.Fatalf("Write：%v", err)
	}
	if got := r2.StatusCode(); got != 200 {
		t.Fatalf("隐式写头 StatusCode()=%d，期望 200", got)
	}

	if m, p := splitKey("GET /x"); m != "GET" || p != "/x" {
		t.Fatalf("splitKey=%q,%q", m, p)
	}
	if m, p := splitKey("bad"); m != "bad" || p != "/" {
		t.Fatalf("splitKey 无空格的兜底=%q,%q", m, p)
	}
}
