package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"sync"
	"testing"

	"agentkit/model"
)

// admin 三链路（Epic 4）的工具名，取自  /  原文。
var adminToolNames = []string{"query_log", "query_code", "query_permission"}

// 入口命令链路（分析师可用）的工具名样例，按业务语义聚合。
var researchToolNames = []string{"user.profile", "correlate", "report"}

func fakeRun(tag string) RunFunc {
	return func(ctx context.Context, args map[string]any) (any, error) {
		return map[string]any{"tag": tag}, nil
	}
}

func stringParamSchema() map[string]any {
	return map[string]any{
		"type":       "object",
		"properties": map[string]any{"value": map[string]any{"type": "string"}},
	}
}

// sampleRegistry 构造一个「入口命令 + admin 三链路」的注册表：
// 入口命令工具未声明角色（所有已判定角色的调用者可用），admin 三链路声明 RequiredRoles=["admin"]。
func sampleRegistry(t *testing.T) *Registry {
	t.Helper()
	list := make([]Tool, 0, len(researchToolNames)+len(adminToolNames))
	for _, name := range researchToolNames {
		list = append(list, Tool{
			Name:    name,
			Params:  stringParamSchema(),
			Summary: "宿主入口词能力 " + name,
			Run:     fakeRun(name),
		})
	}
	for _, name := range adminToolNames {
		list = append(list, Tool{
			Name:          name,
			Params:        stringParamSchema(),
			Summary:       "admin 链路 " + name,
			RequiredRoles: []string{"admin"},
			Run:           fakeRun(name),
		})
	}
	reg, err := NewRegistry(list...)
	if err != nil {
		t.Fatalf("构造注册表失败：%v", err)
	}
	return reg
}

func visibleNames(vis []model.ToolVisible) []string {
	out := make([]string, 0, len(vis))
	for _, v := range vis {
		out = append(out, v.Name)
	}
	return out
}

func toolNames(list []Tool) []string {
	out := make([]string, 0, len(list))
	for _, tl := range list {
		out = append(out, tl.Name)
	}
	return out
}

func contains(list []string, want string) bool {
	for _, got := range list {
		if got == want {
			return true
		}
	}
	return false
}

// ---------- ：声明四要素齐备 ----------

func TestRegistry_ToolDeclComplete(t *testing.T) {
	reg := sampleRegistry(t)

	vis := reg.VisibleByRole("v1")
	if len(vis) != len(researchToolNames) {
		t.Fatalf("分析师可见工具数 = %d，want %d（%v）", len(vis), len(researchToolNames), visibleNames(vis))
	}

	for _, v := range vis {
		if v.Name == "" || v.Params == nil || v.Summary == "" {
			t.Fatalf("工具 %q 声明三要素不齐备：%+v", v.Name, v)
		}
		// 可见外形恰为 {name,params,summary,role} 四键（键必须存在，公开工具 role 为空串）。
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("序列化可见声明失败：%v", err)
		}
		var keys map[string]json.RawMessage
		if err := json.Unmarshal(raw, &keys); err != nil {
			t.Fatalf("反序列化可见声明失败：%v", err)
		}
		got := make([]string, 0, len(keys))
		for k := range keys {
			got = append(got, k)
		}
		sort.Strings(got)
		want := []string{"name", "params", "role", "summary"}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("可见声明键集合 = %v，want %v（payload=%s）", got, want, raw)
		}
		if _, ok := v.Params.(map[string]any); !ok {
			t.Fatalf("工具 %q 的参数 schema 应为对象形态，got %T", v.Name, v.Params)
		}
	}

	// admin 声明的 role 必须非空（四要素中的 role 有实义）。
	admins := reg.VisibleByRole("admin")
	for _, v := range admins {
		if contains(adminToolNames, v.Name) && v.Role != "admin" {
			t.Fatalf("admin 工具 %q 的 role = %q，want %q", v.Name, v.Role, "admin")
		}
	}
}

// ---------- ：admin 专属工具不注册给模型 ----------

func TestRegistry_RoleTrim(t *testing.T) {
	reg := sampleRegistry(t)

	analyst := visibleNames(reg.VisibleByRole("v1"))
	for _, name := range adminToolNames {
		if contains(analyst, name) {
			t.Fatalf("普通分析师 %q 的工具集合里出现了 admin 专属工具 %q：%v", "v1", name, analyst)
		}
	}
	for _, name := range researchToolNames {
		if !contains(analyst, name) {
			t.Fatalf("宿主入口词工具 %q 未注册给分析师：%v", name, analyst)
		}
	}

	admin := visibleNames(reg.VisibleByRole("admin"))
	for _, name := range append(append([]string{}, researchToolNames...), adminToolNames...) {
		if !contains(admin, name) {
			t.Fatalf("admin 可见工具集合缺 %q：%v", name, admin)
		}
	}
}

// TestRegistry_AdminToolsAbsentFromWireForNonAdmin 是  / D8 / AD-4 的**核心证据**：
// 非 admin 请求时，admin 三链路工具不是「被灰掉」也不是「靠提示词约束」，而是**根本不出现**在
// 发往模型的工具声明序列化结果里（wire absence）。
func TestRegistry_AdminToolsAbsentFromWireForNonAdmin(t *testing.T) {
	reg := sampleRegistry(t)

	wire, err := json.Marshal(reg.VisibleByRole("v1"))
	if err != nil {
		t.Fatalf("序列化非 admin 工具声明失败：%v", err)
	}
	for _, name := range adminToolNames {
		if bytes.Contains(wire, []byte(name)) {
			t.Fatalf("非 admin 请求的 wire 上出现了 admin 工具 %q：%s", name, wire)
		}
	}
	// 反向对照：admin 的 wire **必须**含这三个名字，证明上面不是「永远排空」造成的假通过。
	adminWire, err := json.Marshal(reg.VisibleByRole("admin"))
	if err != nil {
		t.Fatalf("序列化 admin 工具声明失败：%v", err)
	}
	for _, name := range adminToolNames {
		if !bytes.Contains(adminWire, []byte(name)) {
			t.Fatalf("admin 请求的 wire 上缺少 %q（对照失败）：%s", name, adminWire)
		}
	}

	// 内部字段（RequiredRoles / Run）在 wire 上不得出现。
	for _, leaked := range []string{"RequiredRoles", "required_roles", "Run", "run"} {
		if bytes.Contains(wire, []byte(leaked)) {
			t.Fatalf("内部字段 %q 泄漏到 wire：%s", leaked, wire)
		}
	}
}

// ---------- fail-closed 第三侧：角色不可判定 ⇒ 不注册 ----------

func TestRegistry_RoleUndeterminedFailClosed(t *testing.T) {
	reg := sampleRegistry(t)

	if got := reg.VisibleByRole(""); len(got) != 0 {
		t.Fatalf("角色不可判定（空串）时仍注册出 %d 个工具：%v", len(got), visibleNames(got))
	}
	if got := reg.TrimByRole(""); len(got) != 0 {
		t.Fatalf("角色不可判定（空串）时仍裁剪出 %d 个工具：%v", len(got), toolNames(got))
	}
	// 未知角色同样按「无权」处理：公开工具照给，admin 工具不给。
	unknown := visibleNames(reg.VisibleByRole("outsider"))
	if contains(unknown, adminToolNames[0]) {
		t.Fatalf("角色体系外的调用者拿到了 admin 工具：%v", unknown)
	}
}

// TestRegistry_MalformedRoleDeclFailClosed 固化一条硬化判据：
// 声明了 `RequiredRoles` 却全是空白的畸形声明，**不得**因为「归一化后为空」被当成
// 「未声明角色要求」而静默变成公开工具。
func TestRegistry_MalformedRoleDeclFailClosed(t *testing.T) {
	reg, err := NewRegistry(Tool{
		Name:          "malformed",
		Params:        stringParamSchema(),
		Summary:       "畸形角色声明",
		RequiredRoles: []string{" ", ""},
		Run:           fakeRun("malformed"),
	})
	if err != nil {
		t.Fatalf("注册失败：%v", err)
	}
	if got := reg.VisibleByRole("admin"); len(got) != 0 {
		t.Fatalf("畸形角色声明被当成公开工具放行：%v", visibleNames(got))
	}
	if got := reg.VisibleByRole("v1"); len(got) != 0 {
		t.Fatalf("畸形角色声明被当成公开工具放行：%v", visibleNames(got))
	}
	if tool, ok := reg.Lookup("malformed"); !ok || tool.Visible().Role != "" {
		t.Fatalf("畸形声明的可见 role 应为空串：%+v ok=%v", tool.Visible(), ok)
	}
}

// ---------- L-014 冻结口径：role 为单值字符串 ----------

func TestRegistry_VisibleRoleShapeFrozen(t *testing.T) {
	cases := []struct {
		name  string
		roles []string
		want  string
	}{
		{name: "未声明角色", roles: nil, want: ""},
		{name: "单角色原样", roles: []string{"admin"}, want: "admin"},
		{name: "多角色去重后字典序连接", roles: []string{"v2", "admin", "v2"}, want: "admin,v2"},
		{name: "空白条目被剔除", roles: []string{" ", "v1", ""}, want: "v1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tl := Tool{Name: "t", Params: stringParamSchema(), Summary: "s", RequiredRoles: tc.roles, Run: fakeRun("t")}
			if got := tl.Visible().Role; got != tc.want {
				t.Fatalf("Visible().Role = %q，want %q", got, tc.want)
			}
		})
	}

	// 映射不得篡改入参（同一工具两次映射逐字节相等，便于 HMAC 复算）。
	roles := []string{"v2", "admin"}
	tl := Tool{Name: "t", Params: stringParamSchema(), Summary: "s", RequiredRoles: roles, Run: fakeRun("t")}
	_ = tl.Visible()
	if strings.Join(roles, ",") != "v2,admin" {
		t.Fatalf("Visible() 篡改了 RequiredRoles 入参：%v", roles)
	}
	a, _ := json.Marshal(tl.Visible())
	b, _ := json.Marshal(tl.Visible())
	if !bytes.Equal(a, b) {
		t.Fatalf("同一工具两次映射结果不一致：%s vs %s", a, b)
	}
}

// ---------- 注册校验（半成品声明不入表） ----------

func TestRegistry_RegisterRejectsIncompleteDecl(t *testing.T) {
	if _, err := NewRegistry(Tool{Name: " ", Params: stringParamSchema(), Summary: "s", Run: fakeRun("x")}); !errors.Is(err, ErrToolNameRequired) {
		t.Fatalf("空工具名应返回 ErrToolNameRequired，got %v", err)
	}
	if _, err := NewRegistry(Tool{Name: "no-run", Params: stringParamSchema(), Summary: "s"}); !errors.Is(err, ErrToolRunRequired) {
		t.Fatalf("缺 Run 应返回 ErrToolRunRequired，got %v", err)
	}

	reg, err := NewRegistry(Tool{Name: "dup", Params: stringParamSchema(), Summary: "s", Run: fakeRun("dup")})
	if err != nil {
		t.Fatalf("首个注册失败：%v", err)
	}
	if err := reg.Register(Tool{Name: "dup", Params: stringParamSchema(), Summary: "s2", Run: fakeRun("dup2")}); !errors.Is(err, ErrToolDuplicate) {
		t.Fatalf("重复工具名应返回 ErrToolDuplicate，got %v", err)
	}
	// 重复注册被拒后，表内仍是首次注册的那一份（不被覆盖）。
	got, ok := reg.Lookup("dup")
	if !ok || got.Summary != "s" {
		t.Fatalf("重复注册后表内声明被改写：%+v ok=%v", got, ok)
	}
}

func TestRegistry_LookupUnknownToolRejected(t *testing.T) {
	reg := sampleRegistry(t)
	if _, ok := reg.Lookup("not.registered"); ok {
		t.Fatal("未注册工具不应被 Lookup 命中")
	}
	if _, ok := reg.Lookup(adminToolNames[0]); !ok {
		t.Fatal("已注册工具应被 Lookup 命中（执行侧拒绝未注册工具的判据）")
	}
	if reg.Len() != len(researchToolNames)+len(adminToolNames) {
		t.Fatalf("Len() = %d，want %d", reg.Len(), len(researchToolNames)+len(adminToolNames))
	}
}

// ---------- 确定性：注册顺序稳定、可复算 ----------

func TestRegistry_TrimDeterministicOrder(t *testing.T) {
	reg := sampleRegistry(t)

	first, err := json.Marshal(reg.VisibleByRole("admin"))
	if err != nil {
		t.Fatalf("序列化失败：%v", err)
	}
	for i := 0; i < 5; i++ {
		again, err := json.Marshal(reg.VisibleByRole("admin"))
		if err != nil {
			t.Fatalf("第 %d 次序列化失败：%v", i, err)
		}
		if !bytes.Equal(first, again) {
			t.Fatalf("第 %d 次裁剪结果与首次不一致\n%s\n%s", i, first, again)
		}
	}
	// 顺序 = 注册顺序，便于 prompt/HMAC 复算。
	if got := visibleNames(reg.VisibleByRole("admin")); strings.Join(got, ",") != strings.Join(append(append([]string{}, researchToolNames...), adminToolNames...), ",") {
		t.Fatalf("裁剪顺序不是注册顺序：%v", got)
	}
}

// ---------- 并发只读（ 的 errgroup 会并行读注册表） ----------

func TestRegistry_ConcurrentTrimAndLookup(t *testing.T) {
	reg := sampleRegistry(t)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				if len(reg.VisibleByRole("v1")) != len(researchToolNames) {
					t.Errorf("并发裁剪结果不稳定")
					return
				}
				if _, ok := reg.Lookup(adminToolNames[0]); !ok {
					t.Errorf("并发 Lookup 丢失已注册工具")
					return
				}
				_ = reg.Names()
			}
		}(i)
	}
	wg.Wait()
}
