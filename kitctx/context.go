// Package kitctx 提供 agentkit 的请求上下文、轨迹与注入点契约。
//
// 本包是 本包 的基础层，**零宿主依赖**：只使用 Go 标准库。
// 任何注入接口的签名只允许使用 stdlib context.Context，绝不出现 magdefined.Context
// （宿主侧 侧在 宿主接入层 内以 per-request 闭包捕获 magdefined.Context 后包装注入，ADR-004）。
//
// 角色（Role）只作为上下文载体字段存在，不存在「role 作为普通函数参数逐层传递」的形态。
package kitctx

import (
	"context"
	"encoding/json"
	"time"
)

// Role 是调用者角色标识。
//
// 取值集合由宿主应用定义（宿主侧 侧为服务端用户体系角色，不信任请求参数中的角色字段）。
// 角色**只**从上下文载体取得，不得作为普通函数参数逐层传递。
type Role string

// Limits 是一次请求的执行预算，由宿主应用从配置注入（宿主侧 侧来源 config/llm.go）。
type Limits struct {
	// MaxSteps 是单次请求允许的工具调用步数上限；达到上限后循环必须终止并显式告知。
	MaxSteps int `json:"max_steps"`
	// StepTimeout 是单次工具调用的独立超时；并行调度时每个调用各带一份。
	// ≤0 表示不设独立超时 —— 本包不替调用方补默认值，补默认值属配置层职责。
	StepTimeout time.Duration `json:"step_timeout"`
}

// MemoryHandle 是会话记忆的显式句柄。
//
// 记忆由调用方在请求中携带、随响应回显，引擎自身不持有任何会话状态。
// 本结构只承载「由调用方携带」这一语义与修订号；记忆的四类分组、来源、压缩与冲突规则
// 由 本包/agent 解释，本包刻意不复制其结构（避免同一结构两处定义而漂移）。
type MemoryHandle struct {
	Revision int64           `json:"revision"`
	Payload  json.RawMessage `json:"payload,omitempty"`
}

// RequestContext 是一次请求的全部显式上下文：由调用方携带、可随请求往返，
// 引擎不依赖任何隐含的服务端会话状态。
//
// 该结构可被序列化后回显（宿主侧 侧的口径为 $.data.context）；
// 注入点（Injections）是活对象，不参与序列化。
type RequestContext struct {
	// UserID 是实际发起者的用户标识。
	UserID string `json:"user_id"`
	// Role 是调用者角色；只能经 RoleFrom 从上下文取得。
	Role Role `json:"role"`
	// CallerID 是对外调用方（业务方）标识；产品内链路为空。
	CallerID string `json:"caller_id,omitempty"`
	// RepresentedUser 是被代表用户：对外调用方以其身份行事，数据范围与审计都收敛到该用户；产品内链路为空。
	RepresentedUser string `json:"represented_user,omitempty"`
	// TraceID 是幂等键与回溯键：轨迹只需该值即可重建完整序列。
	TraceID string `json:"trace_id"`
	// Limits 是本次请求的执行预算。
	Limits Limits `json:"limits"`
	// Memory 是调用方携带的会话记忆句柄；新会话为空。
	Memory *MemoryHandle `json:"memory,omitempty"`
	// Injections 是本请求的注入点，不参与序列化。
	Injections Injections `json:"-"`
}

// requestContextKey 是本包私有的上下文键：不对外暴露，避免与他人的键碰撞。
type requestContextKey struct{}

// WithRequest 把显式上下文挂到 ctx 上。ctx 不得为 nil。
//
// 此后调用链任一环都只从这里取角色等身份信息。
func WithRequest(ctx context.Context, rc RequestContext) context.Context {
	return context.WithValue(ctx, requestContextKey{}, rc)
}

// RequestFrom 从 ctx 取回显式上下文；ctx 为 nil 或未挂载时返回 ok=false。
func RequestFrom(ctx context.Context) (RequestContext, bool) {
	if ctx == nil {
		return RequestContext{}, false
	}
	rc, ok := ctx.Value(requestContextKey{}).(RequestContext)
	return rc, ok
}

// RoleFrom 取出调用者角色，且**只**从上下文取。
//
// 未挂载上下文、或角色为空（不可判定）时返回 ok=false。调用方必须把 ok=false 当作
// **不可判定 ⇒ 拒绝**（fail-closed：工具裁剪侧不注册、越权判定侧拒绝），不得据此放行。
func RoleFrom(ctx context.Context) (Role, bool) {
	rc, ok := RequestFrom(ctx)
	if !ok || rc.Role == "" {
		return "", false
	}
	return rc.Role, true
}
