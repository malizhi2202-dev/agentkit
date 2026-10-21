package preset

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/malizhi2202-dev/agentkit/kit"
	"github.com/malizhi2202-dev/agentkit/scene"
	"github.com/malizhi2202-dev/agentkit/web"
)

// sceneTools 用 P5 的同源声明造两条接口/工具（engine.ask 与 engine.other，同一权限点），
// 这样"场景引用的工具"和"模型可见的工具"是同一批事实，不需要在测试里再维护一份角色表。
func sceneTools(t *testing.T) []ToolSource {
	t.Helper()
	var out []ToolSource
	for _, id := range []string{"engine.ask", "engine.other"} {
		src, err := Served(web.Route{Method: "POST", Path: "/engine/" + id, Name: id}, askIn{}, "engine.ask", askHandler(nil))
		if err != nil {
			t.Fatalf("Served(%s)：%v", id, err)
		}
		out = append(out, src)
	}
	return out
}

func TestPreset_ScenesRejectsDriftAtBuild(t *testing.T) {
	if _, err := NewScenes(nil); !errors.Is(err, ErrScenesRequired) {
		t.Fatalf("nil 表应在构造期被拒，实得 %v", err)
	}
	zero := &Scenes{}
	if err := zero.Apply(nil); !errors.Is(err, ErrScenesRequired) {
		t.Fatalf("零值 Apply 应被拒，实得 %v", err)
	}
	if _, err := zero.Resolve(context.Background(), "x"); !errors.Is(err, ErrScenesRequired) {
		t.Fatalf("零值 Resolve 应被拒，实得 %v", err)
	}
	if zero.Name() != NameScenes || zero.Describe().Notes["scenes"] != "0" {
		t.Fatal("零值读取面不该 panic")
	}

	// 引用了不存在的工具 ⇒ Build 失败（而不是运行时静默少一个工具）
	bad, err := scene.NewRegistry(scene.Scene{ID: "triage", Label: "分诊",
		Tools: []string{"engine.ask", "engine.gone-one", "engine.gone-two"}})
	if err != nil {
		t.Fatalf("NewRegistry：%v", err)
	}
	ps, err := NewScenes(bad)
	if err != nil {
		t.Fatalf("NewScenes：%v", err)
	}
	rt := web.NewRouter(web.Config{})
	k, err := kit.New(kit.Config{Web: rt})
	if err != nil {
		t.Fatalf("kit.New：%v", err)
	}
	if err := k.Use(ps, NewTools(servedPerms(t), sceneTools(t)...), servedPerms(t)); err != nil {
		t.Fatalf("Use：%v", err)
	}
	buildErr := k.Build()
	if !errors.Is(buildErr, ErrScenesToolsMissing) {
		t.Fatalf("漂移应在 Build 期失败，实得 %v", buildErr)
	}
	for _, frag := range []string{"engine.gone-one", "engine.gone-two"} {
		if !strings.Contains(buildErr.Error(), frag) {
			t.Fatalf("失败原因必须点名漂移的工具 %s，实得：%v", frag, buildErr)
		}
	}
	// Apply 相失败会走 kit.rollback：已 Applied 的插件撤销自己的运行时（服务/循环），
	// 而**没装配成功的那个插件必须拒绝继续回答**（不是拿着半份状态继续服务）。
	if _, err := ps.Resolve(withRole("support"), "triage"); !errors.Is(err, ErrScenesNotAssembled) {
		t.Fatalf("Build 失败后场景解析应判未装配，实得 %v", err)
	}
	// 宿主自己递进来的路由表不在框架的撤销范围内（那是宿主的对象）：
	// 因此"能不能对外服务"由 Kit 决定，而不是由表里有没有条目决定 —— 这里刻意不假设它被清空。
	if n := len(rt.Routes()); n != 2 {
		t.Fatalf("同源声明在 Declare 相就登记了路由（本次失败发生在 Apply 相），实得 %d 条", n)
	}
}

func TestPreset_ScenesResolveByRoleAndStop(t *testing.T) {
	reg, err := scene.NewRegistry(
		scene.Scene{ID: "triage", Label: "分诊", Tools: []string{"engine.ask", "engine.other"}},
	)
	if err != nil {
		t.Fatalf("NewRegistry：%v", err)
	}
	ps, err := NewScenes(reg)
	if err != nil {
		t.Fatalf("NewScenes：%v", err)
	}
	if info := ps.Describe(); info.Notes["scenes"] != "1" || info.Notes["ids"] != "triage" {
		t.Fatalf("内省信息不对：%+v", info)
	}
	if _, err := ps.Resolve(withRole("support"), "triage"); !errors.Is(err, ErrScenesNotAssembled) {
		t.Fatalf("Build 前应判未装配，实得 %v", err)
	}
	rt := web.NewRouter(web.Config{})
	k, err := kit.New(kit.Config{Web: rt})
	if err != nil {
		t.Fatalf("kit.New：%v", err)
	}
	if err := k.Use(ps, NewTools(servedPerms(t), sceneTools(t)...), servedPerms(t)); err != nil {
		t.Fatalf("Use：%v", err)
	}
	if err := k.Build(); err != nil {
		t.Fatalf("Build：%v", err)
	}
	res, err := ps.Resolve(withRole("support"), "triage")
	if err != nil {
		t.Fatalf("Resolve：%v", err)
	}
	if !reflect.DeepEqual(res.Tools, []string{"engine.ask", "engine.other"}) || len(res.Dropped) != 0 {
		t.Fatalf("有权角色应拿到整场景：%+v", res)
	}
	// 无权限角色：整批被裁，且每条都有原因（不是"变短了就完事"）
	res, err = ps.Resolve(withRole("auditor"), "triage")
	if err != nil {
		t.Fatalf("Resolve：%v", err)
	}
	if len(res.Tools) != 0 || len(res.Dropped) != 2 {
		t.Fatalf("无权限角色的解析不对：%+v", res)
	}
	for _, d := range res.Dropped {
		if d.Reason != scene.DropNotAuthorized {
			t.Fatalf("裁剪原因不对：%+v", d)
		}
	}
	// 身份没建立：同样整批裁掉（框架不猜默认角色）
	if res, err = ps.Resolve(context.Background(), "triage"); err != nil || len(res.Tools) != 0 {
		t.Fatalf("无身份却解析出了可用工具：%+v %v", res.Tools, err)
	}
	if _, err := ps.Resolve(withRole("support"), "nope"); !errors.Is(err, scene.ErrUnknownScene) {
		t.Fatalf("未知场景应判 ErrUnknownScene，实得 %v", err)
	}
	if err := k.Stop(); err != nil {
		t.Fatalf("Stop：%v", err)
	}
	if _, err := ps.Resolve(withRole("support"), "triage"); !errors.Is(err, ErrScenesStopped) {
		t.Fatalf("停止后应拒绝服务，实得 %v", err)
	}
}
