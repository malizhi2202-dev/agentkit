// Package agent 装配引擎单轮结果：事实性结论与模型生成段数据结构分离、缺口单复分工。
//
// 本包属零依赖框架 agentkit：只用 Go 标准库，不 import 任何
// 宿主 module 前缀/... 包（ 禁动项）。
package agent

import (
	"errors"
	"fmt"
	"time"
)

// generated_by 的取值：事实段由引擎填充，模型段由模型生成。
const (
	GeneratedByEngine = "engine"
	GeneratedByModel  = "model"
)

// SourceRef 是事实性结论的溯源四元组。
type SourceRef struct {
	Capability string    `json:"capability"`
	Source     string    `json:"source"`
	Field      string    `json:"field"`
	FetchedAt  time.Time `json:"fetched_at"`
}

// IsComplete 判定四元组齐备；缺一即不可溯源。
func (s SourceRef) IsComplete() bool {
	return s.Capability != "" && s.Source != "" && s.Field != "" && !s.FetchedAt.IsZero()
}

// ConclusionItem 是结论项七字段 {value, capability, source, field, fetched_at,
// generated_by, confidence}。两类结论在数据结构上区分：
//   - 事实性结论：GeneratedBy=="engine" 且内嵌 SourceRef 非 nil，必须可溯源；
//   - 模型生成段：GeneratedBy=="model" 且内嵌 SourceRef 为 nil，不得伪造溯源。
//
// 内嵌为指针 ⇒ JSON 上两类也不同形：模型项不出现 capability/source/field/fetched_at。
type ConclusionItem struct {
	Value string `json:"value"`
	*SourceRef
	GeneratedBy string  `json:"generated_by"`
	Confidence  float64 `json:"confidence"`
}

// NewFactConclusion 装配一条事实性结论（携带溯源四元组）。
func NewFactConclusion(value string, ref SourceRef, confidence float64) ConclusionItem {
	return ConclusionItem{
		Value:       value,
		SourceRef:   &ref,
		GeneratedBy: GeneratedByEngine,
		Confidence:  confidence,
	}
}

// NewModelConclusion 装配模型生成段（不携带溯源四元组）。
func NewModelConclusion(value string, confidence float64) ConclusionItem {
	return ConclusionItem{
		Value:       value,
		GeneratedBy: GeneratedByModel,
		Confidence:  confidence,
	}
}

// Validate 固化「事实必溯源、模型不伪造溯源」的不变量。
func (c ConclusionItem) Validate() error {
	switch c.GeneratedBy {
	case GeneratedByEngine:
		if c.SourceRef == nil || !c.SourceRef.IsComplete() {
			return fmt.Errorf("agent: 事实性结论缺少溯源四元组 {capability,source,field,fetched_at}: value=%q", c.Value)
		}
	case GeneratedByModel:
		if c.SourceRef != nil {
			return fmt.Errorf("agent: 模型生成段不得携带溯源四元组: value=%q", c.Value)
		}
	default:
		return fmt.Errorf("agent: 未知 generated_by=%q（只允许 %q / %q）",
			c.GeneratedBy, GeneratedByEngine, GeneratedByModel)
	}
	return nil
}

// Gap 是单数缺口：单链路 / 单段顶层的缺口原因。
// 它与 Gaps 语义分工，二者不互相替代：单段原因不展开为逐来源缺口，逐来源缺口也不折叠为单一原因。
type Gap struct {
	Reason string `json:"reason"`
}

// GapEntry 是复数缺口的一项：聚合场景下逐来源的缺口。
type GapEntry struct {
	Source string `json:"source"`
	Reason string `json:"reason"`
}

// Result 是一次请求装配后的结果容器。
type Result struct {
	Facts   []ConclusionItem `json:"facts"`
	Verdict *ConclusionItem  `json:"verdict,omitempty"`
	Gap     *Gap             `json:"gap,omitempty"`
	Gaps    []GapEntry       `json:"gaps,omitempty"`
}

// AddFact 追加一条事实性结论。
func (r *Result) AddFact(value string, ref SourceRef, confidence float64) {
	r.Facts = append(r.Facts, NewFactConclusion(value, ref, confidence))
}

// SetVerdict 装配模型生成段。模型不可用（降级）时不调用 ⇒ JSON 无 model 段。
func (r *Result) SetVerdict(value string, confidence float64) {
	verdict := NewModelConclusion(value, confidence)
	r.Verdict = &verdict
}

// SetGap 记录单链路 / 单段顶层的缺口原因。
func (r *Result) SetGap(reason string) {
	r.Gap = &Gap{Reason: reason}
}

// AddSourceGap 追加聚合场景中某个来源的缺口。
func (r *Result) AddSourceGap(source, reason string) {
	r.Gaps = append(r.Gaps, GapEntry{Source: source, Reason: reason})
}

// Validate 校验装配结果不变量：事实/模型分离，且缺口字段不得为空。
func (r Result) Validate() error {
	for i, fact := range r.Facts {
		if fact.GeneratedBy != GeneratedByEngine {
			return fmt.Errorf("agent: facts[%d] 非事实性结论 generated_by=%q", i, fact.GeneratedBy)
		}
		if err := fact.Validate(); err != nil {
			return fmt.Errorf("agent: facts[%d]: %w", i, err)
		}
	}
	if r.Verdict != nil {
		if r.Verdict.GeneratedBy != GeneratedByModel {
			return fmt.Errorf("agent: verdict 非模型生成段 generated_by=%q", r.Verdict.GeneratedBy)
		}
		if err := r.Verdict.Validate(); err != nil {
			return fmt.Errorf("agent: verdict: %w", err)
		}
	}
	if r.Gap != nil && r.Gap.Reason == "" {
		return errors.New("agent: gap.reason 不得为空（禁止用空缺口冒充成功）")
	}
	for i, gap := range r.Gaps {
		if gap.Source == "" || gap.Reason == "" {
			return fmt.Errorf("agent: gaps[%d] 需 source 与 reason 齐备", i)
		}
	}
	return nil
}
