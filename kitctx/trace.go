package kitctx

import (
	"errors"
	"sort"
	"sync"
	"time"
)

// StepStatus 是轨迹中单步的状态。
type StepStatus string

const (
	StepStatusRunning StepStatus = "running"
	StepStatusDone    StepStatus = "done"
	StepStatusFailed  StepStatus = "failed"
)

// Valid 报告状态是否为上述三者之一。
func (s StepStatus) Valid() bool {
	switch s {
	case StepStatusRunning, StepStatusDone, StepStatusFailed:
		return true
	}
	return false
}

// Step 是轨迹中的一次工具调用记录。
//
// 该结构即落日志形态（宿主侧 侧口径 LOG:engine.trail[] 的一行）：自带 TraceID 与 Seq，
// 因此**只靠 traceid 过滤 + 按 Seq 排序**即可重建完整序列。
// Params 与 ResultSummary 均为**摘要**（可直接落日志，不含未脱敏原文）。
type Step struct {
	TraceID       string        `json:"trace_id"`
	Seq           int           `json:"seq"`
	Name          string        `json:"name"`
	Params        string        `json:"params"`
	Status        StepStatus    `json:"status"`
	Elapsed       time.Duration `json:"elapsed"`
	ResultSummary string        `json:"result_summary"`
}

var (
	// ErrTraceIDRequired 表示轨迹没有 traceid，无从回放。
	ErrTraceIDRequired = errors.New("kitctx: trace id required")
	// ErrStepNameRequired 表示步骤缺工具名，无法核验。
	ErrStepNameRequired = errors.New("kitctx: step name required")
	// ErrStepStatusInvalid 表示步骤状态不在 {running, done, failed} 内。
	ErrStepStatusInvalid = errors.New("kitctx: invalid step status")
	// ErrTraceIDMismatch 表示步骤带了与本轨迹不一致的 traceid。
	ErrTraceIDMismatch = errors.New("kitctx: step trace id mismatch")
)

// Trace 是**一次**会话的轨迹：只能经 Record 追加，因此序列完整、Seq 连续且可按 traceid 回放。
//
// 并行工具调用会并发 Record，故本类型自身并发安全。
// 一条 Trace 只服务一个 traceid；带其它 traceid 的步骤会被拒绝而非静默混入，否则回放将串号。
type Trace struct {
	traceID string
	mu      sync.Mutex
	steps   []Step
}

// NewTrace 为给定 traceid 建一条轨迹。
func NewTrace(traceID string) *Trace { return &Trace{traceID: traceID} }

// TraceID 返回本轨迹的 traceid。
func (t *Trace) TraceID() string { return t.traceID }

// Record 追加一步，并返回**归一化后的落日志记录**（已由本方法补齐 TraceID 与 Seq）。
//
// 下列情形一律拒绝、不落半成品记录：轨迹缺 traceid、步骤缺 name、状态非法、
// 或步骤自带的 traceid 与本轨迹不一致。
func (t *Trace) Record(step Step) (Step, error) {
	if t.traceID == "" {
		return Step{}, ErrTraceIDRequired
	}
	if step.Name == "" {
		return Step{}, ErrStepNameRequired
	}
	if !step.Status.Valid() {
		return Step{}, ErrStepStatusInvalid
	}
	if step.TraceID != "" && step.TraceID != t.traceID {
		return Step{}, ErrTraceIDMismatch
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	step.TraceID = t.traceID
	step.Seq = len(t.steps) + 1
	t.steps = append(t.steps, step)
	return step, nil
}

// Records 返回轨迹记录的**副本**（按 Seq 升序）。
//
// 返回副本而非内部切片：调用方改动返回值不得影响轨迹本身的可回放性。
func (t *Trace) Records() []Step {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]Step, len(t.steps))
	copy(out, t.steps)
	return out
}

// Replay 从轨迹记录里只靠 traceid 重建完整序列：过滤该 traceid 的记录并按 Seq 升序排序。
//
// traceID 为空、或没有任何匹配记录时返回 ok=false —— 不返回空序列冒充成功。
func Replay(records []Step, traceID string) ([]Step, bool) {
	if traceID == "" {
		return nil, false
	}
	var out []Step
	for _, record := range records {
		if record.TraceID == traceID {
			out = append(out, record)
		}
	}
	if len(out) == 0 {
		return nil, false
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out, true
}
