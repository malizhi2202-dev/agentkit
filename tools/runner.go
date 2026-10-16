// runner.go —— 工具调用的并行调度：errgroup 并行 + 每步独立超时 + 未注册/越权拒绝 + 单步失败整体继续。
//
// 硬约束：
//
//  1. 并行只能经 `golang.org/x/sync/errgroup`（ 明文要求；`go.mod` 已有 `// indirect` require，
//     不新增依赖、不跑 `go mod tidy`）。本文件**不存在**裸 `go` 语句 —— 由 `TestRunner_NoBareGoroutine`
//     以 AST 扫描固化。
//  2. 每步带**独立超时**，超时值取自 `kitctx.Limits.StepTimeout`（L-015 契约 3：并行调度时每个调用
//     各带一份；≤0 表示不设独立超时，框架侧**不补默认值** —— 补默认值属配置层职责）。
//  3. 模型请求**未注册**工具 ⇒ 该步被拒绝并记录（`rejected=true` + `status=failed` + 可识别错误），
//     绝不静默忽略、绝不放宽边界。
//  4. **已注册但调用者角色无权** ⇒ 与「未注册」同样拒绝（`rejected=true` + `status=failed`），
//     执行体一次都不进入。为什么必须有这道执行侧复核：
//     `TrimByRole` 只保证「模型看不见」，而模型可能**自发**输出无权工具名（幻觉 / prompt 注入诱导；
//     `宿主接入层` 对 `req.Tool` 的 `IsAdmin` 门禁只覆盖**显式指定**的工具）⇒ 只靠裁剪
//     就等于把安全边界建立在模型行为上。判定**复用注册表的同一个谓词**（`LookupAuthorized` →
//     `Tool.allows`），本文件**不写**任何角色→工具白名单/枚举（第二套口径 = 会漂移的口径）。
//  5. 角色**只从服务端上下文**取（`kitctx.RoleFrom(ctx)`），绝不从模型输出/请求体取；
//     ctx 未挂载请求上下文或角色不可判定 ⇒ role="", `allows("")` 恒 false ⇒ 全部按越权拒绝
//     （fail-closed，**没有**「默认 admin」或「nil 即放行」的兜底）。
//  6. 单步失败 / 超时 / panic ⇒ 只该步失败，**整体继续**：errgroup 的任务恒返回 nil，
//     因此一个错误不会提前中止同批其它调用。
//
// 注入点（Logger / Clock）随请求上下文携带（`kitctx.Injections`），故本类型无状态、无锁。
// 包级说明见 registry.go；本文件不重复声明 package doc。

package tools

import (
	"context"
	"errors"
	"fmt"
	"time"

	"golang.org/x/sync/errgroup"

	"agentkit/kitctx"
	"agentkit/model"
)

// 执行侧的可识别错误。均以 `%w` 包装，调用方用 errors.Is 判定。
var (
	// ErrToolNotRegistered 表示模型请求调用了一个不在注册表中的工具：该步被拒绝并记录。
	ErrToolNotRegistered = errors.New("tools: 未注册的工具（模型请求了注册表之外的工具）")
	// ErrToolForbidden 表示工具**已注册**但调用者角色无权调用它：与未注册同样拒绝，但**错误可区分**
	// （审计/定位要能分辨「幻觉出的工具名」与「真实存在、只是越权」），且执行体一次都不进入
	// 。
	ErrToolForbidden = errors.New("tools: 已注册但调用者角色无权调用（越权调用被拒）")
	// ErrToolTimeout 表示该步超出独立超时；**始终同时**包装 context.DeadlineExceeded，两种 errors.Is 都成立。
	ErrToolTimeout = errors.New("tools: 工具调用超时")
	// ErrToolPanic 表示执行体 panic：只判该步失败，其余调用照常。
	ErrToolPanic = errors.New("tools: 工具执行 panic")
)

// Result 是一次工具调用的执行结果；`Runner.Run` 的返回值与入参 calls **逐位对齐**。
//
// 它同时是「轨迹装配」与「缺口显式化」的数据源：调用方据 Status/Err 记缺口、
// 据 Rejected 记拒绝、据 Value 构造回填给模型的 tool 消息。
type Result struct {
	// Call 是本次调用的原始请求（含模型给出的调用 ID 与工具名，便于回填 tool 消息）。
	Call model.ToolCall
	// Status 只可能是 done 或 failed：runner 不在结果里暴露 running 中间态。
	Status kitctx.StepStatus
	// Value 是执行体的返回值；拒绝 / 超时 / 失败时一律为 nil（**不得**把超时结果当成功结果）。
	Value any
	// Err 是失败原因：未注册 ⇒ 包装 ErrToolNotRegistered；已注册但无权 ⇒ 包装 ErrToolForbidden；
	// 超时 ⇒ 同时包装 ErrToolTimeout 与 context.DeadlineExceeded；panic ⇒ 包装 ErrToolPanic；
	// 其它 ⇒ 执行体原始错误（含参数解析错误）。
	Err error
	// Rejected 表示「该调用被拒绝执行」（未注册工具 ，或已注册但调用者角色无权）。
	// 恒等于 errors.Is(Err, ErrToolNotRegistered) || errors.Is(Err, ErrToolForbidden)；
	// 单独给出字段是为了让落日志/落轨迹侧不必先做错误判定（两种拒绝用 Err 与 reason token 区分）。
	Rejected bool
	// Elapsed 是该步耗时（经注入 Clock 取值，见 kitctx.Clock）。
	Elapsed time.Duration
}

// Runner 按 errgroup 并行调度模型请求的工具调用。
//
// 无状态：注册表自带 RWMutex（可并发读），注入点随请求 ctx 携带。
type Runner struct {
	registry *Registry
}

// NewRunner 绑定一个注册表。
//
// registry 为 nil 时**不 panic**也不放行：所有调用一律按「未注册」拒绝（fail-closed，
// 与 Registry.Lookup 的 nil 容忍一致）。
//
// 调用者的**角色不在构造期传入**：Runner 被 `agent.Loop` 长驻复用，而角色是**每请求**的
// （：只从 `ctx` 取）。故角色在 `Run` 内由 `kitctx.RoleFrom(ctx)` 读一次。
func NewRunner(registry *Registry) *Runner { return &Runner{registry: registry} }

// Run 并行执行一批模型请求的工具调用，返回与 calls **逐位对齐**的结果。
//
// 语义（逐条对应 AC）：
//   - 每个已注册且**调用者有权**的调用各起一个 errgroup 任务、各带独立超时；
//   - 未注册 ⇒ 派发阶段即拒绝；已注册但调用者角色无权 ⇒ 同样拒绝且不进入执行体
//     ；两者都不进任何执行体；
//   - 任一步失败 / 超时 / panic ⇒ 该步 failed，其余步骤照常执行完毕；
//   - 全部落定后按入参顺序逐条写日志：日志顺序确定，且只落稳定 reason token（不落错误原文，NFR-S6 同精神）。
//
// 刻意**不返回聚合 error**：单步结果即结论，聚合错误会诱导调用方「一步失败即整单失败」。
func (r *Runner) Run(ctx context.Context, calls []model.ToolCall) []Result {
	if len(calls) == 0 {
		return nil
	}
	logger := kitctx.LoggerFrom(ctx)
	clock := kitctx.ClockFrom(ctx)
	stepTimeout := stepTimeoutFrom(ctx)
	// 角色**只从服务端上下文取**，请求期内不可变 ⇒ 本批次只读一次：
	// 绝不用「默认 admin」之类兜底 —— 取不到就是不可判定（role="")，`allows("")` 恒 false ⇒ 全部越权拒绝。
	role, _ := kitctx.RoleFrom(ctx)

	// 惰性取注册表：r 为 nil（或 registry 为 nil）时 Lookup 全部落空 ⇒ fail-closed。
	var registry *Registry
	if r != nil {
		registry = r.registry
	}

	// 预分配等长切片：每个任务只写**自己那一位**，无需二次加锁（-race 下成立）。
	results := make([]Result, len(calls))

	var group errgroup.Group
	for i, call := range calls {
		idx, request := i, call
		// 执行侧的**唯一授权判定**：registered/already-visible 都从注册表的同一份数据与同一个
		// `allows` 谓词得出（`LookupAuthorized`），本文件不持有任何角色→工具清单。
		tool, registered, allowed := registry.LookupAuthorized(request.Function.Name, string(role))
		switch {
		case !registered:
			results[idx] = rejectedResult(request,
				fmt.Errorf("%w: name=%q", ErrToolNotRegistered, request.Function.Name))
			continue
		case !allowed:
			// 已注册但该角色无权：拒绝，**绝不**降级成空结果、也绝不执行。
			results[idx] = rejectedResult(request,
				fmt.Errorf("%w: name=%q role=%q", ErrToolForbidden, request.Function.Name, role))
			continue
		}
		group.Go(func() error {
			results[idx] = runStep(ctx, tool, request, stepTimeout, clock)
			return nil // 刻意恒 nil：单步失败不得提前中止同批其它调用（契约点）
		})
	}
	// Wait 返回即代表所有任务已回收（errgroup 的生命周期保证）。返回值恒为 nil，理由见上。
	_ = group.Wait()

	reportSteps(logger, results)
	return results
}

// rejectedResult 构造一条「该调用被拒绝执行」的结果：`status=failed` + `Value=nil` + `Rejected=true`。
//
// 「未注册」与「已注册但调用者角色无权」共用这一构造 ——
// 两者的**外形与处置一致**（拒绝、不执行、记录），差别只在 `Err`（供审计区分是哪一种拒绝）：
// 共用构造可避免两处各写一遍「拒绝结果长什么样」而在后续修改中漂移。
func rejectedResult(request model.ToolCall, err error) Result {
	return Result{Call: request, Status: kitctx.StepStatusFailed, Err: err, Rejected: true}
}

// runStep 执行单步：参数解析 → 独立超时 → 执行 → 结果归类。
//
// 任何形态的失败都只落到本步返回值上：函数本身不 panic、不返回聚合错误。
// 每步耗时一律经注入 Clock 取值（kitctx 契约：一切耗时经 Clock，测试可确定性复现）。
//
// 超时是**协作式**的：独立超时经 context 传递，执行体必须尊重 `ctx`（既有 `host_biz/**` 同款用法）。
// 刻意不做「硬性放弃」——那需要把已超时的执行体丢在孤儿 goroutine 里，恰好违反  的
// 「不裸起 goroutine（无回收）」口径；执行体不尊重 ctx 属执行体缺陷（由 capability 侧守卫）。
func runStep(ctx context.Context, tool Tool, call model.ToolCall, stepTimeout time.Duration, clock kitctx.Clock) (result Result) {
	result = Result{Call: call, Status: kitctx.StepStatusFailed}

	// 最先登记 panic 兜底（defer 后进先出 ⇒ 它最后执行，但仍在同一栈帧内完成 recover）：
	// 执行体 panic 只判该步失败，不拖垮整批、不拖垮进程。
	defer func() {
		if p := recover(); p != nil {
			result.Value = nil
			result.Err = fmt.Errorf("%w: name=%q: %v", ErrToolPanic, call.Function.Name, p)
		}
	}()

	args, err := call.Args()
	if err != nil {
		result.Err = err // 「模型给了坏参数」是执行失败，不是「未注册」（契约点 只管后者）
		return result
	}

	start := clock.Now()
	defer func() { result.Elapsed = clock.Now().Sub(start) }()

	stepCtx := ctx
	if stepTimeout > 0 {
		var cancel context.CancelFunc
		stepCtx, cancel = context.WithTimeout(ctx, stepTimeout)
		defer cancel() // 每个调用各带一份，任务退出前必回收（无定时器泄漏）
	}

	value, err := tool.Run(stepCtx, args)
	switch {
	case errors.Is(stepCtx.Err(), context.DeadlineExceeded):
		// 超时**一律**判失败：即便执行体赶在返回前把结果算完了，也不得把超时当成功。
		result.Value = nil
		result.Err = fmt.Errorf("%w: %w: name=%q", ErrToolTimeout, context.DeadlineExceeded, call.Function.Name)
	case err != nil:
		result.Value = nil
		result.Err = err
	default:
		result.Status = kitctx.StepStatusDone
		result.Value = value
	}
	return result
}

// stepTimeoutFrom 从请求上下文取本请求的步超时（kitctx.Limits.StepTimeout，L-015 契约 3）。
//
// 未挂载上下文、或值 ≤0 ⇒ 0（不设独立超时）：框架侧不替配置层补默认值。
func stepTimeoutFrom(ctx context.Context) time.Duration {
	rc, ok := kitctx.RequestFrom(ctx)
	if !ok {
		return 0
	}
	return rc.Limits.StepTimeout
}

// reportSteps 按入参顺序写「一步一条」日志，供  的「被拒绝并记录」与失败可观测。
//
// 只落稳定 reason token（`tool.not_registered` / `tool.forbidden` / `tool.timeout` / `tool.panic` /
// `tool.failed`），不落错误原文与参数原文：避免工具结果或下游报错里的敏感内容被带进日志（NFR-S6 同精神）。
// 「未注册」与「已注册但越权」用**不同 token**：两者的处置动作不同（前者多半是幻觉工具名，后者是
// 真实的越权尝试，需要审计定位）。
func reportSteps(logger kitctx.Logger, results []Result) {
	for _, result := range results {
		name := result.Call.Function.Name
		switch {
		case result.Rejected:
			reason := "tool.not_registered"
			if errors.Is(result.Err, ErrToolForbidden) {
				reason = "tool.forbidden"
			}
			logger.Warn("engine.tool.rejected", map[string]any{
				"tool": name, "rejected": true, "reason": reason,
			})
		case errors.Is(result.Err, ErrToolTimeout):
			logger.Warn("engine.tool.failed", map[string]any{"tool": name, "reason": "tool.timeout"})
		case errors.Is(result.Err, ErrToolPanic):
			logger.Warn("engine.tool.failed", map[string]any{"tool": name, "reason": "tool.panic"})
		case result.Status != kitctx.StepStatusDone:
			logger.Warn("engine.tool.failed", map[string]any{"tool": name, "reason": "tool.failed"})
		}
	}
}
