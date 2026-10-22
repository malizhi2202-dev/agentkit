// Package model 是 agentkit 的模型协议层：定义「与模型交换什么」的类型契约，
// 并提供唯一的出域传输注入点 HTTPDoer。
//
// 硬约束（ / D1 / ）：
//   - 只依赖 stdlib，**不得** import 任何 `宿主 module 前缀/...` 包
//     （整个 module 路径都含 宿主库_宿主 ⇒ 该规则等价于「可整目录复制到另一个 module」）；
//   - 不读配置、不拼端点、不发请求：端点白名单与真实调用分别由同包 EndpointPolicy / Client
//     与出域出口 `既有模型层` 负责。
package model

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
)

// HTTPDoer 是出域 HTTP 传输的注入点；stdlib *http.Client 天然满足该接口。
//
// 之所以做成接口而非直接用 *http.Client（ / D4 / ）：
//  1. 框架零依赖 ⇒ 出域只能用 stdlib，传输实现必须由调用方注入；
//  2. 审计不落原文、只落可交叉核验的证据（sent_hmac / sent_bytes / masker_applied[] 等），
//     要做到「证据与实际 payload 一致」，只能由注入方捕获**实际** payload 复算；
//  3. 自测可用 fake 模拟「不可用 / 超时 / 正常」三态，不打真实网络。
//
// 实现约定：Do 必须原样转发 req（不得改写 req.URL 或更换端点），并且不得吞掉传输错误——
// 端点白名单的运行期二次校验与「模型不可用 ⇒ 降级」判断都依赖调用方看到真实结果。
type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// ChatMessage 是一条模型消息。Role ∈ {system, user, assistant, tool}：
// assistant 消息可携带 ToolCalls；tool 消息用 ToolCallID 回填对应工具的执行结果。
type ChatMessage struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
}

// ChatRequest 是一次出域请求：消息序列 + 本次按角色裁剪后**可见**的工具声明。
//
// Tools 刻意用 ToolVisible（而非内部 ToolDecl）：模型可见外形由类型系统保证恰为
//
//	的四要素，内部字段（RequiredRoles / Source / ResultShape）在 wire 上不可能出现。
type ChatRequest struct {
	// Model 是模型标识（如 "minimax/MiniMax-M3.1-Flash"）。
	//
	// wire 形状沿用 OpenAI chat-completions（见 ToolCall 的说明），其 `model` 字段为**必填**；
	// 框架**不写死**任何模型名（部署选型属调用方/部署配置），故由调用方注入。
	// 留空则该字段不出现在 wire 上（omitempty）—— 面向 OpenAI 兼容端点的调用方**必须**填。
	Model    string        `json:"model,omitempty"`
	Messages []ChatMessage `json:"messages"`
	Tools    []ToolVisible `json:"tools,omitempty"`
}

// ChatResponse 是模型返回体。
type ChatResponse struct {
	Choices []ChatChoice `json:"choices"`
}

// ChatChoice 是一个候选回复；FinishReason == "tool_calls" 表示模型要求调用工具。
type ChatChoice struct {
	Index        int         `json:"index"`
	Message      ChatMessage `json:"message"`
	FinishReason string      `json:"finish_reason,omitempty"`
}

// ToolCall 是模型请求调用某个工具。wire 外形沿用 OpenAI chat-completions 的 function 形式
// （私有化模型服务普遍兼容该形状；模型未选型 D-8，故沿用事实标准而非自创）。
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type,omitempty"`
	Function ToolFunction `json:"function"`
}

// ToolFunction 是模型给出的工具名与参数（参数为 JSON 文本，与事实标准一致）。
type ToolFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// Args 把 Arguments（JSON 文本）解析成参数表。
// 非法 JSON ⇒ 返回可识别错误，**不**静默返回空 map（否则会把「模型给了坏参数」伪装成「无参数」）。
func (c ToolCall) Args() (map[string]any, error) {
	if strings.TrimSpace(c.Function.Arguments) == "" {
		return map[string]any{}, nil
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(c.Function.Arguments), &args); err != nil {
		return nil, fmt.Errorf("工具 %q 的参数不是合法 JSON: %w", c.Function.Name, err)
	}
	return args, nil
}

// ToolDecl 是工具的**内部**声明：
//   - RequiredRoles：所需权限（内部名），供 TrimByRole 裁剪；**不得**作为函数参数逐层传递角色；
//   - Source / ResultShape：溯源与结果装配用的内部信息，模型不可见；
//   - 仅有 Visible 的四要素可以出域。
type ToolDecl struct {
	Name          string
	Params        any
	Summary       string
	RequiredRoles []string
	Source        string
	ResultShape   string
}

// ToolVisible 是「对模型可见」的工具声明外形，恰为 {name,params,summary,role}。
type ToolVisible struct {
	Name    string `json:"name"`
	Params  any    `json:"params"`
	Summary string `json:"summary"`
	Role    string `json:"role"`
}

// Visible 把内部声明映射成模型可见外形。
//
// 映射口径（内部 RequiredRoles 复数集合 → 可见 role 单值）：
//   - 单角色 ⇒ 原样输出（如 {"role":"admin"}）；
//   - 多角色 ⇒ 去重后按字典序以 "," 连接，输出确定性相同（同一工具两次映射逐字节相等，便于 HMAC 复算）；
//   - 未声明角色 ⇒ 空字符串（表示该工具未声明角色要求，裁剪语义由 tools 注册表定义）。
func (d ToolDecl) Visible() ToolVisible {
	return ToolVisible{
		Name:    d.Name,
		Params:  d.Params,
		Summary: d.Summary,
		Role:    joinRoles(d.RequiredRoles),
	}
}

// VisibleDecls 批量映射，nil/空输入返回 nil（wire 上由 omitempty 省略）。
func VisibleDecls(decls []ToolDecl) []ToolVisible {
	if len(decls) == 0 {
		return nil
	}
	out := make([]ToolVisible, 0, len(decls))
	for _, d := range decls {
		out = append(out, d.Visible())
	}
	return out
}

// joinRoles 去重 + 字典序排序，不修改入参。
func joinRoles(roles []string) string {
	if len(roles) == 0 {
		return ""
	}
	seen := make(map[string]struct{}, len(roles))
	uniq := make([]string, 0, len(roles))
	for _, r := range roles {
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}
		if _, ok := seen[r]; ok {
			continue
		}
		seen[r] = struct{}{}
		uniq = append(uniq, r)
	}
	if len(uniq) == 0 {
		return ""
	}
	sort.Strings(uniq)
	return strings.Join(uniq, ",")
}
