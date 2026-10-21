package scene

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/malizhi2202-dev/agentkit/tools"
)

func toolReg(t *testing.T) *tools.Registry {
	t.Helper()
	reg, err := tools.NewRegistry()
	if err != nil {
		t.Fatalf("NewRegistry：%v", err)
	}
	for _, tc := range []struct {
		name  string
		roles []string
	}{
		{"user.profile", []string{"support", "admin"}},
		{"whoami", nil},
		{"report.build", []string{"admin"}},
	} {
		name := tc.name
		if err := reg.Register(tools.Tool{Name: name, Summary: name, RequiredRoles: tc.roles,
			Run: func(_ context.Context, _ map[string]any) (any, error) { return name, nil }}); err != nil {
			t.Fatalf("Register(%s)：%v", name, err)
		}
	}
	return reg
}

func TestScene_RegistryRejectsMalformed(t *testing.T) {
	for _, c := range []struct {
		name  string
		seeds []Scene
		want  error
	}{
		{"空表", nil, ErrNoScenes},
		{"缺 ID", []Scene{{Label: "x", Tools: []string{"a"}}}, ErrIDRequired},
		{"ID 只有空白", []Scene{{ID: "  ", Label: "x", Tools: []string{"a"}}}, ErrIDRequired},
		{"缺展示名", []Scene{{ID: "a", Tools: []string{"b"}}}, ErrLabelRequired},
		{"不引用工具", []Scene{{ID: "a", Label: "A", Tools: []string{"  ", ""}}}, ErrNoTools},
		{"ID 重复", []Scene{{ID: "a", Label: "A", Tools: []string{"t"}}, {ID: " a ", Label: "B", Tools: []string{"t"}}}, ErrDuplicateID},
	} {
		if _, err := NewRegistry(c.seeds...); !errors.Is(err, c.want) {
			t.Errorf("%s：应判 %v，实得 %v", c.name, c.want, err)
		}
	}
	r, err := NewRegistry(Scene{ID: " ip查 ", Label: " IP 查 ", Tools: []string{" user.profile ", "user.profile", "", "whoami"}})
	if err != nil {
		t.Fatalf("合法场景被拒：%v", err)
	}
	got, ok := r.Get("ip查")
	if !ok {
		t.Fatal("ID 未归一化")
	}
	if got.Label != "IP 查" || !reflect.DeepEqual(got.Tools, []string{"user.profile", "whoami"}) {
		t.Fatalf("归一化/去重结果不对：%+v", got)
	}
	if !got.Valid() {
		t.Fatal("合法场景应 Valid()")
	}
	if (Scene{ID: "a", Label: "b"}).Valid() || (Scene{ID: "", Label: "b", Tools: []string{"t"}}).Valid() {
		t.Fatal("Valid 放过畸形形状")
	}
}

func TestScene_ReadSurfaceIsCopyAndOrderStable(t *testing.T) {
	r, err := NewRegistry(
		Scene{ID: "zeta", Label: "Z", Tools: []string{"whoami"}},
		Scene{ID: "alpha", Label: "A", Tools: []string{"whoami"}},
	)
	if err != nil {
		t.Fatalf("NewRegistry：%v", err)
	}
	if ids := r.IDs(); !reflect.DeepEqual(ids, []string{"zeta", "alpha"}) {
		t.Fatalf("IDs 应按登记顺序（管理面展示要稳定）：%v", ids)
	}
	s, _ := r.Get("alpha")
	s.Tools[0] = "hijacked"
	again, _ := r.Get("alpha")
	if again.Tools[0] != "whoami" {
		t.Fatalf("Get 交出的是内部切片而非副本：%v", again.Tools)
	}
	ids := r.IDs()
	ids[0] = "hijacked"
	if r.IDs()[0] != "zeta" {
		t.Fatal("IDs 交出的是内部切片")
	}
	if r.Len() != 2 {
		t.Fatalf("Len 不对：%d", r.Len())
	}
	var nilReg *Registry
	if nilReg.Len() != 0 || nilReg.IDs() != nil {
		t.Fatal("nil 表的读取面应安全返回空")
	}
	if _, ok := nilReg.Get("x"); ok {
		t.Fatal("nil 表不该命中场景")
	}
	if _, err := nilReg.Resolve("x", "support", toolReg(t)); !errors.Is(err, ErrUnknownScene) {
		t.Fatalf("nil 表 Resolve 应判未知场景，实得 %v", err)
	}
}

func TestScene_ResolveTrimsByTheSinglePredicate(t *testing.T) {
	r, err := NewRegistry(Scene{ID: "triage", Label: "分诊",
		Tools: []string{"report.build", "user.profile", "whoami", "gone.tool"}})
	if err != nil {
		t.Fatalf("NewRegistry：%v", err)
	}
	reg := toolReg(t)

	// support：越权的 report.build、不存在的 gone.tool 被裁；顺序仍按场景声明序
	res, err := r.Resolve(" triage ", "support", reg)
	if err != nil {
		t.Fatalf("Resolve：%v", err)
	}
	if !reflect.DeepEqual(res.Tools, []string{"user.profile", "whoami"}) {
		t.Fatalf("support 的可用集不对：%v", res.Tools)
	}
	if len(res.Dropped) != 2 || res.Dropped[0].Reason != DropNotAuthorized || res.Dropped[1].Reason != DropUnknownTool {
		t.Fatalf("被裁工具必须逐条留原因：%+v", res.Dropped)
	}
	for _, d := range res.Dropped {
		if !d.Reason.Valid() {
			t.Fatalf("裁掉原因用了闭集外的值：%+v", d)
		}
	}
	// admin：三条都在
	res, _ = r.Resolve("triage", "admin", reg)
	if len(res.Tools) != 3 || res.Tools[0] != "report.build" {
		t.Fatalf("admin 的可用集不对：%v", res.Tools)
	}
	// 无权限角色：只剩无必需角色那条（与 tools.allows 同口径）
	res, _ = r.Resolve("triage", "auditor", reg)
	if !reflect.DeepEqual(res.Tools, []string{"whoami"}) {
		t.Fatalf("auditor 应只剩 whoami：%v", res.Tools)
	}
	// 角色没建立（空串）⇒ **一条都不剩**，连"无必需角色"的工具也不行：
	// tools.allows 要的是"任意**已判定**角色"，空串不是已判定角色。框架不猜默认角色就体现在这里。
	res, _ = r.Resolve("triage", "", reg)
	if len(res.Tools) != 0 {
		t.Fatalf("空角色不该有任何可用工具：%v", res.Tools)
	}
	if len(res.Dropped) != 4 || res.Dropped[0].Reason != DropNotAuthorized || res.Dropped[3].Reason != DropUnknownTool {
		t.Fatalf("空角色下的留痕不对：%+v", res.Dropped)
	}
	if res.Scene.ID != "triage" {
		t.Fatalf("解析结果应回带场景本体：%+v", res.Scene)
	}
	if _, err := r.Resolve("nope", "support", reg); !errors.Is(err, ErrUnknownScene) {
		t.Fatalf("未知场景应判 ErrUnknownScene，实得 %v", err)
	}
	if _, err := r.Resolve("triage", "support", nil); !errors.Is(err, ErrRegistryRequired) {
		t.Fatalf("缺工具表应判 ErrRegistryRequired，实得 %v", err)
	}
	kinds := DropKinds()
	if len(kinds) != 2 || kinds[0] != DropNotAuthorized || kinds[1] != DropUnknownTool {
		t.Fatalf("闭集不对：%v", kinds)
	}
	if DropReason("garbage").Valid() {
		t.Fatal("未知值被当作合法")
	}
}

func TestScene_MissingReportsDriftOnce(t *testing.T) {
	r, err := NewRegistry(
		Scene{ID: "a", Label: "A", Tools: []string{"user.profile", "gone.tool", "ghost.tool"}},
		Scene{ID: "b", Label: "B", Tools: []string{"gone.tool", "whoami"}},
	)
	if err != nil {
		t.Fatalf("NewRegistry：%v", err)
	}
	reg := toolReg(t)
	if got := r.Missing(reg); !reflect.DeepEqual(got, []string{"gone.tool", "ghost.tool"}) {
		t.Fatalf("Missing 应按出现顺序去重报告漂移：%v", got)
	}
	if got := r.Missing(nil); got != nil {
		t.Fatalf("没有工具表时不该假装能校验：%v", got)
	}
	clean, err := NewRegistry(Scene{ID: "a", Label: "A", Tools: []string{"user.profile", "whoami"}})
	if err != nil {
		t.Fatalf("NewRegistry：%v", err)
	}
	if got := clean.Missing(reg); len(got) != 0 {
		t.Fatalf("没有漂移却报了出来：%v", got)
	}
}
