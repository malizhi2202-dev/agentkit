package semantic

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/malizhi2202-dev/agentkit/tools"
)

// 本文件是「语义 → 工具」的绑定层（P6）：把一次理解结果（意图 + 实体）翻成
// 一串**可回放的调用计划**。它只选择与装配参数，不执行任何东西 —— 执行仍归调用循环。
//
// 两条纪律决定了这里几乎没有"逻辑"，只有查表：
//
//  1. **不重写第二份判据**。"能不能进编排"用 `Understanding.Executable`（本包既有唯一入口），
//     "这个工具当前角色能不能用"用 `tools.Registry.LookupAuthorized`（框架内唯一授权谓词）。
//     本文件里没有任何一处比较角色字符串。
//  2. **不猜**。实体缺失、诉求没被理解到、工具不认识 ⇒ 留下一条 Skip 而不是凑一个参数。
//     凑出来的参数会让下游拿着零值去查一个不存在的东西（P5 在工具边界上拦过一次，这里从源头就不生成）。

// SkipReason 是"某条规则没有成为调用"的原因闭集。
type SkipReason string

const (
	// SkipPendingConfirmation 表示存在待用户确认的候选实体：语义层不替用户挑一个。
	SkipPendingConfirmation SkipReason = "pending_confirmation"
	// SkipUnsupported 表示诉求不被当前能力范围覆盖。
	SkipUnsupported SkipReason = "unsupported"
	// SkipNotRequested 表示这条规则的诉求不在本次理解结果里（规则与输入不相干）。
	SkipNotRequested SkipReason = "not_requested"
	// SkipUnknownTool 表示工具表里根本没有这个名字（配置漂移，不是权限问题）。
	SkipUnknownTool SkipReason = "unknown_tool"
	// SkipNotAuthorized 表示工具存在但当前角色不被授权（含角色不可用的情况）。
	SkipNotAuthorized SkipReason = "not_authorized"
	// SkipMissingEntity 表示装配参数所需的实体类型没被识别出来。
	SkipMissingEntity SkipReason = "missing_entity"
)

var skipKinds = map[SkipReason]bool{
	SkipPendingConfirmation: true,
	SkipUnsupported:         true,
	SkipNotRequested:        true,
	SkipUnknownTool:         true,
	SkipNotAuthorized:       true,
	SkipMissingEntity:       true,
}

// Valid 报告是否为已知原因（闭集：新增值必须同时进 skipKinds 与测试）。
func (r SkipReason) Valid() bool { return skipKinds[r] }

// String 让零值原因在日志与 JSON 里也读得通，而不是空串。
func (r SkipReason) String() string {
	if r == "" {
		return "unspecified"
	}
	return string(r)
}

// SkipKinds 返回原因闭集（按字典序，供管理面枚举与测试遍历）。
func SkipKinds() []SkipReason {
	out := make([]SkipReason, 0, len(skipKinds))
	for k := range skipKinds {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// 绑定层的登记错误（全部构造期，不留到运行期）。
var (
	// ErrNoRules 表示构造了一个没有任何规则的规划器（多半是配置没读到）。
	ErrNoRules = errors.New("semantic: 绑定规则为空")
	// ErrRuleIntentRequired 表示规则没写触发诉求。
	ErrRuleIntentRequired = errors.New("semantic: 规则缺少 Intent")
	// ErrRuleToolRequired 表示规则没写目标工具名。
	ErrRuleToolRequired = errors.New("semantic: 规则缺少 Tool 名")
	// ErrRuleArgKeyRequired 表示实体映射的键或目标参数键为空（会造出一个无法寻址的参数）。
	ErrRuleArgKeyRequired = errors.New("semantic: 规则的实体映射键/参数键为空")
	// ErrRuleConstConflict 表示常量参数与实体装配的目标参数键重名（两份来源争同一个键）。
	ErrRuleConstConflict = errors.New("semantic: 常量参数与实体装配的键冲突")
	// ErrRuleDuplicate 表示同一 (Intent, Tool) 被声明两次（顺序决定结果就是隐患）。
	ErrRuleDuplicate = errors.New("semantic: 重复的 (Intent, Tool) 规则")
	// ErrRegistryRequired 表示 Plan 调用没交出工具表。
	ErrRegistryRequired = errors.New("semantic: 需要 tools.Registry（授权判定的唯一来源）")
)

// Rule 是一条绑定：某个诉求命中时，用哪个工具、参数从哪类实体来。
//
// FromEntity 是「实体类型 → 工具参数键」的映射（如 {"ip":"ip"}）；
// Const 是附加常量（如 {"scope":"platform"}），与 FromEntity 的目标键**不得重名**。
type Rule struct {
	Intent     Intent
	Tool       string
	FromEntity map[string]string
	Const      map[string]any
}

// Call 是一次确定的工具调用计划。
type Call struct {
	Intent Intent         `json:"intent"`
	Tool   string         `json:"tool"`
	Args   map[string]any `json:"args,omitempty"`
}

// Skip 是"这条规则为什么没成为调用"的事实留痕。
//
// 整次理解结果不可执行时留下的是**一条**不带 Intent/Tool 的 Skip —— 那是全局判定，
// 不该被摊成"每条规则各自不成立"的噪音。
type Skip struct {
	Intent Intent     `json:"intent,omitempty"`
	Tool   string     `json:"tool,omitempty"`
	Reason SkipReason `json:"reason"`
	Detail string     `json:"detail,omitempty"`
}

// Plan 是一次绑定的产物：按规则声明序排列，因此可稳定回放与比对。
type Plan struct {
	Calls   []Call `json:"calls"`
	Skipped []Skip `json:"skipped,omitempty"`
}

// Empty 报告计划里没有任何待执行调用。
func (p Plan) Empty() bool { return len(p.Calls) == 0 }

// JSON 把计划序列化成一串稳定字节（"轨迹可回放"的字面落点）。
//
// 稳定靠两件事：规则按声明序遍历；实体类型与常量键在装配前排序 —— Go 的 map 遍历顺序随机，
// 不排序就会让同一次输入产出不同字节，回放与幂等键都无从谈起。
func (p Plan) JSON() (string, error) {
	b, err := json.Marshal(p)
	if err != nil {
		return "", fmt.Errorf("semantic: 计划序列化失败：%w", err)
	}
	return string(b), nil
}

// Planner 是按声明序遍历规则的绑定器。
type Planner struct {
	rules []Rule
}

// NewPlanner 构造绑定器，并在此时（而不是运行期）拒绝所有畸形规则。
//
// 规则会被**完整复制**：调用方之后改动自己那份切片/映射不会影响已构造的绑定器，
// 否则"装配期定形"就是假的。
func NewPlanner(rules ...Rule) (*Planner, error) {
	if len(rules) == 0 {
		return nil, ErrNoRules
	}
	seen := make(map[string]bool, len(rules))
	out := make([]Rule, 0, len(rules))
	for i, r := range rules {
		tool := strings.TrimSpace(r.Tool)
		if strings.TrimSpace(string(r.Intent)) == "" {
			return nil, fmt.Errorf("%w：第 %d 条", ErrRuleIntentRequired, i)
		}
		if tool == "" {
			return nil, fmt.Errorf("%w：第 %d 条", ErrRuleToolRequired, i)
		}
		from := make(map[string]string, len(r.FromEntity))
		targets := make(map[string]bool, len(r.FromEntity))
		for etype, key := range r.FromEntity {
			e, k := strings.TrimSpace(etype), strings.TrimSpace(key)
			if e == "" || k == "" {
				return nil, fmt.Errorf("%w：第 %d 条（%s→%q）", ErrRuleArgKeyRequired, i, etype, key)
			}
			if _, dup := from[e]; dup {
				return nil, fmt.Errorf("%w：第 %d 条同一实体类型 %q 映射了两个参数键", ErrRuleArgKeyRequired, i, e)
			}
			from[e], targets[k] = k, true
		}
		consts := make(map[string]any, len(r.Const))
		for k, v := range r.Const {
			key := strings.TrimSpace(k)
			if key == "" {
				return nil, fmt.Errorf("%w：第 %d 条常量参数键为空", ErrRuleArgKeyRequired, i)
			}
			if targets[key] {
				return nil, fmt.Errorf("%w：第 %d 条常量键 %q 已被实体装配占用", ErrRuleConstConflict, i, key)
			}
			consts[key] = v
		}
		id := string(r.Intent) + "\x00" + tool
		if seen[id] {
			return nil, fmt.Errorf("%w：第 %d 条 (%s, %s)", ErrRuleDuplicate, i, r.Intent, tool)
		}
		seen[id] = true
		out = append(out, Rule{Intent: r.Intent, Tool: tool, FromEntity: from, Const: consts})
	}
	return &Planner{rules: out}, nil
}

// Rules 返回规则副本（供管理面内省；改动副本不影响绑定器）。
func (p *Planner) Rules() []Rule {
	if p == nil {
		return nil
	}
	out := make([]Rule, 0, len(p.rules))
	for _, r := range p.rules {
		cp := Rule{Intent: r.Intent, Tool: r.Tool}
		if len(r.FromEntity) > 0 {
			cp.FromEntity = make(map[string]string, len(r.FromEntity))
			for k, v := range r.FromEntity {
				cp.FromEntity[k] = v
			}
		}
		if len(r.Const) > 0 {
			cp.Const = make(map[string]any, len(r.Const))
			for k, v := range r.Const {
				cp.Const[k] = v
			}
		}
		out = append(out, cp)
	}
	return out
}

// Plan 把一次理解结果翻成调用计划。
//
// 判定顺序刻意是「全局可执行性 → 授权 → 参数装配」：先看这次输入允不允许进编排
// （候选/不支持），再看这个工具对当前角色开不开门，最后才谈参数。反过来就会出现
// "因为缺实体而 Skip"这种把配置问题伪装成数据问题的结论。
//
// role 为空不特殊处理：唯一谓词会把"角色不可用"判成不允许，框架不替宿主猜一个默认角色。
func (p *Planner) Plan(u Understanding, role string, reg *tools.Registry) (Plan, error) {
	if p == nil || len(p.rules) == 0 {
		return Plan{}, ErrNoRules
	}
	if reg == nil {
		return Plan{}, ErrRegistryRequired
	}
	var plan Plan
	if !u.Executable() {
		reason := SkipUnsupported
		detail := "诉求不被当前能力范围覆盖"
		if len(u.Candidates) > 0 {
			// 候选优先：这是"等用户确认"这个更具体的门（不擅自挑一个）。
			reason = SkipPendingConfirmation
			detail = fmt.Sprintf("存在 %d 个待确认候选", len(u.Candidates))
		}
		plan.Skipped = append(plan.Skipped, Skip{Reason: reason, Detail: detail})
		return plan, nil
	}
	for _, r := range p.rules {
		if !u.HasIntent(r.Intent) {
			plan.Skipped = append(plan.Skipped, Skip{
				Intent: r.Intent, Tool: r.Tool, Reason: SkipNotRequested,
			})
			continue
		}
		_, registered, allowed := reg.LookupAuthorized(r.Tool, role)
		if !registered {
			plan.Skipped = append(plan.Skipped, Skip{
				Intent: r.Intent, Tool: r.Tool, Reason: SkipUnknownTool,
				Detail: "工具表里没有这个名字（配置漂移）",
			})
			continue
		}
		if !allowed {
			plan.Skipped = append(plan.Skipped, Skip{
				Intent: r.Intent, Tool: r.Tool, Reason: SkipNotAuthorized,
			})
			continue
		}
		args, missing := assemble(u, r)
		if len(missing) > 0 {
			plan.Skipped = append(plan.Skipped, Skip{
				Intent: r.Intent, Tool: r.Tool, Reason: SkipMissingEntity,
				Detail: "缺少实体类型：" + strings.Join(missing, ","),
			})
			continue
		}
		plan.Calls = append(plan.Calls, Call{Intent: r.Intent, Tool: r.Tool, Args: args})
	}
	return plan, nil
}

// assemble 装配参数：常量打底，再按**排序后**的实体类型写入实体值。
//
// 同类型多个实体时取第一个（按 u.Entities 的原有顺序）：类型歧义本该在语义层就落进
// Candidates，走到这里还歧义说明上游判断已过；这里不报错也不换值，保持"顺序决定结果"的唯一口径。
func assemble(u Understanding, r Rule) (map[string]any, []string) {
	args := make(map[string]any, len(r.Const)+len(r.FromEntity))
	for k, v := range r.Const {
		args[k] = v
	}
	if len(r.FromEntity) == 0 {
		if len(args) == 0 {
			return nil, nil
		}
		return args, nil
	}
	types := make([]string, 0, len(r.FromEntity))
	for etype := range r.FromEntity {
		types = append(types, etype)
	}
	sort.Strings(types)
	var missing []string
	for _, etype := range types {
		value, ok := firstEntity(u, etype)
		if !ok {
			missing = append(missing, etype)
			continue
		}
		args[r.FromEntity[etype]] = value
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, missing
	}
	if len(args) == 0 {
		return nil, nil
	}
	return args, nil
}

// firstEntity 按 u.Entities 原有顺序取该类型的第一个值。
func firstEntity(u Understanding, etype string) (string, bool) {
	for _, e := range u.Entities {
		if e.Type == etype {
			return e.Value, true
		}
	}
	return "", false
}
