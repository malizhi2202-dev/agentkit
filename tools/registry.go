// Package tools 是 agentkit 的工具注册表：工具的**内部声明** + 按角色裁剪后的**模型可见集合**。
//
// 硬约束（ · D1 / D8 /  /  / NFR-A3 / NFR-T4）：
//
//  1. 本包只依赖 Go 标准库与 **`github.com/malizhi2202-dev/agentkit/**` 内的兄弟包**，不得依赖 宿主侧 的业务实现
//     （宿主侧目录（业务逻辑、接入层、配置）与 `宿主错误包/**`、`middlewares/**` …）。
//     判定式按 **ADR-010**（已接受，纠正 :268-270 / ADR-004 /  的**字面过宽写法**）：
//     「import 路径落在 module 前缀之下、但**不在** `.../本包/` 子树内」即违规。
//  2. 声明的可见外形**不复制**：本包直接复用  的 `model.ToolDecl` / `model.ToolVisible` /
//     `VisibleDecls`（type 与 role 映射只有一处定义，L-014 的冻结口径由 `model` 唯一拥有）。
//  3. 裁剪是**注册语义**，不是提示词约束：无权工具根本不进入对模型可见的工具声明。
//     不是「注册了再运行时拒绝」，也不是「灰掉」。
//     ⚠️ 但裁剪**不是唯一防线**：模型可能**自发**输出（幻觉 / prompt 注入诱导）一个没让它看见的名字
//     ⇒ 执行侧另有**同一谓词**的复核（`LookupAuthorized` + `runner.go` 的拒绝分支），见硬约束 5。
//  4. 角色不可判定（空串）⇒ 一个工具都不返回（fail-closed 第三侧，:325）。空角色
//     **不等于**「公开工具可用」：调用方拿到空工具集应中止并走 401，不降级。
//  5. **执行侧的授权判定与本包的裁剪复用同一个谓词**（`Tool.allows`），本包内不存在第二套
//     「角色 ⇒ 有权工具集」的口径：`TrimByRole` / `VisibleByRole`（声明侧）与
//     `LookupAuthorized`（执行侧）走的是同一份 `RequiredRoles` + 同一个判定式。
//     （本 change 已因「同一不变量写两遍」吃过亏 —— 角色体系三份枚举、仅一份有守卫，REVIEW-2 节C-1。）
//
// 角色来源（口径）：调用方在边界处经 `kitctx.RoleFrom(ctx)` 读出角色后传入本包；本包**不**把它
// 逐层传递（ 约束的是「逐层传递」，此处是唯一的读取点 → 使用点一跳）。
package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/malizhi2202-dev/agentkit/model"
)

// 注册失败的可识别错误。均以 `%w` 包装，调用方可用 errors.Is 判定。
var (
	// ErrToolNameRequired 表示工具没有名字：模型无法引用，声明不成立。
	ErrToolNameRequired = errors.New("tools: 工具名不能为空")
	// ErrToolDuplicate 表示工具名重复：同名两份声明会让模型选中的工具不可预期。
	ErrToolDuplicate = errors.New("tools: 工具名重复")
	// ErrToolRunRequired 表示声明缺 Run：模型能看见却执行不了的「半成品声明」不入表（fail-closed）。
	ErrToolRunRequired = errors.New("tools: 工具缺少 Run（不可执行的声明不入表）")
)

// RunFunc 是工具的执行体。
//
// 签名只用 stdlib `context.Context`（ 同款注入约束）：调用者 ctx 由上层在
// `kitctx.WithRequest` 之后传入，执行体内部据此携带调用者身份取数并过 Masker。
// 参数表来自模型给出的 JSON（解析在 `model.ToolCall.Args`），故为 `map[string]any`。
type RunFunc func(ctx context.Context, args map[string]any) (any, error)

// Tool 是注册表的**内部**声明：`{Name, Params, Summary, RequiredRoles}` 供裁剪与映射，
// `Run` 供执行。`RequiredRoles` / `Run` 都是内部字段，**不得**出现在模型可见外形里。
//
// `RequiredRoles` 为空表示「未声明角色要求」⇒ 任意**已判定**角色的调用者都可用；
// 非空表示「调用者角色必须是其中之一」。
type Tool struct {
	// Name 是模型引用工具的名字。
	Name string
	// Params 是参数 schema，惯例为 JSON-Schema 对象；本包只承载不校验。
	Params any
	// Summary 是给模型看的用途摘要。
	Summary string
	// RequiredRoles 是所需角色集合（内部名）。
	RequiredRoles []string
	// Run 是执行体；为 nil 的声明在 Register 时被拒绝（ErrToolRunRequired）。
	Run RunFunc
}

// Decl 把注册项映射为模型协议层的**内部**声明（ 的 `model.ToolDecl`）。
//
// 只映射两者共有的四要素 + 参数/摘要；`Source` / `ResultShape` 由声明提供方
// （`既有实现/tools`）按能力填充，注册表不臆造。
func (t Tool) Decl() model.ToolDecl {
	return model.ToolDecl{
		Name:          t.Name,
		Params:        t.Params,
		Summary:       t.Summary,
		RequiredRoles: t.RequiredRoles,
	}
}

// Visible 把内部声明映射成模型可见外形。
//
// 直接复用 `model.ToolDecl.Visible`：`role` 的口径（单值字符串 / 多角色去重后字典序 `,` 连接 /
// 未声明空串，L-014）在 `model` 内**唯一实现**，本包不再实现第二遍（R3）。
func (t Tool) Visible() model.ToolVisible {
	return t.Decl().Visible()
}

// allows 报告「已判定的 role」是否有权使用本工具。
//
// role == "" 一律 false：角色的**不可判定**由调用方在边界处拦截，本包不把它当作「匿名可用」。
//
// `RequiredRoles` 声明了却全是空白（`[]string{" "}` 这类畸形声明）同样一律 false：
// 畸形声明不得因「归一化后为空」被当成「未声明角色要求」而静默变成公开工具（fail-closed）。
func (t Tool) allows(role string) bool {
	if role == "" {
		return false
	}
	if len(t.RequiredRoles) == 0 {
		return true // 未声明角色要求 ⇒ 任意已判定角色可用
	}
	for _, required := range t.RequiredRoles {
		if strings.TrimSpace(required) == role {
			return true
		}
	}
	return false
}

// Registry 是工具注册表：注册顺序即声明顺序（确定性输出）。
//
// 并发安全：注册（写）与裁剪 / 查询（读）之间用 RWMutex 保护，因为  的 errgroup 会并行执行工具、
//
//	的调用循环会在多步之间重复裁剪。构造完成后通常只读。
type Registry struct {
	mu     sync.RWMutex
	order  []string
	byName map[string]Tool
}

// NewRegistry 按给定顺序注册一批工具；任一声明不合法则整体不成立（返回 nil + 错误）。
//
// 「整体不成立」是刻意的 fail-closed：半张表比没有表更危险（模型可能选中未装配的能力）。
func NewRegistry(list ...Tool) (*Registry, error) {
	r := &Registry{byName: make(map[string]Tool, len(list))}
	for _, t := range list {
		if err := r.Register(t); err != nil {
			return nil, err
		}
	}
	return r, nil
}

// Register 追加一个工具声明。
//
// 拒绝三种情况：工具名为空（ErrToolNameRequired）、Run 为 nil（ErrToolRunRequired）、
// 工具名已存在（ErrToolDuplicate）。被拒时注册表不变（不会覆盖既有声明）。
func (r *Registry) Register(t Tool) error {
	name := strings.TrimSpace(t.Name)
	if name == "" {
		return fmt.Errorf("%w: name=%q", ErrToolNameRequired, t.Name)
	}
	if t.Run == nil {
		return fmt.Errorf("%w: name=%q", ErrToolRunRequired, name)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.byName == nil {
		r.byName = make(map[string]Tool)
	}
	if _, dup := r.byName[name]; dup {
		return fmt.Errorf("%w: name=%q", ErrToolDuplicate, name)
	}
	t.Name = name
	r.byName[name] = t
	r.order = append(r.order, name)
	return nil
}

// Len 返回已注册工具数量。
func (r *Registry) Len() int {
	if r == nil {
		return 0
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.order)
}

// Names 返回按注册顺序排列的工具名副本（不暴露内部切片）。
func (r *Registry) Names() []string {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if len(r.order) == 0 {
		return nil
	}
	out := make([]string, len(r.order))
	copy(out, r.order)
	return out
}

// Lookup 按名取声明，供执行侧判定「模型请求调用的工具是否已注册」。
func (r *Registry) Lookup(name string) (Tool, bool) {
	if r == nil {
		return Tool{}, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.byName[name]
	return t, ok
}

// LookupAuthorized 按名取声明 **并**判定「调用者角色是否有权使用它」——执行侧的授权入口
// 。
//
// 判定复用 `Tool.allows`：与 `TrimByRole` / `VisibleByRole` 是**同一个谓词、同一份 `RequiredRoles`**
// ⇒ 不存在第二套「角色 ⇒ 有权工具集」的口径（本包硬约束 5）。
//
// 返回值语义（三者互斥且穷尽）：
//   - registered == false ⇒ 该名字不在注册表中；
//   - registered == true && !allowed ⇒ 已注册但该角色无权。可见性裁剪本不会让模型看见它，
//     但模型可能自发输出 / 被注入诱导出该名字 ⇒ 执行侧必须与「未注册」同样拒绝（见 runner.Run）；
//   - role 不可判定（空串）⇒ allowed 恒 false：与 `allows` 的空角色口径一致，**不是**「匿名可用」，
//     调用方据此 fail-closed（不得用默认角色或 nil 即放行兜底）。
func (r *Registry) LookupAuthorized(name, role string) (tool Tool, registered, allowed bool) {
	tool, registered = r.Lookup(name)
	if !registered {
		return Tool{}, false, false
	}
	return tool, true, tool.allows(role)
}

// TrimByRole 按角色裁剪注册表，返回该角色**有权使用**的工具（注册顺序）。
//
// 语义（ / D8 / ）：无权工具**不进入**结果集 —— 它不会出现在任何下游构造的
// 模型可见声明里，模型无从选择。
//
// ⚠️ 这条裁剪是「模型看不见」，**不构成**「点名也执行不了」的保证：模型可能自发输出一个没让它看见
// 的名字（幻觉 / prompt 注入诱导）。因此执行侧必须另有复核 —— `LookupAuthorized` 用**同一个**
// `allows` 谓词判定，`runner.Run` 对无权调用与「未注册」同样拒绝。
// 删除裁剪与删除执行侧复核是两件事：两者都在，才是纵深防御。
//
// role 为空（不可判定）时返回 nil：fail-closed 第三侧。调用方应据此中止请求。
// 结果为空时返回 nil（与 `model.VisibleDecls` 的空输入语义一致，让 wire 上省略 `tools` 键）。
func (r *Registry) TrimByRole(role string) []Tool {
	if r == nil || role == "" {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []Tool
	for _, name := range r.order {
		t, ok := r.byName[name]
		if !ok || !t.allows(role) {
			continue
		}
		out = append(out, t)
	}
	return out
}

// VisibleByRole 是 TrimByRole 的声明侧出口：直接产出  的 `$.tools[].{name,params,summary,role}`。
//
// 批量映射走 `model.VisibleDecls`（ 的批量出口，与单条 `Visible` 同一口径）；顺序与
// TrimByRole 一致；role 不可判定或裁完为空时返回 nil（wire 上 `tools` 键省略）。
func (r *Registry) VisibleByRole(role string) []model.ToolVisible {
	trimmed := r.TrimByRole(role)
	if len(trimmed) == 0 {
		return nil
	}
	decls := make([]model.ToolDecl, 0, len(trimmed))
	for _, t := range trimmed {
		decls = append(decls, t.Decl())
	}
	return model.VisibleDecls(decls)
}
