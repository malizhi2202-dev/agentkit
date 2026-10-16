package semantic

import (
	"context"
	"errors"
	"strings"
	"testing"

	"agentkit/tools"
)

func bindRegistry(t *testing.T, runs *int) *tools.Registry {
	t.Helper()
	reg, err := tools.NewRegistry()
	if err != nil {
		t.Fatalf("NewRegistry：%v", err)
	}
	must := func(name string, roles []string) {
		t.Helper()
		if err := reg.Register(tools.Tool{
			Name: name, Summary: name + " 摘要", RequiredRoles: roles,
			Run: func(_ context.Context, _ map[string]any) (any, error) {
				if runs != nil {
					*runs++
				}
				return "ok", nil
			},
		}); err != nil {
			t.Fatalf("Register(%s)：%v", name, err)
		}
	}
	must("engine.profile", []string{"support", "admin"})
	must("report.build", nil) // 无必需角色 ⇒ 任意已判定角色可用
	return reg
}

func understood(entities []Entity, intents ...Intent) Understanding {
	return Understanding{Entities: entities, Intents: intents}
}

func TestSemantic_RuleValidation(t *testing.T) {
	ok := Rule{Intent: IntentCorrelationAnalysis, Tool: "engine.profile",
		FromEntity: map[string]string{"ip": "ip"}, Const: map[string]any{"scope": "platform"}}
	for _, c := range []struct {
		name  string
		rules []Rule
		want  error
	}{
		{"空规则", nil, ErrNoRules},
		{"缺诉求", []Rule{{Tool: "x"}}, ErrRuleIntentRequired},
		{"缺工具名", []Rule{{Intent: "a", Tool: "  "}}, ErrRuleToolRequired},
		{"实体映射键为空", []Rule{{Intent: "a", Tool: "x", FromEntity: map[string]string{"": "ip"}}}, ErrRuleArgKeyRequired},
		{"实体映射目标为空", []Rule{{Intent: "a", Tool: "x", FromEntity: map[string]string{"ip": " "}}}, ErrRuleArgKeyRequired},
		{"同一实体类型映射两个键", []Rule{{Intent: "a", Tool: "x", FromEntity: map[string]string{"ip": "ip", " ip ": "host"}}}, ErrRuleArgKeyRequired},
		{"常量键与实体目标重名", []Rule{{Intent: "a", Tool: "x", FromEntity: map[string]string{"ip": "ip"}, Const: map[string]any{"ip": 1}}}, ErrRuleConstConflict},
		{"常量键为空", []Rule{{Intent: "a", Tool: "x", Const: map[string]any{"": 1}}}, ErrRuleArgKeyRequired},
		{"重复 (Intent,Tool)", []Rule{ok, {Intent: IntentCorrelationAnalysis, Tool: " engine.profile "}}, ErrRuleDuplicate},
	} {
		if _, err := NewPlanner(c.rules...); !errors.Is(err, c.want) {
			t.Errorf("%s：应判 %v，实得 %v", c.name, c.want, err)
		}
	}
	rules := []Rule{{Intent: IntentCorrelationAnalysis, Tool: " engine.profile ",
		FromEntity: map[string]string{"ip": "addr"}, Const: map[string]any{"scope": "platform"}}}
	p, err := NewPlanner(rules...)
	if err != nil {
		t.Fatalf("合法规则被拒：%v", err)
	}
	if got := p.Rules()[0]; got.Tool != "engine.profile" {
		t.Fatalf("工具名未归一化：%q", got.Tool)
	}
	// 调用方改自己那份，不得影响已构造的绑定器（"装配期定形"必须是真的）
	rules[0].Tool = "hijacked"
	rules[0].FromEntity["ip"] = "hijacked"
	rules[0].Const["scope"] = "hijacked"
	got := p.Rules()[0]
	if got.Tool != "engine.profile" || got.FromEntity["ip"] != "addr" || got.Const["scope"] != "platform" {
		t.Fatalf("绑定器仍引用调用方的切片/映射：%+v", got)
	}
	got.FromEntity["ip"] = "mutated"
	if p.Rules()[0].FromEntity["ip"] != "addr" {
		t.Fatal("Rules() 交出的是内部映射而非副本")
	}
}

func TestSemantic_PlanGatesOnExecutability(t *testing.T) {
	p, err := NewPlanner(Rule{Intent: IntentCorrelationAnalysis, Tool: "engine.profile",
		FromEntity: map[string]string{"ip": "ip"}})
	if err != nil {
		t.Fatalf("NewPlanner：%v", err)
	}
	reg := bindRegistry(t, nil)
	ip := []Entity{{Type: "ip", Value: "1.2.3.4"}}

	// 有候选 ⇒ 整次不可执行，且只留一条全局 Skip（不摊成"每条规则各自不成立"）
	withCand := Understanding{Entities: ip, Intents: []Intent{IntentCorrelationAnalysis},
		Candidates: []Candidate{{Type: "ip", Value: "5.6.7.8"}, {Type: "host", Value: "h"}}}
	plan, err := p.Plan(withCand, "support", reg)
	if err != nil {
		t.Fatalf("Plan：%v", err)
	}
	if !plan.Empty() || len(plan.Skipped) != 1 {
		t.Fatalf("有候选时不该产出调用且只留一条事实：%+v", plan)
	}
	if plan.Skipped[0].Reason != SkipPendingConfirmation || plan.Skipped[0].Intent != "" ||
		!strings.Contains(plan.Skipped[0].Detail, "2 个") {
		t.Fatalf("全局判定落点不对：%+v", plan.Skipped[0])
	}
	// 不支持 ⇒ 另一条闭集值
	var u Understanding
	u.Unsupported = UnsupportedFor([]Mechanism{{Name: "engine.profile", Label: "领域对象档案"}})
	plan, err = p.Plan(u, "support", reg)
	if err != nil {
		t.Fatalf("Plan：%v", err)
	}
	if len(plan.Skipped) != 1 || plan.Skipped[0].Reason != SkipUnsupported {
		t.Fatalf("不支持应判 SkipUnsupported：%+v", plan.Skipped)
	}
	// 两者同时成立 ⇒ 候选优先（"等确认"是更具体的门，写进文档也写进断言）
	both := Understanding{Candidates: []Candidate{{Type: "ip", Value: "x"}},
		Unsupported: UnsupportedFor(nil)}
	plan, _ = p.Plan(both, "support", reg)
	if plan.Skipped[0].Reason != SkipPendingConfirmation {
		t.Fatalf("候选应优先于不支持：%+v", plan.Skipped[0])
	}
}

func TestSemantic_PlanAuthorizationIsTheOnlyPredicate(t *testing.T) {
	var runs int
	reg := bindRegistry(t, &runs)
	p, err := NewPlanner(
		Rule{Intent: IntentCorrelationAnalysis, Tool: "engine.profile", FromEntity: map[string]string{"ip": "ip"}},
		Rule{Intent: IntentPreliminaryReport, Tool: "report.build"},
		Rule{Intent: Intent("no.such.intent"), Tool: "engine.profile", FromEntity: map[string]string{"ip": "ip"}},
		Rule{Intent: IntentCorrelationAnalysis, Tool: "engine.gone", FromEntity: map[string]string{"ip": "ip"}},
	)
	if err != nil {
		t.Fatalf("NewPlanner：%v", err)
	}
	u := understood([]Entity{{Type: "ip", Value: "1.2.3.4"}}, IntentCorrelationAnalysis, IntentPreliminaryReport)

	plan, err := p.Plan(u, "support", reg)
	if err != nil {
		t.Fatalf("Plan：%v", err)
	}
	// 声明序：engine.profile(support 有权) → report.build(无必需角色) → 未知意图 → 未知工具
	if len(plan.Calls) != 2 || plan.Calls[0].Tool != "engine.profile" || plan.Calls[1].Tool != "report.build" {
		t.Fatalf("有权角色的计划不对：%+v", plan.Calls)
	}
	if len(plan.Skipped) != 2 || plan.Skipped[0].Reason != SkipNotRequested || plan.Skipped[1].Reason != SkipUnknownTool {
		t.Fatalf("诉求未命中/工具未登记应分别留痕：%+v", plan.Skipped)
	}
	if plan.Calls[1].Args != nil {
		t.Fatalf("无参工具不该造出空 map：%+v", plan.Calls[1])
	}
	if runs != 0 {
		t.Fatalf("Plan 执行了 %d 次工具（它只该选择与装配）", runs)
	}

	// 越权角色：工具存在但不授权 ⇒ not_authorized（判定完全由 tools 那一条谓词给出）
	plan, _ = p.Plan(u, "auditor", reg)
	if len(plan.Calls) != 1 || plan.Calls[0].Tool != "report.build" {
		t.Fatalf("越权角色仍拿到了受限工具：%+v", plan.Calls)
	}
	found := false
	for _, s := range plan.Skipped {
		if s.Reason == SkipNotAuthorized && s.Tool == "engine.profile" {
			found = true
		}
	}
	if !found {
		t.Fatalf("越权应留 not_authorized 事实：%+v", plan.Skipped)
	}
	// 空角色（身份没建立）⇒ **一条调用都不生成**，连"无必需角色"的工具也不行：
	// tools.allows 要的是"任意**已判定**角色"，空串不是已判定角色。框架不猜默认角色就在这里体现。
	plan, _ = p.Plan(u, "", reg)
	if !plan.Empty() {
		t.Fatalf("空角色不该有任何调用：%+v", plan.Calls)
	}
	if runs != 0 {
		t.Fatal("工具被执行了")
	}
}

func TestSemantic_PlanAssemblesArgsFromEntities(t *testing.T) {
	p, err := NewPlanner(Rule{Intent: IntentCorrelationAnalysis, Tool: "engine.profile",
		FromEntity: map[string]string{"ip": "addr", "hostgroup": "group"},
		Const:      map[string]any{"scope": "platform", "limit": 3}})
	if err != nil {
		t.Fatalf("NewPlanner：%v", err)
	}
	reg := bindRegistry(t, nil)
	plan, err := p.Plan(understood([]Entity{{Type: "hostgroup", Value: "机房C"}, {Type: "ip", Value: "1.2.3.4"}},
		IntentCorrelationAnalysis), "support", reg)
	if err != nil {
		t.Fatalf("Plan：%v", err)
	}
	if len(plan.Calls) != 1 {
		t.Fatalf("应产出一条调用：%+v", plan)
	}
	args := plan.Calls[0].Args
	if args["addr"] != "1.2.3.4" || args["group"] != "机房C" || args["scope"] != "platform" || args["limit"] != 3 {
		t.Fatalf("参数装配不对：%v", args)
	}
	// 缺实体 ⇒ 留痕并指名缺哪个类型，绝不塞零值（下游拿零值去查就是假结论）
	plan, _ = p.Plan(understood([]Entity{{Type: "ip", Value: "1.2.3.4"}}, IntentCorrelationAnalysis), "support", reg)
	if !plan.Empty() {
		t.Fatalf("缺 hostgroup 却产出了调用：%+v", plan.Calls)
	}
	if plan.Skipped[0].Reason != SkipMissingEntity || !strings.Contains(plan.Skipped[0].Detail, "hostgroup") {
		t.Fatalf("缺实体的留痕不对：%+v", plan.Skipped[0])
	}
	if strings.Contains(plan.Skipped[0].Detail, "ip") {
		t.Fatalf("不存在的实体不该被报成缺失：%s", plan.Skipped[0].Detail)
	}
	// 同类型多实体：取顺序上的第一个（歧义本该在语义层落进 Candidates）
	// 同类型多实体：取顺序上第一个（上面那个 planner 还要求 hostgroup 实体，这里单造一个只要 ip 的）
	only, err := NewPlanner(Rule{Intent: IntentCorrelationAnalysis, Tool: "engine.profile",
		FromEntity: map[string]string{"ip": "addr"}})
	if err != nil {
		t.Fatalf("NewPlanner：%v", err)
	}
	multi, err := only.Plan(understood([]Entity{{Type: "ip", Value: "first"}, {Type: "ip", Value: "second"}},
		IntentCorrelationAnalysis), "support", reg)
	if err != nil {
		t.Fatalf("Plan：%v", err)
	}
	if len(multi.Calls) != 1 || multi.Calls[0].Args["addr"] != "first" {
		t.Fatalf("同类型多实体应取顺序上第一个：%+v", multi.Calls)
	}
}

func TestSemantic_PlanIsReplayable(t *testing.T) {
	p, err := NewPlanner(
		Rule{Intent: IntentCorrelationAnalysis, Tool: "engine.profile",
			FromEntity: map[string]string{"ip": "addr", "hostgroup": "group"},
			Const:      map[string]any{"scope": "platform", "limit": 3, "mode": "fast"}},
		Rule{Intent: IntentPreliminaryReport, Tool: "report.build"},
	)
	if err != nil {
		t.Fatalf("NewPlanner：%v", err)
	}
	reg := bindRegistry(t, nil)
	u := understood([]Entity{{Type: "ip", Value: "1.2.3.4"}, {Type: "hostgroup", Value: "机房C"}},
		IntentCorrelationAnalysis, IntentPreliminaryReport)
	first, err := p.Plan(u, "support", reg)
	if err != nil {
		t.Fatalf("Plan：%v", err)
	}
	want, err := first.JSON()
	if err != nil {
		t.Fatalf("JSON：%v", err)
	}
	for i := 0; i < 60; i++ {
		got, err := p.Plan(u, "support", reg)
		if err != nil {
			t.Fatalf("Plan：%v", err)
		}
		b, err := got.JSON()
		if err != nil {
			t.Fatalf("JSON：%v", err)
		}
		if b != want {
			t.Fatalf("第 %d 次回放字节不同：\n  %s\n  %s", i, want, b)
		}
	}
	// 字段名与结构本身也是契约（下游按它落审计/回放）
	for _, frag := range []string{`"calls":[{`, `"intent":"correlation_analysis"`, `"tool":"engine.profile"`,
		`"args":{"addr":"1.2.3.4"`, `"group":"机房C"`, `"limit":3`, `"mode":"fast"`, `"scope":"platform"`,
		`"tool":"report.build"`} {
		if !strings.Contains(want, frag) {
			t.Errorf("回放 JSON 缺片段 %s：%s", frag, want)
		}
	}
	if strings.Contains(want, `"args":null`) {
		t.Errorf("无参调用不该出现 args:null：%s", want)
	}
}

func TestSemantic_SkipReasonClosedSet(t *testing.T) {
	kinds := SkipKinds()
	if len(kinds) != 6 {
		t.Fatalf("闭集应 6 项：%v", kinds)
	}
	for i := 1; i < len(kinds); i++ {
		if kinds[i-1] >= kinds[i] {
			t.Fatalf("SkipKinds 未排序：%v", kinds)
		}
	}
	for _, k := range kinds {
		if !k.Valid() {
			t.Errorf("%s 未在闭集内", k)
		}
	}
	for _, k := range []SkipReason{"", "garbage", "pending", "PENDING_CONFIRMATION"} {
		if k.Valid() {
			t.Errorf("未知值 %q 被当作合法", k)
		}
	}
	if got := SkipReason("").String(); got != "unspecified" {
		t.Fatalf("零值原因应有可读串，实得 %q", got)
	}
	if got := SkipNotAuthorized.String(); got != "not_authorized" {
		t.Fatalf("String 应等于值本身，实得 %q", got)
	}
}

func TestSemantic_PlanRejectsMissingInputs(t *testing.T) {
	p, err := NewPlanner(Rule{Intent: IntentCorrelationAnalysis, Tool: "engine.profile"})
	if err != nil {
		t.Fatalf("NewPlanner：%v", err)
	}
	if _, err := p.Plan(understood(nil, IntentCorrelationAnalysis), "support", nil); !errors.Is(err, ErrRegistryRequired) {
		t.Fatalf("缺工具表应判 ErrRegistryRequired，实得 %v", err)
	}
	var nilPlanner *Planner
	if got, err := nilPlanner.Plan(understood(nil, IntentCorrelationAnalysis), "support", bindRegistry(t, nil)); !errors.Is(err, ErrNoRules) {
		t.Fatalf("nil 绑定器应判 ErrNoRules，实得 %v（plan=%+v）", err, got)
	}
	if nilPlanner.Rules() != nil {
		t.Fatal("nil 绑定器 Rules 应为 nil")
	}
	if !(Plan{}).Empty() {
		t.Fatal("零值 Plan 应为空")
	}
}

// TestSemantic_PlanDoesNotInventRoles 是"框架不造身份"的另一半：
// 角色只能通过参数交进来，绑定层自己不看 ctx、不读环境变量、不猜默认。
func TestSemantic_PlanRoleIsJustAString(t *testing.T) {
	p, err := NewPlanner(Rule{Intent: IntentCorrelationAnalysis, Tool: "engine.profile",
		FromEntity: map[string]string{"ip": "ip"}})
	if err != nil {
		t.Fatalf("NewPlanner：%v", err)
	}
	reg := bindRegistry(t, nil)
	u := understood([]Entity{{Type: "ip", Value: "1.2.3.4"}}, IntentCorrelationAnalysis)
	for _, role := range []string{"support", "admin", "auditor", "", "  ", "R2"} {
		plan, err := p.Plan(u, role, reg)
		if err != nil {
			t.Fatalf("Plan(%q)：%v", role, err)
		}
		switch role {
		case "support", "admin":
			if len(plan.Calls) != 1 {
				t.Fatalf("角色 %q 应有调用：%+v", role, plan)
			}
		default:
			if !plan.Empty() {
				t.Fatalf("角色 %q 不该有调用（大小写不同的 R2 也不是同一个角色）：%+v", role, plan.Calls)
			}
		}
	}
}
