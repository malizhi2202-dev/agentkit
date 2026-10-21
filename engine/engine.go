// Package engine 是最后那一步：把已经做好的插件接成"一个能跑、也能停"的引擎。
//
// 为什么还要有这一层：`kit` 只保证"装配正确"（两阶段、依赖排序、失败回滚），
// `preset` 只保证"每块都能插"。宿主真正想写的通常是一行：
//
//	e, err := engine.New(engine.Deps{Model: m, Perms: perms, Tools: decl, Loop: loop})
//	out, err := e.Handle(ctx, question)
//
// 于是"装配顺序、哪些插件必须先存在、停止时谁先关"这些事不再需要每个宿主各自记一遍 ——
// 这正是两仓里重复出现过的东西（宿主侧 的 `config/engine.go` 与 log_collection 的组装函数）。
//
// 本层刻意**薄**：它不实现任何判定，只做组装与生命周期。
// 授权、裁剪、限额、降级、轨迹全部仍由被注入的那几个插件自己负责（框架内授权谓词只有一条，
// 见 ADR-011 规则 5）；`engine` 只是把它们接起来，所以它没有任何"看起来像业务"的字段。
package engine

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/malizhi2202-dev/agentkit/agent"
	"github.com/malizhi2202-dev/agentkit/kit"
	"github.com/malizhi2202-dev/agentkit/preset"
)

// 四个注入点缺任意一个都在**构造期**报错，不留到第一次请求才炸。
//
// 为什么是这四个：少模型就没有循环可跑；少权限表则工具声明无处校验（同源声明在 Build 期要查权限点）；
// 少工具表循环会"能跑但什么都调不到"（静默退化）；少循环则 Handle 无事可做。
// 而 Server / Scenes / Planner / Audit 是**可选**的（放 `Also`）：纯内网批处理不需要 HTTP 入口，
// 强制它们就会逼宿主造假日件 —— 那是本末倒置。
var (
	// ErrModelRequired 表示没注入模型插件。
	ErrModelRequired = errors.New("engine: 未注入模型插件（preset.Model）")
	// ErrPermsRequired 表示没注入权限表插件。
	ErrPermsRequired = errors.New("engine: 未注入权限表（preset.Perms）")
	// ErrToolsRequired 表示没注入工具/接口声明插件。
	ErrToolsRequired = errors.New("engine: 未注入工具声明（preset.Tools）")
	// ErrLoopRequired 表示没注入循环插件。
	ErrLoopRequired = errors.New("engine: 未注入循环（preset.Loop）")
	// ErrKitRequired 表示装配根没能构造出来（内部保护，正常不会由调用方触发）。
	ErrKitRequired = errors.New("engine: 装配根未构造")
	// ErrEngineStopped 表示引擎已停止 ⇒ 不再接受新问题（与"没装配好"是两件事，要能分开诊断）。
	ErrEngineStopped = errors.New("engine: 引擎已停止，不再处理请求")
)

// Deps 是构造引擎所需的东西。
type Deps struct {
	// Model / Perms / Tools / Loop 四个为必填。
	Model *preset.Model
	Perms *preset.Perms
	Tools *preset.Tools
	Loop  *preset.Loop

	// Config 是交给装配根的东西（Clock / Logger / Web / Tools 表）；零值即可用。
	Config kit.Config
	// Also 是可选扩展（Server / Scenes / Planner / Audit…），走同一条 Use/Build 路径。
	Also []kit.Plugin
}

// Engine 是一个已装配、可停止的引擎实例。它不持有会话状态（ADR-002）：
// 记忆与身份都由 `Handle` 的 ctx 携带。
type Engine struct {
	k    *kit.Kit
	loop *preset.Loop

	mu      sync.RWMutex
	stopped bool
}

// New 组装并 Build。任一必需注入点缺失 ⇒ 返回聚合错误（一次看全，不用一轮一轮试）。
func New(d Deps) (*Engine, error) {
	var problems []error
	if d.Model == nil {
		problems = append(problems, ErrModelRequired)
	}
	if d.Perms == nil {
		problems = append(problems, ErrPermsRequired)
	}
	if d.Tools == nil {
		problems = append(problems, ErrToolsRequired)
	}
	if d.Loop == nil {
		problems = append(problems, ErrLoopRequired)
	}
	if len(problems) > 0 {
		return nil, errors.Join(problems...)
	}
	k, err := kit.New(d.Config)
	if err != nil {
		return nil, fmt.Errorf("engine: 装配根构造失败：%w", err)
	}
	e := &Engine{k: k, loop: d.Loop}
	// 顺序：权限表与模型先给，声明与循环后给 —— 依赖排序本来就会纠正错序，
	// 写在这儿是为了让"读代码的人"不必先去查拓扑。
	// `e` 自己也作为插件登记：`Kit.OnStop` 只允许在插件 Apply 期间注册（装配期能力），
	// 所以"引擎停止后拒绝服务"这件事必须由引擎在 Apply 里挂钩，而不是 New 里事后补 —— 后者会被 kit 直接拒绝。
	plugins := append([]kit.Plugin{e, d.Perms, d.Model, d.Tools, d.Loop}, d.Also...)
	if err := k.Use(plugins...); err != nil {
		return nil, fmt.Errorf("engine: 插件登记失败：%w", err)
	}
	if err := k.Build(); err != nil {
		return nil, fmt.Errorf("engine: 装配失败：%w", err)
	}
	return e, nil
}

// 引擎自身实现 kit.Plugin：它需要的唯一装配期能力是"停止钩子"，而那个钩子只能在 Apply 期间注册。

// Name 返回插件名。
func (e *Engine) Name() string { return "engine.root" }

// Declare 不登记任何扩展点（引擎只是把别人接起来）。
func (e *Engine) Declare(*kit.Declarer) error { return nil }

// Apply 挂停止钩子：`Stop` 之后 `Handle` 不再接受新问题。
func (e *Engine) Apply(k *kit.Kit) error {
	return k.OnStop(func() {
		e.mu.Lock()
		e.stopped = true
		e.mu.Unlock()
	})
}

var _ kit.Plugin = (*Engine)(nil)

// Handle 处理一个问题。身份（角色、用户、预算、traceid）都从 ctx 读：
// 本层不接受角色参数，免得在这里长出"默认角色"（框架不猜默认角色）。
func (e *Engine) Handle(ctx context.Context, question string) (agent.LoopOutcome, error) {
	if e == nil || e.k == nil {
		return agent.LoopOutcome{}, ErrKitRequired
	}
	e.mu.RLock()
	stopped := e.stopped
	e.mu.RUnlock()
	if stopped {
		return agent.LoopOutcome{}, ErrEngineStopped
	}
	return e.loop.Ask(ctx, question)
}

// Tools 返回当前工具表的快照名（管理面/自检查用），停止后仍可读（只读事实，不是新执行）。
func (e *Engine) Tools() []string {
	if e == nil || e.k == nil {
		return nil
	}
	return e.k.Tools().Names()
}

// Kit 交出装配根：需要更细控制（自己加钩子、读内省信息）的宿主不必绕过本层另起一套。
func (e *Engine) Kit() *kit.Kit {
	if e == nil {
		return nil
	}
	return e.k
}

// Stop 关闭引擎（幂等）。已装配插件的 OnStop 按登记的逆序执行。
func (e *Engine) Stop() error {
	if e == nil || e.k == nil {
		return ErrKitRequired
	}
	return e.k.Stop()
}
