package preset

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/malizhi2202-dev/agentkit/kit"
	"github.com/malizhi2202-dev/agentkit/kitctx"
	"github.com/malizhi2202-dev/agentkit/semantic"
	"github.com/malizhi2202-dev/agentkit/web"
)

func ipRule(tool string) semantic.Rule {
	// 实体类型 ip → 参数键 q：这个键名必须与 Served 生成的 schema 对得上，
	// 否则工具边界会以"缺必填字段"拒掉 —— 两处的同源关系就是被这条断言钉住的。
	return semantic.Rule{Intent: semantic.IntentCorrelationAnalysis, Tool: tool,
		FromEntity: map[string]string{"ip": "q"}}
}

func TestPreset_PlannerLifecycleGates(t *testing.T) {
	if _, err := NewPlanner(semantic.Rule{Tool: "x"}); !errors.Is(err, semantic.ErrRuleIntentRequired) {
		t.Fatalf("畸形规则应在构造期被拒，实得 %v", err)
	}
	zero := &Planner{}
	if err := zero.Apply(nil); !errors.Is(err, ErrPlannerRulesRequired) {
		t.Fatalf("零值绑定器 Apply 应被拒，实得 %v", err)
	}
	if _, err := zero.Plan(context.Background(), semantic.Understanding{}); !errors.Is(err, ErrPlannerRulesRequired) {
		t.Fatalf("零值绑定器 Plan 应被拒，实得 %v", err)
	}
	if zero.Rules() != nil || zero.Name() != NamePlanner {
		t.Fatal("零值绑定器的读取面不该 panic")
	}

	src, err := Served(web.Route{Method: "POST", Path: "/engine/ask", Name: "engine.ask"},
		askIn{}, "engine.ask", askHandler(nil))
	if err != nil {
		t.Fatalf("Served：%v", err)
	}
	pl, err := NewPlanner(ipRule("engine.ask"))
	if err != nil {
		t.Fatalf("NewPlanner：%v", err)
	}
	if got := pl.Name(); got != NamePlanner {
		t.Fatalf("插件名不对：%q", got)
	}
	if info := pl.Describe(); info.Notes["rules"] != "1" {
		t.Fatalf("内省信息缺规则数：%+v", info)
	}
	// Apply 之前不能用（它要的是 Kit 那张唯一工具表）
	if _, err := pl.Plan(context.Background(), semantic.Understanding{}); !errors.Is(err, ErrPlannerNotAssembled) {
		t.Fatalf("未装配应判 ErrPlannerNotAssembled，实得 %v", err)
	}
	rt := web.NewRouter(web.Config{})
	k, err := kit.New(kit.Config{Web: rt})
	if err != nil {
		t.Fatalf("kit.New：%v", err)
	}
	if err := k.Use(pl, NewTools(servedPerms(t), src), servedPerms(t)); err != nil {
		t.Fatalf("Use：%v", err)
	}
	if err := k.Build(); err != nil {
		t.Fatalf("Build：%v", err)
	}
	ctx := kitctx.WithRequest(context.Background(), kitctx.RequestContext{UserID: "u", Role: "support", TraceID: "tr"})
	plan, err := pl.Plan(ctx, semantic.Understanding{
		Entities: []semantic.Entity{{Type: "ip", Value: "1.2.3.4"}},
		Intents:  []semantic.Intent{semantic.IntentCorrelationAnalysis},
	})
	if err != nil {
		t.Fatalf("Plan：%v", err)
	}
	if len(plan.Calls) != 1 {
		t.Fatalf("应产出一条调用：%+v", plan)
	}
	if err := k.Stop(); err != nil {
		t.Fatalf("Stop：%v", err)
	}
	if _, err := pl.Plan(ctx, semantic.Understanding{}); !errors.Is(err, ErrPlannerStopped) {
		t.Fatalf("停止后应拒绝产出新计划，实得 %v", err)
	}
}

// TestPreset_PlannerDrivesServedRoute 是 P5+P6 的合拢判据：
// 一句话 → 语义理解 → 绑定计划 → 执行 P5 那条"接口即工具" → 接口的响应回到调用方。
// 全程不经网络、不再写第二份权限判定、也不允许"越权角色选中同一个接口"。
func TestPreset_PlannerDrivesServedRoute(t *testing.T) {
	var calls int
	handler := func(w http.ResponseWriter, r *http.Request) error {
		var in askIn
		if err := web.DecodeJSON(r, &in); err != nil {
			return web.Errorf(web.ErrKindInvalidInput, "参数无法解码")
		}
		if in.Q == "" {
			return web.Errorf(web.ErrKindInvalidInput, "缺少 q")
		}
		calls++
		web.WriteJSON(w, map[string]string{"answer": in.Q + " 属于 demo-profile-07（机房C）"})
		return nil
	}
	src, err := Served(web.Route{Method: "POST", Path: "/engine/profile", Name: "engine.profile"},
		askIn{}, "engine.ask", handler)
	if err != nil {
		t.Fatalf("Served：%v", err)
	}
	pl, err := NewPlanner(ipRule("engine.profile"))
	if err != nil {
		t.Fatalf("NewPlanner：%v", err)
	}
	rt := web.NewRouter(web.Config{})
	k, err := kit.New(kit.Config{Web: rt})
	if err != nil {
		t.Fatalf("kit.New：%v", err)
	}
	if err := k.Use(pl, NewTools(servedPerms(t), src), servedPerms(t)); err != nil {
		t.Fatalf("Use：%v", err)
	}
	if err := k.Build(); err != nil {
		t.Fatalf("Build：%v", err)
	}

	u := semantic.Understanding{
		Entities: []semantic.Entity{{Type: "ip", Value: "1.2.3.4"}},
		Intents:  []semantic.Intent{semantic.IntentCorrelationAnalysis},
	}
	run := func(ctx context.Context) (semantic.Plan, string) {
		t.Helper()
		plan, err := pl.Plan(ctx, u)
		if err != nil {
			t.Fatalf("Plan：%v", err)
		}
		var out string
		for _, c := range plan.Calls {
			tool, ok := k.Tools().Lookup(c.Tool)
			if !ok {
				t.Fatalf("计划里的工具 %q 不在表里", c.Tool)
			}
			res, err := tool.Run(ctx, c.Args)
			if err != nil {
				// 越权/缺参数等在这里都会以 error 回来；本用例只跑有权角色
				t.Fatalf("Run(%s)：%v", c.Tool, err)
			}
			b, err := json.Marshal(res)
			if err != nil {
				t.Fatalf("Marshal：%v", err)
			}
			out = string(b)
		}
		return plan, out
	}

	// ① 有权角色：一句话最终变成"接口被执行一次 + 响应回到调用方"
	plan, got := run(withRole("support"))
	if calls != 1 {
		t.Fatalf("接口应被调用 1 次，实得 %d", calls)
	}
	if !strings.Contains(got, "demo-profile-07") || !strings.Contains(got, "1.2.3.4") {
		t.Fatalf("接口响应没回到调用方：%q", got)
	}
	if len(plan.Calls) != 1 || plan.Calls[0].Intent != semantic.IntentCorrelationAnalysis {
		t.Fatalf("计划形状不对：%+v", plan.Calls)
	}
	// 计划可回放（同一输入两次字节相同），这是  轨迹口径的前提
	a, err := plan.JSON()
	if err != nil {
		t.Fatalf("JSON：%v", err)
	}
	again, err := pl.Plan(withRole("support"), u)
	if err != nil {
		t.Fatalf("Plan：%v", err)
	}
	if b, _ := again.JSON(); a != b {
		t.Fatalf("同一输入两次计划字节不同：\n %s\n %s", a, b)
	}

	// ② 越权角色：PLAN 给 P6 标的那条风险 —— 语义选中了，执行侧仍必须拒
	calls = 0
	plan, _ = run(withRole("auditor"))
	if calls != 0 {
		t.Fatalf("越权角色却执行了接口（calls=%d）", calls)
	}
	if !plan.Empty() {
		t.Fatalf("越权角色不该有调用：%+v", plan.Calls)
	}
	if len(plan.Skipped) != 1 || plan.Skipped[0].Reason != semantic.SkipNotAuthorized {
		t.Fatalf("越权必须留痕：%+v", plan.Skipped)
	}

	// ③ 身份根本没建立（ctx 无 role）：与越权同一个落点，不猜默认角色
	calls = 0
	plan, _ = run(context.Background())
	if calls != 0 || !plan.Empty() {
		t.Fatalf("无身份却产出了执行：%+v calls=%d", plan, calls)
	}
	if plan.Skipped[0].Reason != semantic.SkipNotAuthorized {
		t.Fatalf("无身份应留 not_authorized 事实：%+v", plan.Skipped)
	}

	// ④ 有待确认候选 ⇒ 语义层不替用户挑一个，接口一次都不跑
	calls = 0
	cand := semantic.Understanding{
		Entities:   u.Entities,
		Intents:    u.Intents,
		Candidates: []semantic.Candidate{{Type: "ip", Value: "9.9.9.9"}},
	}
	plan, err = pl.Plan(withRole("support"), cand)
	if err != nil {
		t.Fatalf("Plan：%v", err)
	}
	if calls != 0 || !plan.Empty() || plan.Skipped[0].Reason != semantic.SkipPendingConfirmation {
		t.Fatalf("有候选时不该执行任何东西：%+v", plan)
	}

	if err := k.Stop(); err != nil {
		t.Fatalf("Stop：%v", err)
	}
}

func withRole(role kitctx.Role) context.Context {
	return kitctx.WithRequest(context.Background(), kitctx.RequestContext{
		UserID: "u-planner", Role: role, TraceID: "tr-planner",
		Limits: kitctx.Limits{MaxSteps: 4, StepTimeout: 2 * time.Second},
	})
}
