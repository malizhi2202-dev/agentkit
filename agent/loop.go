// loop.go —— agent 调用循环：模型自选工具 / 步数上限 / 降级 / 轨迹。
//
// 状态机（，本文件是其唯一实现）：
//
//	idle → planning → [tool_call → running → done|failed] …(≤MaxSteps) → verdict(可选) → assembled
//
//   - planning：按角色裁剪后的工具声明 + 消息序列出域请求模型（可为零工具 = 纯对话）；
//   - tool_call/running/done|failed：模型点名的**一批**工具经  的 Runner 并行执行，
//     结果按调用序写入轨迹；failed 记缺口但**整体继续**，rejected 记 failed 并留痕
//     ；
//   - verdict：模型不再请求工具时取其文本为模型段（内容为空则不产生模型段）；
//   - assembled：本层只产出 LoopOutcome，装配（事实段/缺口/报告）归调用方。
//
// 硬约束：
//
//  1. **调度完全由模型决定**：本文件不存在任何工具名字面量、不存在预设调用序列；每轮调哪几个工具、
//     什么顺序、调几次，只来自模型返回的 `ToolCalls`。
//  2. **预算只来自 `kitctx.Limits`，框架不补默认值**（L-015 契约 3 / L-024 契约 1）：
//     步数上限 = `Limits.MaxSteps`（**≤0 = 不设上限**，非零默认值归配置层 `config/llm.go`）；
//     每步超时 = `Limits.StepTimeout`（**≤0 = 不设独立超时**，由  的 Runner 落实）。
//  3. **降级可识别、绝不当成功**：模型不可用/超时/未配置端点 ⇒ `Degraded=true` +
//     逐字文案 + 可 `errors.Is` 的稳定哨兵，跳过 verdict 直接 assembled；请求整体不阻塞（不返回 error）。
//  4. **轨迹写入归本层且 Seq 确定**（L-026 裁决 1）：Runner 只返回**逐位同序**的结果，本层在
//     `Runner.Run` 返回后**按调用位置**逐条 `Trace.Record` ⇒ 并行之下 Seq 仍等于调用序，与完成序无关。
//  5. **出域策略拒绝不伪装成模型不可用**（ / L-016）：端点白名单/禁跟随重定向类哨兵
//     原样上抛（循环中止、`Degraded=false`），调用方据此记 `LOG:llm.endpoint.rejected`。
//
// 包级说明见 result.go / memory.go；本文件不重复声明 package doc。

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"agentkit/kitctx"
	"agentkit/model"
	"agentkit/tools"
)

// 用户可见文案（**逐字冻结**： / ）。单一来源，调用方不得再抄一份。
const (
	// NoticeStepsLimited 是步数到达上限时的提示。
	NoticeStepsLimited = "已达工具调用步数上限"
	// NoticeModelUnavailable 是模型降级时的提示。
	NoticeModelUnavailable = "模型不可用，未生成结论"
)

// 轨迹与缺口的 reason 词表（稳定 token，与  的失败日志同一套词）：
// 只落 token，不落错误原文与参数原文（NFR-S6 同精神）。
const (
	reasonToolNotRegistered = "tool.not_registered"
	// reasonToolForbidden 用于「工具已注册但调用者角色无权」：与 not_registered 分开，
	// 是因为两者的处置动作不同（前者多半是模型幻觉出的工具名，后者是真实的越权尝试）。
	// 判定与拒绝都在  的 Runner 内完成（复用注册表的同一个 allows 谓词），本层只做归类。
	reasonToolForbidden = "tool.forbidden"
	reasonToolTimeout   = "tool.timeout"
	reasonToolPanic     = "tool.panic"
	reasonToolFailed    = "tool.failed"
	// reasonToolUnserializable 用于「执行体成功但结果无法序列化」这一执行期缺陷：
	// 送不进模型/日志/缺口的结果不得在任何一处呈现为成功（不得静默成空结果）。
	reasonToolUnserializable = "tool.unserializable"
	// reasonMaxStepsUndetermined 用于「步数预算未判定（MaxSteps<=0 ⇒ 不设上限）」这一**成本控制缺失**信号
	// （L-029 契约 5：配置层必须给非零值，框架既不补默认值、也不静默不限步）。
	reasonMaxStepsUndetermined = "max_steps_undetermined"
)

// unnamedTool 是「模型给出空工具名」时轨迹里的占位名。
//
// kitctx.Trace 拒绝无名步骤，而  要求被拒绝的调用**被记录**（不得静默忽略）⇒
// 用占位名记录该步，既不丢步、也不把空名伪装成某个真实工具。
const unnamedTool = "unknown"

// 循环级可识别错误：只用于「循环根本没开始（未出域）」的 fail-closed 拒绝 —— 不是模型失败，故不降级。
var (
	// ErrRoleUnresolved 角色不可判定（未挂载请求上下文 / 角色为空）⇒ 工具集无从裁剪，拒绝进入循环
	// （ 工具裁剪侧 fail-closed 第三侧）。调用方据此走 401，不得降级放行。
	ErrRoleUnresolved = errors.New("agent.role.unresolved: 角色不可判定，拒绝进入调用循环（fail-closed）")
	// ErrTraceIDRequired 缺 traceid ⇒ 轨迹不可回放，拒绝进入循环而不是静默不记录。
	ErrTraceIDRequired = errors.New("agent.trace_id.required: 缺少 traceid，轨迹无法回放，拒绝进入调用循环")
)

// LoopConfig 是调用循环的注入输入（框架不读任何配置： / D1）。
type LoopConfig struct {
	// Registry 是工具注册表：每轮按角色裁剪后产出**模型可见**的工具声明。
	// nil ⇒ 模型看不到任何工具（fail-closed），循环仍可进行纯对话。
	Registry *tools.Registry
	// Client 是框架内唯一的出域客户端。nil ⇒ 按「模型未配置」降级，不 panic。
	Client *model.Client
	// Endpoint 是本次出域端点，由调用方从配置注入（ 契约：端点随调用传入，Client 不读配置）。
	// 空串 ⇒ 端点策略判 `endpoint.empty` ⇒ 降级。
	Endpoint string
}

// Loop 是调用循环执行器。**无状态**：注册表只读、客户端只读、每请求状态全在 Run 的返回值里，
// 因此可被多 goroutine 并发复用（轨迹按请求各建一条，不共享）。
type Loop struct {
	registry *tools.Registry
	client   *model.Client
	endpoint string
	runner   *tools.Runner
}

// NewLoop 固化注入的工具集与出域通道。
func NewLoop(cfg LoopConfig) *Loop {
	return &Loop{
		registry: cfg.Registry,
		client:   cfg.Client,
		endpoint: cfg.Endpoint,
		runner:   tools.NewRunner(cfg.Registry),
	}
}

// LoopOutcome 是一次调用循环的产出：装配（事实段/缺口/报告）之前**全部可观察事实**。
type LoopOutcome struct {
	// Verdict 是模型给出的最终文本（结论段的唯一来源）。
	// 降级或模型未给文本时为空 ⇒ 调用方据此不产生模型段。
	Verdict string
	// Steps 是**实际发生**的工具调用轨迹，按调用序、Seq 从 1 连续。
	Steps []kitctx.Step
	// Gaps 是「已注册但执行失败」的步骤缺口（source = 工具名，reason = 稳定 token）。
	// 被拒绝的步骤**不**计入。
	Gaps []GapEntry
	// Rejected 是被拒绝执行的工具名（按调用序； 的册外工具，**以及**执行侧判定为
	// 「已注册但该角色无权」的工具）。两类都在 `Steps[].ResultSummary` 里带稳定 reason token 区分。
	Rejected []string
	// StepsLimited 表示循环因步数上限而终止。
	StepsLimited bool
	// Degraded 表示模型不可用/超时/未配置 ⇒ 未生成模型段，但已发生的结构化结果照常。
	Degraded bool
	// Notice 是给用户看的冻结文案：StepsLimited ⇒ NoticeStepsLimited；Degraded ⇒ NoticeModelUnavailable。
	Notice string
	// Err 是降级或中止的原因（可 errors.Is 到 model / tools 的稳定哨兵）；正常收口时为 nil。
	Err error
	// Messages 是循环结束时的完整消息序列（含 assistant 与 tool 消息），供上下文回显。
	Messages []model.ChatMessage
}

// Run 执行一次调用循环。
//
// 形参 messages 是**已经装配好的**初始消息序列（system + user；记忆/历史的围栏与脱敏由调用方在
// 出域出口完成，本层不做也不得不做 —— 框架只负责调度、预算、降级与轨迹）。
//
// 返回的 error **只**表示「循环没有跑起来（未出域）」：
//   - ErrRoleUnresolved：角色不可判定 ⇒ fail-closed 拒绝（工具集无从裁剪）；
//   - ErrTraceIDRequired：缺 traceid ⇒ 轨迹不可回放，不得静默不记录；
//   - 出域策略拒绝（端点白名单 / 禁跟随重定向等）⇒ 原样上抛 / 哨兵，**不伪装成模型不可用**。
//
// 模型侧的不可用/超时**不**走 error：按 /5 落 `Degraded=true` + 冻结文案，请求整体不阻塞。
func (l *Loop) Run(ctx context.Context, messages []model.ChatMessage) (LoopOutcome, error) {
	rc, ok := kitctx.RequestFrom(ctx)
	if !ok {
		return LoopOutcome{}, ErrRoleUnresolved
	}
	role, ok := kitctx.RoleFrom(ctx)
	if !ok {
		return LoopOutcome{}, ErrRoleUnresolved
	}
	if rc.TraceID == "" {
		return LoopOutcome{}, ErrTraceIDRequired
	}

	logger := kitctx.LoggerFrom(ctx)
	trace := kitctx.NewTrace(rc.TraceID)
	maxSteps := rc.Limits.MaxSteps

	// 预算未判定（MaxSteps<=0 ⇒ 不设上限，语义见 planBatch）：循环只受模型自身行为约束，
	// 这是**成本**控制缺失 —— 必须留一条可 grep 的安全信号，不得静默不限步
	// （否则配置层漏传 0 时，生产表现是「偶发巨额模型调用账单」而无人察觉；L-029 契约 5）。
	// 每请求只记一次（不是每轮），字段只落稳定 token 与配置数值，不落任何数据原文。
	if maxSteps <= 0 {
		logger.Warn("llm.loop.unbounded", map[string]any{
			"trace_id":  rc.TraceID,
			"reason":    reasonMaxStepsUndetermined,
			"max_steps": maxSteps,
		})
	}

	out := LoopOutcome{Messages: append([]model.ChatMessage(nil), messages...)}
	// 角色在请求期内不可变 ⇒ 工具声明**每请求裁剪一次**：多轮之间集合稳定，也不重复占用注册表锁。
	visible := l.visibleTools(string(role))
	remaining := maxSteps

	for {
		resp, err := l.complete(ctx, out.Messages, visible)
		if err != nil {
			return l.abort(out, trace, logger, err)
		}
		// Complete 保证候选非空（否则返回 ErrModelNoChoices），故此处下标安全。
		choice := resp.Choices[0]
		calls := choice.Message.ToolCalls
		if len(calls) == 0 {
			// 模型不再请求工具 ⇒ 进入 verdict（文本为空则不产生模型段）。
			out.Verdict = choice.Message.Content
			return out, nil
		}

		batch, limited, exhausted := planBatch(calls, maxSteps, remaining)
		if exhausted {
			// 预算已耗尽而模型仍在请求工具：不执行本轮，直接以「已达上限」收口。
			return stepsLimited(out), nil
		}

		results := l.runner.Run(ctx, batch)
		for i := range results {
			results[i] = normalizeResult(results[i]) // 「送不出去的结果」不得被当成功（轨迹/缺口/回填三处同口径）
		}
		out.record(trace, logger, results) // 按位置 Record ⇒ Seq 与调用序一致（L-026 裁决 1）
		out.Messages = append(out.Messages, choice.Message)
		for _, result := range results {
			out.Messages = append(out.Messages, toolMessage(result))
		}
		if limited {
			return stepsLimited(out), nil
		}
		if maxSteps > 0 {
			remaining -= len(batch)
		}
	}
}

// complete 发起一次出域请求（客户端未注入 ⇒ 按「模型未配置」返回可识别哨兵，不 panic）。
func (l *Loop) complete(ctx context.Context, messages []model.ChatMessage, visible []model.ToolVisible) (*model.ChatResponse, error) {
	if l == nil || l.client == nil {
		return nil, model.ErrClientNotConfigured
	}
	return l.client.Complete(ctx, l.endpoint, model.ChatRequest{Messages: messages, Tools: visible})
}

// visibleTools 取本请求角色下**可见**的工具声明。
func (l *Loop) visibleTools(role string) []model.ToolVisible {
	if l == nil {
		return nil
	}
	return l.registry.VisibleByRole(role)
}

// abort 收口一次失败的出域调用：
//   - 模型侧不可用/超时/取消/未配置（含「端点未配置」）⇒ **降级**，记 `LOG:llm.error{reason}`；
//   - 其余（端点白名单拒绝、禁跟随重定向、编码失败等）⇒ 中止并把哨兵原样上抛，
//     **不伪装成模型不可用**（：把配置错误变成静默降级会掩盖真实故障）。
func (l *Loop) abort(out LoopOutcome, trace *kitctx.Trace, logger kitctx.Logger, err error) (LoopOutcome, error) {
	out.Err = err
	if !isDegradable(err) {
		logger.Warn("llm.endpoint.rejected", map[string]any{
			"trace_id": trace.TraceID(), "reason": reasonToken(err),
		})
		return out, err
	}
	out.Degraded = true
	out.Notice = NoticeModelUnavailable
	logger.Error("llm.error", map[string]any{
		"trace_id": trace.TraceID(), "reason": reasonToken(err),
	})
	return out, nil
}

// stepsLimited 以「已达工具调用步数上限」收口。
func stepsLimited(out LoopOutcome) LoopOutcome {
	out.StepsLimited = true
	out.Notice = NoticeStepsLimited
	return out
}

// planBatch 按剩余预算裁剪本轮模型请求的工具调用（**一步 = 一次工具调用**，对齐  的
// `[tool_call → running → done|failed] …(≤maxSteps)` 与 NFR-P4 的「工具调用步数」口径）。
//
// 返回：
//   - batch：本轮真正会被执行的调用（其余丢弃 —— 未执行的调用**不进轨迹**，轨迹只记实际发生的事）；
//   - limited：本轮被预算截断（执行完 batch 后必须以「已达上限」收口）；
//   - exhausted：预算在**进入本轮之前**已耗尽（本轮一个都不执行，直接收口）。
//
// maxSteps <= 0 表示**不设上限**：框架不替配置层补默认值（L-015 契约 3 / L-024 契约 1）——
// 因此**非零默认值必须由 config/llm.go 提供**，否则循环只受模型自身行为约束。
func planBatch(calls []model.ToolCall, maxSteps, remaining int) (batch []model.ToolCall, limited, exhausted bool) {
	if maxSteps <= 0 {
		return calls, false, false
	}
	if remaining <= 0 {
		return nil, false, true
	}
	if len(calls) > remaining {
		return calls[:remaining], true, false
	}
	return calls, false, false
}

// record 把一批**逐位同序**的执行结果按调用位置写入轨迹（L-026 裁决 1），并派生缺口/拒绝清单。
//
// 每步一条 `LOG:engine.trail`（宿主侧 侧即 `LOG:engine.trail[]` 的一行）：只落哈希摘要与稳定 token，
// 因此「同一 traceid 可重建序列」与「日志不落原文」同时成立。
func (out *LoopOutcome) record(trace *kitctx.Trace, logger kitctx.Logger, results []tools.Result) {
	for _, result := range results {
		step, err := trace.Record(kitctx.Step{
			Name:          traceName(result),
			Params:        summarizeArgs(result.Call),
			Status:        result.Status,
			Elapsed:       result.Elapsed,
			ResultSummary: summarizeResult(result),
		})
		if err != nil {
			// 预检已保证 traceid 存在；此处只可能因结果自身不合法而失败 —— 记录但不静默丢步。
			logger.Warn("engine.trail.record_failed", map[string]any{"reason": reasonToken(err)})
			continue
		}
		out.Steps = append(out.Steps, step)
		if result.Rejected {
			out.Rejected = append(out.Rejected, step.Name)
		} else if result.Status != kitctx.StepStatusDone {
			out.Gaps = append(out.Gaps, GapEntry{Source: step.Name, Reason: stepReason(result)})
		}
		logger.Info("engine.trail", map[string]any{
			"trace_id":   step.TraceID,
			"seq":        step.Seq,
			"tool":       step.Name,
			"status":     string(step.Status),
			"elapsed_ms": step.Elapsed.Milliseconds(),
			"params":     step.Params,
			"result":     step.ResultSummary,
		})
	}
}

// isDegradable 判定「引擎应降级」的失败类别。
//
// 组成： 的 8 个模型侧哨兵 +  的 `endpoint.empty`（**未配置端点 = 零配置启动路径，必须降级而非报错**）。
// 其余一律不降级：端点白名单/重定向类拒绝是配置或安全事件（加载期即拒），编码失败是程序缺陷 ——
// 把它们降级成「模型不可用」会让真实故障静默消失。
func isDegradable(err error) bool {
	for _, sentinel := range []error{
		model.ErrClientNotConfigured,
		model.ErrModelUnavailable,
		model.ErrModelTimeout,
		model.ErrModelCanceled,
		model.ErrModelStatus,
		model.ErrModelMalformed,
		model.ErrModelTooLarge,
		model.ErrModelNoChoices,
		model.ErrEndpointEmpty,
	} {
		if errors.Is(err, sentinel) {
			return true
		}
	}
	return false
}

// reasonToken 取错误的**稳定文本 token 前缀**（`model.unavailable` / `endpoint.public` …）作为日志 reason。
//
// 口径来自 / 的哨兵约定（文本以稳定标识开头 ⇒ 既可用 errors.Is 判定，也可直接落日志，L-024/L-016）。
func reasonToken(err error) string {
	if err == nil {
		return "tool.failed"
	}
	text := err.Error()
	if i := strings.IndexAny(text, ": "); i > 0 {
		return text[:i]
	}
	return text
}

// traceName 取落轨迹的工具名；空名（模型给出畸形调用）用占位名代替，避免丢步。
func traceName(result tools.Result) string {
	if name := strings.TrimSpace(result.Call.Function.Name); name != "" {
		return name
	}
	return unnamedTool
}

// stepReason 把一次执行结果归类为稳定 token（与  的失败日志同一套词）。
func stepReason(result tools.Result) string {
	switch {
	case errors.Is(result.Err, tools.ErrToolForbidden):
		// 已注册但越权：必须在轨迹里与「未注册」区分（审计要能分辨幻觉工具名与真实越权尝试）。
		return reasonToolForbidden
	case result.Rejected:
		return reasonToolNotRegistered
	case errors.Is(result.Err, tools.ErrToolTimeout):
		return reasonToolTimeout
	case errors.Is(result.Err, tools.ErrToolPanic):
		return reasonToolPanic
	case errors.Is(result.Err, errToolUnserializable):
		return reasonToolUnserializable
	default:
		return reasonToolFailed
	}
}

// errToolUnserializable 标记「执行体成功但结果无法序列化」，供 stepReason 归类为 tool.unserializable。
var errToolUnserializable = errors.New("agent: 工具结果无法序列化为 JSON")

// normalizeResult 把「执行体成功但结果无法序列化」规范为**失败**。
//
// 理由：这类结果既回填不了模型、也留不下证据。若仍呈报 done，就是「把送不出去的结果当成功」
// 。规范化在 Run 进入轨迹与回填**之前**统一发生，
// 保证轨迹状态、缺口、回填消息三处口径一致。
func normalizeResult(result tools.Result) tools.Result {
	if result.Status != kitctx.StepStatusDone {
		return result
	}
	if _, err := json.Marshal(result.Value); err != nil {
		result.Status = kitctx.StepStatusFailed
		result.Value = nil
		result.Err = fmt.Errorf("%w: %v", errToolUnserializable, err)
	}
	return result
}

// summarizeArgs 产出**可落日志**的参数摘要：整串哈希 + 每个参数的**形态**（类型/长度/条数），
// 不含任何取值原文（「参数/结果摘要只落哈希或结构化白名单字段」）。
func summarizeArgs(call model.ToolCall) string {
	args, err := call.Args()
	if err != nil {
		return `{"invalid":true}`
	}
	raw, err := json.Marshal(args) // map ⇒ 键有序，哈希与输入顺序无关
	if err != nil {
		return `{"invalid":true}`
	}
	shape := make(map[string]string, len(args))
	for key, value := range args {
		shape[key] = valueShape(value)
	}
	text, err := json.Marshal(argSummary{Hash: shortHash(raw), Shape: shape})
	if err != nil {
		return `{"invalid":true}`
	}
	return string(text)
}

// argSummary 是参数摘要的落日志形态。
type argSummary struct {
	// Hash 是规范化参数 JSON 的短哈希：同一参数两次调用逐字节相同（确定性），且不可反推原文。
	Hash string `json:"sha256"`
	// Shape 是参数名 → 值形态（不含取值）。
	Shape map[string]string `json:"shape,omitempty"`
}

// valueShape 描述一个参数值的形态：类型 + 长度/条数（不落取值）。
func valueShape(value any) string {
	switch v := value.(type) {
	case nil:
		return "null"
	case string:
		return fmt.Sprintf("string:%d", len(v))
	case bool:
		return "bool"
	case float64, float32, int, int64, int32, json.Number:
		return "number"
	case []any:
		return fmt.Sprintf("array:%d", len(v))
	case map[string]any:
		return fmt.Sprintf("object:%d", len(v))
	default:
		return fmt.Sprintf("%T", v)
	}
}

// summarizeResult 产出**可落日志**的结果摘要：状态 + 失败 token，成功时附结果哈希与字节数。
func summarizeResult(result tools.Result) string {
	summary := resultSummary{Status: string(result.Status)}
	if result.Status != kitctx.StepStatusDone {
		summary.Reason = stepReason(result)
	} else if raw, err := json.Marshal(result.Value); err == nil {
		summary.Hash, summary.Bytes = shortHash(raw), len(raw)
	} else {
		summary.Status, summary.Reason = string(kitctx.StepStatusFailed), reasonToolUnserializable
	}
	text, err := json.Marshal(summary)
	if err != nil {
		return `{"status":"failed","reason":"` + reasonToolUnserializable + `"}`
	}
	return string(text)
}

// resultSummary 是结果摘要的落日志形态。
type resultSummary struct {
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
	Hash   string `json:"sha256,omitempty"`
	Bytes  int    `json:"bytes,omitempty"`
}

// shortHash 取规范化 JSON 的 SHA-256 前 6 字节（12 个十六进制字符）。
func shortHash(raw []byte) string {
	// 同一口径只有一处实现（`kitctx.Digest`），这里只截前 6 字节长度，行为与改造前逐字节一致。
	return kitctx.Digest(raw)[:12]
}

// toolMessage 把单步结果回填成模型可读的 tool 消息。
//
// **失败与拒绝必须显式标注**（status/reason），绝不让「超时 / 未注册」在模型眼里看起来像成功
// ；同时只回填稳定 token 作为原因，不回填下游错误原文。
func toolMessage(result tools.Result) model.ChatMessage {
	body := toolResultBody{Status: string(result.Status)}
	switch {
	case result.Status != kitctx.StepStatusDone:
		body.Reason = stepReason(result)
	default:
		raw, err := json.Marshal(result.Value)
		if err != nil {
			body.Status, body.Reason = string(kitctx.StepStatusFailed), reasonToolUnserializable
			break
		}
		body.Value = raw
	}
	text, err := json.Marshal(body)
	if err != nil {
		text = []byte(`{"status":"failed","reason":"` + reasonToolUnserializable + `"}`)
	}
	return model.ChatMessage{Role: "tool", ToolCallID: result.Call.ID, Content: string(text)}
}

// toolResultBody 是回填给模型的 tool 消息体（OpenAI function 形式的 content 字符串内 JSON）。
type toolResultBody struct {
	Status string          `json:"status"`
	Reason string          `json:"reason,omitempty"`
	Value  json.RawMessage `json:"value,omitempty"`
}
