// Package playbook 是"剧本"：一串**按序执行、可预演、可补偿**的步骤。
//
// 它与 `scene` 的分工是刻意的，别混：
//
//   - `scene` 回答"这个模板里当前角色能用哪些工具"（一张静态表，不执行任何东西）；
//   - `playbook` 回答"这些步骤按什么顺序跑、失败了怎么收"（一份可运行的编排）。
//
// 三条形状约束都来自既有纪律：
//
//  1. **步骤只引用工具名**，参数是静态值。授权与否完全交给 `tools.Registry` 那一条谓词
//     （ADR-011 规则 5）：本包里没有任何一处比较角色字符串。
//  2. **预演（DryRun）与真执行（Execute）走同一份校验**，否则"预演通过、执行炸掉"就是假的。
//  3. **补偿与步骤过的是同一条授权谓词、同一个角色**，因此"这个角色跑不了回滚"这种剧本
//     在预演阶段就被整份拒掉（不是执行到一半才发现撤不回）；运行期的撤销不再重复判权限，
//     否则同一不变量就被写了两遍。
package playbook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/malizhi2202-dev/agentkit/kitctx"
	"github.com/malizhi2202-dev/agentkit/tools"
)

// 构造与执行阶段的错误。
var (
	// ErrIDRequired 表示剧本缺少 ID（幂等键与日志定位都靠它）。
	ErrIDRequired = errors.New("playbook: 剧本缺少 ID")
	// ErrLabelRequired 表示剧本缺少展示名。
	ErrLabelRequired = errors.New("playbook: 剧本缺少展示名（Label）")
	// ErrNoSteps 表示空剧本（PLAN 给的判据里"缺步 ⇒ 红"就是这一条）。
	ErrNoSteps = errors.New("playbook: 剧本没有任何步骤")
	// ErrStepNameRequired 表示步骤缺少名字（补偿与轨迹都要按它寻址）。
	ErrStepNameRequired = errors.New("playbook: 步骤缺少 Name")
	// ErrStepToolRequired 表示步骤没有引用工具。
	ErrStepToolRequired = errors.New("playbook: 步骤没有引用 Tool")
	// ErrDuplicateStep 表示步骤名重复。
	ErrDuplicateStep = errors.New("playbook: 步骤名重复")
	// ErrRegistryRequired 表示校验/执行时没交出工具表。
	ErrRegistryRequired = errors.New("playbook: 需要 tools.Registry（授权判定的唯一来源）")
)

// ProblemKind 是"这一步为什么不能跑"的原因闭集。
type ProblemKind string

const (
	// ProblemUnknownTool 表示引用的工具在表里不存在（配置漂移）。
	ProblemUnknownTool ProblemKind = "unknown_tool"
	// ProblemNotAuthorized 表示工具存在但当前角色不被授权。
	ProblemNotAuthorized ProblemKind = "not_authorized"
)

var problemKinds = map[ProblemKind]bool{ProblemUnknownTool: true, ProblemNotAuthorized: true}

// Valid 报告是否为已知原因。
func (k ProblemKind) Valid() bool { return problemKinds[k] }

// ProblemKinds 返回闭集（字典序）。
func ProblemKinds() []ProblemKind {
	out := make([]ProblemKind, 0, len(problemKinds))
	for k := range problemKinds {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Problem 是一条"预演就该发现"的事实。
type Problem struct {
	Step   string      `json:"step"`
	Tool   string      `json:"tool"`
	Kind   ProblemKind `json:"kind"`
	Detail string      `json:"detail,omitempty"`
}

// Step 是一个步骤。Compensation 为该步成功后的撤销动作（nil ⇒ 不可撤销）。
type Step struct {
	Name         string         `json:"name"`
	Tool         string         `json:"tool"`
	Args         map[string]any `json:"args,omitempty"`
	Compensation *Step          `json:"compensation,omitempty"`
}

// Playbook 是一份按序编排。构造后规则不再变化（参数在构造期复制）。
type Playbook struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Steps []Step `json:"steps"`
}

// New 构造剧本并当场拒绝畸形形状（不留到运行期才发现"步骤没名字"）。
func New(id, label string, steps ...Step) (*Playbook, error) {
	id, label = strings.TrimSpace(id), strings.TrimSpace(label)
	if id == "" {
		return nil, ErrIDRequired
	}
	if label == "" {
		return nil, fmt.Errorf("%w：%s", ErrLabelRequired, id)
	}
	if len(steps) == 0 {
		return nil, fmt.Errorf("%w：%s", ErrNoSteps, id)
	}
	seen := map[string]bool{}
	out := make([]Step, 0, len(steps))
	for i, st := range steps {
		cp, err := normalize(id, i, st, seen)
		if err != nil {
			return nil, err
		}
		out = append(out, cp)
	}
	return &Playbook{ID: id, Label: label, Steps: out}, nil
}

// normalize 校验并复制一个步骤（含它的补偿步骤）。
func normalize(id string, i int, st Step, seen map[string]bool) (Step, error) {
	name := strings.TrimSpace(st.Name)
	tool := strings.TrimSpace(st.Tool)
	switch {
	case name == "":
		return Step{}, fmt.Errorf("%w：剧本 %s 第 %d 步", ErrStepNameRequired, id, i+1)
	case seen[name]:
		return Step{}, fmt.Errorf("%w：%s（剧本 %s）", ErrDuplicateStep, name, id)
	case tool == "":
		return Step{}, fmt.Errorf("%w：步骤 %q", ErrStepToolRequired, name)
	}
	seen[name] = true
	out := Step{Name: name, Tool: tool, Args: copyArgs(st.Args)}
	if st.Compensation != nil {
		cp, err := normalize(id+"#"+name+"-补偿", 0, *st.Compensation, map[string]bool{})
		if err != nil {
			return Step{}, err
		}
		out.Compensation = &cp
	}
	return out, nil
}

func copyArgs(args map[string]any) map[string]any {
	if len(args) == 0 {
		return nil
	}
	out := make(map[string]any, len(args))
	for k, v := range args {
		out[strings.TrimSpace(k)] = v
	}
	return out
}

// Validate 只回答"这个当前角色能不能跑"，不执行任何东西。
//
// 角色为空 ⇒ 有必需角色的步骤全部报 not_authorized（空串不是"已判定角色"，框架不猜默认）。
func (p *Playbook) Validate(role string, reg *tools.Registry) ([]Problem, error) {
	if p == nil || len(p.Steps) == 0 {
		return nil, ErrNoSteps
	}
	if reg == nil {
		return nil, ErrRegistryRequired
	}
	var problems []Problem
	for _, st := range p.Steps {
		problems = append(problems, checkStep(st, role, reg)...)
	}
	return problems, nil
}

// checkStep 校验一个步骤及其补偿步骤（补偿也过同一条谓词）。
func checkStep(st Step, role string, reg *tools.Registry) []Problem {
	var out []Problem
	add := func(s Step, note string) {
		_, registered, allowed := reg.LookupAuthorized(s.Tool, role)
		var kind ProblemKind
		switch {
		case !registered:
			kind = ProblemUnknownTool
			note = strings.TrimSpace("工具表里没有这个名字（配置漂移） " + note)
		case !allowed:
			kind = ProblemNotAuthorized
			note = strings.TrimSpace("当前角色不被授权 " + note)
		default:
			return
		}
		out = append(out, Problem{Step: s.Name, Tool: s.Tool, Kind: kind, Detail: note})
	}
	add(st, "")
	if st.Compensation != nil {
		// 指名"是谁的补偿"：只看到补偿步骤名，管理面上还得回头查剧本才知道它属于哪一步。
		add(*st.Compensation, "（步骤 "+st.Name+" 的补偿动作）")
	}
	return out
}

// DryRun 预演：返回将要执行的步骤清单与所有障碍，**一次工具都不调**。
//
// 这是"零写入"判据的落点：预演通过不代表执行成功（外部世界会变），但预演必须先发现
// 那些确定性可发现的问题（引用漂移、越权步骤）—— 让执行去撞它们就等于没有预演。
func (p *Playbook) DryRun(role string, reg *tools.Registry) ([]Step, []Problem, error) {
	problems, err := p.Validate(role, reg)
	if err != nil {
		return nil, nil, err
	}
	if len(problems) > 0 {
		return nil, problems, nil
	}
	steps := make([]Step, 0, len(p.Steps))
	for _, st := range p.Steps {
		steps = append(steps, Step{Name: st.Name, Tool: st.Tool, Args: copyArgs(st.Args),
			Compensation: st.Compensation})
	}
	return steps, nil, nil
}

// Result 是一步的执行结果。
type Result struct {
	Step string `json:"step"`
	Tool string `json:"tool"`
	// Output 是工具返回值序列化后的文本（不塞结构体，避免把宿主类型带进可序列化结果）。
	Output   string `json:"output,omitempty"`
	Err      string `json:"err,omitempty"`
	Compensa string `json:"compensated,omitempty"`
}

// Outcome 是一次执行的完整结果。
type Outcome struct {
	Completed []Result  `json:"completed"`
	Failed    *Result   `json:"failed,omitempty"`
	Problems  []Problem `json:"problems,omitempty"`
	// Compensated 是逆序撤销掉的步骤名（失败才会有）。
	Compensated []string `json:"compensated,omitempty"`
	// CompensationProblems 记录撤销本身失败的事实 —— 静默失败的回滚比回滚没发生更危险。
	CompensationProblems []string `json:"compensation_problems,omitempty"`
}

// OK 报告这次执行是否全部完成且没有需要撤销的东西。
func (o Outcome) OK() bool { return o.Failed == nil && len(o.Compensated) == 0 }

// Execute 顺序执行；任一步失败 ⇒ 停下并**逆序补偿已完成的步骤**。
//
// 补偿失败不中断后续补偿（否则第一个撤销失败就把剩下的脏状态永久留下），但每一条都必须被记下来。
func (p *Playbook) Execute(ctx context.Context, role string, reg *tools.Registry, log kitctx.Logger) (Outcome, error) {
	steps, problems, err := p.DryRun(role, reg)
	if err != nil {
		return Outcome{}, err
	}
	var out Outcome
	if len(problems) > 0 {
		out.Problems = problems
		// 预演发现问题时不执行任何步骤，但要把"没跑"这件事作为结果交回去（不是 error）。
		failed := &Result{Step: problems[0].Step, Tool: problems[0].Tool, Err: string(problems[0].Kind)}
		out.Failed = failed
		return out, nil
	}
	for _, st := range steps {
		tool, _ := reg.Lookup(st.Tool) // DryRun 已确认存在且可用
		res := Result{Step: st.Name, Tool: st.Tool}
		value, runErr := tool.Run(ctx, copyArgs(st.Args))
		if runErr != nil {
			res.Err = runErr.Error()
			out.Failed = &res
			p.compensate(ctx, reg, log, out.Completed, &out)
			return out, nil
		}
		if b, mErr := json.Marshal(value); mErr == nil {
			res.Output = string(b)
		} else {
			res.Output = fmt.Sprint(value)
		}
		out.Completed = append(out.Completed, res)
	}
	return out, nil
}

// compensate 逆序执行已完成步骤的补偿动作。
func (p *Playbook) compensate(ctx context.Context, reg *tools.Registry, log kitctx.Logger, done []Result, out *Outcome) {
	byName := map[string]Step{}
	for _, st := range p.Steps {
		byName[st.Name] = st
	}
	for i := len(done) - 1; i >= 0; i-- {
		st, ok := byName[done[i].Step]
		if !ok || st.Compensation == nil {
			continue
		}
		c := *st.Compensation
		// 补偿的授权**不在这里再判一遍**：`Validate` 已经用同一条谓词、同一个角色查过步骤与补偿，
		// 有障碍时 Execute 一开始就不会动（见 DryRun 分支）。这里只兜"取不到可执行体"这种
		// 表本身出问题的情形 —— 它会让 nil RunFunc 直接 panic，必须挡在调用之前。
		tool, ok := reg.Lookup(c.Tool)
		if !ok || tool.Run == nil {
			msg := fmt.Sprintf("补偿步骤 %s 无可执行体（工具 %s）", c.Name, c.Tool)
			out.CompensationProblems = append(out.CompensationProblems, msg)
			warn(log, "playbook: 补偿不可执行", map[string]any{"step": c.Name, "tool": c.Tool})
			continue
		}
		if _, err := tool.Run(ctx, copyArgs(c.Args)); err != nil {
			msg := fmt.Sprintf("补偿步骤 %s 执行失败：%v", c.Name, err)
			out.CompensationProblems = append(out.CompensationProblems, msg)
			warn(log, "playbook: 补偿失败", map[string]any{"step": c.Name, "err": err.Error()})
			continue
		}
		out.Compensated = append(out.Compensated, st.Name)
		for j := len(out.Completed) - 1; j >= 0; j-- {
			if out.Completed[j].Step == st.Name {
				out.Completed[j].Compensa = c.Name
				break
			}
		}
	}
}

func warn(log kitctx.Logger, msg string, fields map[string]any) {
	if log != nil {
		log.Warn(msg, fields)
	}
}

// IdemKey 返回剧本的幂等键：形状内容哈希（64 位十六进制），不含任何执行结果。
//
// 稳定性靠两点：步骤顺序即声明顺序；参数经 `encoding/json` 序列化（map 键排序）。
// 因此"同一个剧本换个角色"是**同一个**键（键表达的是做什么，不是谁在做），
// 而改一个参数值就会换键 —— 这正是幂等去重需要的口径。
func (p *Playbook) IdemKey() (string, error) {
	if p == nil || len(p.Steps) == 0 {
		return "", ErrNoSteps
	}
	canonical := struct {
		ID    string `json:"id"`
		Label string `json:"label"`
		Steps []Step `json:"steps"`
	}{ID: p.ID, Label: p.Label, Steps: p.Steps}
	raw, err := json.Marshal(canonical)
	if err != nil {
		return "", fmt.Errorf("playbook: 幂等键序列化失败：%w", err)
	}
	return kitctx.Digest(raw), nil
}
