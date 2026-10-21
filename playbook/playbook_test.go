package playbook

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/malizhi2202-dev/agentkit/kitctx"
	"github.com/malizhi2202-dev/agentkit/tools"
)

// recorder 记录每个工具被调用的顺序，并可让指定工具失败。
type recorder struct {
	order   []string
	failOn  map[string]error
	allowed map[string][]string
}

func (rc *recorder) registry(t *testing.T) *tools.Registry {
	t.Helper()
	reg, err := tools.NewRegistry()
	if err != nil {
		t.Fatalf("NewRegistry：%v", err)
	}
	for _, name := range rc.names() {
		n := name
		if err := reg.Register(tools.Tool{Name: n, Summary: n, RequiredRoles: rc.allowed[n],
			Run: func(_ context.Context, _ map[string]any) (any, error) {
				rc.order = append(rc.order, n)
				if err := rc.failOn[n]; err != nil {
					return nil, err
				}
				return map[string]string{"ran": n}, nil
			}}); err != nil {
			t.Fatalf("Register(%s)：%v", n, err)
		}
	}
	return reg
}

func (rc *recorder) names() []string {
	seen := map[string]bool{}
	var out []string
	for n := range rc.allowed {
		if !seen[n] {
			out = append(out, n)
			seen[n] = true
		}
	}
	for n := range rc.failOn {
		if !seen[n] {
			out = append(out, n)
			seen[n] = true
		}
	}
	return out
}

func newRecorder() *recorder {
	return &recorder{failOn: map[string]error{}, allowed: map[string][]string{
		"s1": {"support"}, "s2": {"support"}, "s3": {"support"},
		"c1": {"support"}, "c2": {"support"}, "c-locked": {"admin"},
	}}
}

func comp(name, tool string) *Step {
	return &Step{Name: name, Tool: tool}
}

func TestPlaybook_ConstructionRejects(t *testing.T) {
	for _, c := range []struct {
		name  string
		id    string
		label string
		steps []Step
		want  error
	}{
		{"缺 ID", "", "L", []Step{{Name: "a", Tool: "t"}}, ErrIDRequired},
		{"缺展示名", "pb", "  ", []Step{{Name: "a", Tool: "t"}}, ErrLabelRequired},
		{"没有步骤", "pb", "L", nil, ErrNoSteps},
		{"步骤缺名", "pb", "L", []Step{{Tool: "t"}}, ErrStepNameRequired},
		{"步骤缺工具", "pb", "L", []Step{{Name: "a"}}, ErrStepToolRequired},
		{"步骤名重复", "pb", "L", []Step{{Name: "a", Tool: "t"}, {Name: " a ", Tool: "u"}}, ErrDuplicateStep},
		{"补偿步骤缺工具", "pb", "L", []Step{{Name: "a", Tool: "t", Compensation: &Step{Name: "ca"}}}, ErrStepToolRequired},
		{"补偿步骤缺名", "pb", "L", []Step{{Name: "a", Tool: "t", Compensation: &Step{Tool: "c"}}}, ErrStepNameRequired},
	} {
		if _, err := New(c.id, c.label, c.steps...); !errors.Is(err, c.want) {
			t.Errorf("%s：应判 %v，实得 %v", c.name, c.want, err)
		}
	}
	steps := []Step{{Name: "a", Tool: "t", Args: map[string]any{"k": "v"}, Compensation: comp("ca", "ct")}}
	pb, err := New(" pb ", " 剧本 ", steps...)
	if err != nil {
		t.Fatalf("合法剧本被拒：%v", err)
	}
	if pb.ID != "pb" || pb.Label != "剧本" || pb.Steps[0].Name != "a" {
		t.Fatalf("归一化未生效：%+v", pb)
	}
	// 调用方之后改自己那份，不得影响已构造的剧本（否则幂等键与执行会跟着漂）
	steps[0].Tool = "hijacked"
	steps[0].Args["k"] = "hijacked"
	steps[0].Compensation.Tool = "hijacked"
	if pb.Steps[0].Tool != "t" || pb.Steps[0].Args["k"] != "v" || pb.Steps[0].Compensation.Tool != "ct" {
		t.Fatalf("剧本仍引用调用方的切片/映射：%+v", pb.Steps[0])
	}
}

func TestPlaybook_DryRunIsZeroWrite(t *testing.T) {
	rc := newRecorder()
	reg := rc.registry(t)
	pb, err := New("pb", "剧本",
		Step{Name: "one", Tool: "s1", Args: map[string]any{"a": 1}},
		Step{Name: "two", Tool: "s2", Compensation: comp("two-c", "c1")},
		Step{Name: "three", Tool: "s3"})
	if err != nil {
		t.Fatalf("New：%v", err)
	}
	steps, problems, err := pb.DryRun("support", reg)
	if err != nil {
		t.Fatalf("DryRun：%v", err)
	}
	if len(problems) != 0 {
		t.Fatalf("有权角色不该有障碍：%+v", problems)
	}
	if len(steps) != 3 || steps[0].Name != "one" || steps[2].Name != "three" {
		t.Fatalf("预演结果顺序不对：%+v", steps)
	}
	if len(rc.order) != 0 {
		t.Fatalf("DryRun 执行了工具（零写入判据破了）：%v", rc.order)
	}
	// 预演必须把确定性可发现的问题提前报出来，而不是让执行去撞
	rc2 := newRecorder()
	reg2 := rc2.registry(t)
	bad, err := New("bad", "坏剧本",
		Step{Name: "one", Tool: "s1"},
		Step{Name: "ghost", Tool: "no.such.tool"},
		Step{Name: "locked", Tool: "c-locked", Compensation: comp("x", "s2")})
	if err != nil {
		t.Fatalf("New：%v", err)
	}
	steps, problems, err = bad.DryRun("support", reg2)
	if err != nil {
		t.Fatalf("DryRun：%v", err)
	}
	if steps != nil {
		t.Fatal("有障碍时不该给出可执行清单")
	}
	if len(problems) != 2 || problems[0].Kind != ProblemUnknownTool || problems[1].Kind != ProblemNotAuthorized {
		t.Fatalf("障碍分类不对：%+v", problems)
	}
	for _, pr := range problems {
		if !pr.Kind.Valid() || pr.Step == "" || pr.Tool == "" {
			t.Fatalf("障碍事实不完整：%+v", pr)
		}
	}
	// Execute 也必须先预演：有障碍 ⇒ 一个工具都不调
	out, err := bad.Execute(context.Background(), "support", reg2, nil)
	if err != nil {
		t.Fatalf("Execute：%v", err)
	}
	if len(rc2.order) != 0 || out.OK() || out.Failed == nil || len(out.Problems) != 2 {
		t.Fatalf("有障碍的执行应整批停在最前面：%+v order=%v", out, rc2.order)
	}
	// 空角色（身份没建立）⇒ 全部 not_authorized：步骤与**补偿步骤**过的是同一条谓词（框架不猜默认角色）
	if _, problems, _ := pb.DryRun("", reg); len(problems) != 4 {
		t.Fatalf("空角色应连补偿步骤一起被拒：%+v", problems)
	}
	if _, _, err := pb.DryRun("support", nil); !errors.Is(err, ErrRegistryRequired) {
		t.Fatalf("缺工具表应判 ErrRegistryRequired，实得 %v", err)
	}
	var nilPB *Playbook
	if _, err := nilPB.Validate("support", reg); !errors.Is(err, ErrNoSteps) {
		t.Fatalf("nil 剧本应判 ErrNoSteps，实得 %v", err)
	}
}

func TestPlaybook_ExecuteStopsAndCompensatesInReverse(t *testing.T) {
	rc := newRecorder()
	rc.failOn["s2"] = errors.New("第三步失败了")
	reg := rc.registry(t)
	pb, err := New("pb", "剧本",
		Step{Name: "one", Tool: "s1", Compensation: comp("c-one", "c1")},
		Step{Name: "two", Tool: "s2", Compensation: comp("c-two", "c1")},
		Step{Name: "three", Tool: "s3", Compensation: comp("c-three", "c1")})
	if err != nil {
		t.Fatalf("New：%v", err)
	}
	out, err := pb.Execute(withRole("support"), "support", reg, nil)
	if err != nil {
		t.Fatalf("Execute：%v", err)
	}
	if !reflect.DeepEqual(rc.order, []string{"s1", "s2", "c1"}) {
		t.Fatalf("执行/补偿顺序应为 [第一步, 失败步, 逆序补偿]：%v", rc.order)
	}
	if len(out.Completed) != 1 || out.Completed[0].Step != "one" {
		t.Fatalf("已完成步骤不对：%+v", out.Completed)
	}
	if out.Failed == nil || out.Failed.Step != "two" || !strings.Contains(out.Failed.Err, "第三步失败了") {
		t.Fatalf("失败步骤事实不对：%+v", out.Failed)
	}
	if !reflect.DeepEqual(out.Compensated, []string{"one"}) {
		t.Fatalf("补偿留痕不对：%v", out.Compensated)
	}
	if out.Completed[0].Compensa != "c-one" {
		t.Fatalf("被补偿的步骤要标出来：%+v", out.Completed[0])
	}
	if out.OK() {
		t.Fatal("失败并回滚过，不该报告 OK")
	}
	if len(out.CompensationProblems) != 0 {
		t.Fatalf("补偿成功却记了问题：%v", out.CompensationProblems)
	}
	// 第三步永不被调（失败即停）
	for _, n := range rc.order {
		if n == "s3" {
			t.Fatal("失败后仍继续执行了后续步骤")
		}
	}
}

func TestPlaybook_CompensationGatedAndBestEffort(t *testing.T) {
	// ① 补偿工具对当前角色不授权 ⇒ 不能借回滚调用它
	rc := newRecorder()
	rc.failOn["s2"] = errors.New("失败")
	reg := rc.registry(t)
	pb, err := New("pb", "剧本",
		Step{Name: "one", Tool: "s1", Compensation: comp("c-one", "c-locked")},
		Step{Name: "two", Tool: "s2"})
	if err != nil {
		t.Fatalf("New：%v", err)
	}
	out, err := pb.Execute(withRole("support"), "support", reg, nil)
	if err != nil {
		t.Fatalf("Execute：%v", err)
	}
	// 撤不回的剧本一开始就不该跑：预演阶段就拒，一个工具都不调
	if len(rc.order) != 0 {
		t.Fatalf("补偿不可用时仍执行了步骤：%v", rc.order)
	}
	if len(out.Problems) != 1 || out.Problems[0].Kind != ProblemNotAuthorized ||
		out.Problems[0].Step != "c-one" || !strings.Contains(out.Problems[0].Detail, "步骤 one 的补偿动作") {
		t.Fatalf("障碍应指名那条撤不回的补偿及其归属：%+v", out.Problems)
	}
	if len(out.Compensated) != 0 || len(out.Completed) != 0 {
		t.Fatalf("被整体拒掉却记了执行/补偿：%+v", out)
	}

	// ② 第一个补偿失败 ⇒ 剩下的仍要继续补偿（否则脏状态被永久留下），但失败必须被记下
	rc2 := newRecorder()
	rc2.failOn["s3"] = errors.New("第三步失败")
	rc2.failOn["c2"] = errors.New("撤销器坏了")
	reg2 := rc2.registry(t)
	pb2, err := New("pb", "剧本",
		Step{Name: "one", Tool: "s1", Compensation: comp("c-one", "c1")},
		Step{Name: "two", Tool: "s2", Compensation: comp("c-two", "c2")},
		Step{Name: "three", Tool: "s3"})
	if err != nil {
		t.Fatalf("New：%v", err)
	}
	out2, err := pb2.Execute(withRole("support"), "support", reg2, nil)
	if err != nil {
		t.Fatalf("Execute：%v", err)
	}
	if !reflect.DeepEqual(rc2.order, []string{"s1", "s2", "s3", "c2", "c1"}) {
		t.Fatalf("补偿应逆序尽力走完（一个失败不阻断其余）：%v", rc2.order)
	}
	if len(out2.CompensationProblems) != 1 || !strings.Contains(out2.CompensationProblems[0], "撤销器坏了") {
		t.Fatalf("补偿失败必须被记下：%v", out2.CompensationProblems)
	}
	if !reflect.DeepEqual(out2.Compensated, []string{"one"}) {
		t.Fatalf("只有真撤销成功的才算已补偿（c2 失败 ⇒ 只剩 one）：%v", out2.Compensated)
	}
	// 日志里也要有这条（管理面看得见回滚失败，而不是只埋在返回值里）
	if len(out2.Compensated)+len(out2.CompensationProblems) != 2 {
		t.Fatalf("回滚事实不完整：%+v", out2)
	}
}

func TestPlaybook_IdemKeyIsShapeOnly(t *testing.T) {
	build := func(v any) *Playbook {
		pb, err := New("pb", "剧本",
			Step{Name: "one", Tool: "s1", Args: map[string]any{"a": v, "b": "x"}},
			Step{Name: "two", Tool: "s2"})
		if err != nil {
			t.Fatalf("New：%v", err)
		}
		return pb
	}
	first, err := build(1).IdemKey()
	if err != nil {
		t.Fatalf("IdemKey：%v", err)
	}
	if len(first) != 64 || strings.TrimRight(first, "0123456789abcdef") != "" {
		t.Fatalf("幂等键应是 64 位十六进制：%q", first)
	}
	if second, _ := build(1).IdemKey(); second != first {
		t.Fatalf("同一剧本两次键不同：%s vs %s", first, second)
	}
	// 参数 map 的插入顺序不得影响键（JSON 序列化按字典序）
	a, _ := New("pb", "剧本", Step{Name: "one", Tool: "s1", Args: map[string]any{"a": 1, "b": "x"}})
	b, _ := New("pb", "剧本", Step{Name: "one", Tool: "s1", Args: map[string]any{"b": "x", "a": 1}})
	ka, _ := a.IdemKey()
	kb, _ := b.IdemKey()
	if ka != kb {
		t.Fatalf("map 插入顺序影响了幂等键：%s vs %s", ka, kb)
	}
	if other, _ := build(2).IdemKey(); other == first {
		t.Fatal("改了参数值却是同一个键")
	}
	// 角色不进键：键表达"做什么"，不表达"谁在做"
	if k, _ := build(1).IdemKey(); k != first {
		t.Fatal("幂等键不稳定")
	}
	reordered, err := New("pb", "剧本",
		Step{Name: "two", Tool: "s2"},
		Step{Name: "one", Tool: "s1", Args: map[string]any{"a": 1, "b": "x"}})
	if err != nil {
		t.Fatalf("New：%v", err)
	}
	if kr, _ := reordered.IdemKey(); kr == first {
		t.Fatal("步骤顺序变了却是同一个键（顺序有含义）")
	}
	var nilPB *Playbook
	if _, err := nilPB.IdemKey(); !errors.Is(err, ErrNoSteps) {
		t.Fatalf("nil 剧本应判 ErrNoSteps，实得 %v", err)
	}
}

func TestPlaybook_ProblemKindsClosedSet(t *testing.T) {
	kinds := ProblemKinds()
	if len(kinds) != 2 || kinds[0] != ProblemNotAuthorized || kinds[1] != ProblemUnknownTool {
		t.Fatalf("闭集不对：%v", kinds)
	}
	for _, k := range kinds {
		if !k.Valid() {
			t.Errorf("%s 未在闭集内", k)
		}
	}
	for _, k := range []ProblemKind{"", "unknown", "UNKNOWN_TOOL", "forbidden"} {
		if k.Valid() {
			t.Errorf("未知值 %q 被当作合法", k)
		}
	}
}

func withRole(role kitctx.Role) context.Context {
	return kitctx.WithRequest(context.Background(), kitctx.RequestContext{
		UserID: "u-pb", Role: role, TraceID: "tr-pb",
	})
}
