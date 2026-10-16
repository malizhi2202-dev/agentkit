package agent

import (
	"encoding/json"
	"testing"
	"time"
)

//  · RED/GREEN 验收测试：
//   -   模型段单独标记 generated_by=="model"，与事实段数据结构区分
//   -   每条事实性结论携带 {capability, source, field, fetched_at}
//   -   结论项七字段 {value, capability, source, field, fetched_at, generated_by, confidence}
//   -       缺口单复并存且语义分工：gap{reason} 与 gaps[].{source,reason}

func fetchedAt() time.Time {
	return time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
}

func sampleRef() SourceRef {
	return SourceRef{
		Capability: "ip_detail",
		Source:     "GET /ip/detail",
		Field:      "risk_level",
		FetchedAt:  fetchedAt(),
	}
}

func mustJSONMap(t *testing.T, v any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	return m
}

var provenanceKeys = []string{"capability", "source", "field", "fetched_at"}

const sevenFields = 7

func TestResult_FactCarriesProvenanceQuadruple(t *testing.T) {
	item := NewFactConclusion("高危", sampleRef(), 0.92)

	if item.GeneratedBy != GeneratedByEngine {
		t.Fatalf("事实性结论 generated_by 期望 %q，得到 %q", GeneratedByEngine, item.GeneratedBy)
	}
	if item.SourceRef == nil {
		t.Fatal("事实性结论必须携带溯源四元组（契约点）")
	}
	if item.Capability != "ip_detail" || item.Source != "GET /ip/detail" || item.Field != "risk_level" {
		t.Fatalf("溯源四元组不齐备: %#v", *item.SourceRef)
	}
	if !item.FetchedAt.Equal(fetchedAt()) {
		t.Fatalf("fetched_at 期望 %v，得到 %v", fetchedAt(), item.FetchedAt)
	}
	if err := item.Validate(); err != nil {
		t.Fatalf("事实性结论应通过校验: %v", err)
	}

	m := mustJSONMap(t, item)
	for _, k := range append([]string{"value", "generated_by", "confidence"}, provenanceKeys...) {
		if _, ok := m[k]; !ok {
			t.Errorf("结论项七字段缺少 %q: %v", k, m)
		}
	}
	if len(m) != sevenFields {
		t.Errorf("结论项应恰为七字段，实际 %d 个: %v", len(m), m)
	}
}

func TestResult_ModelSectionTagged(t *testing.T) {
	var r Result
	r.AddFact("高危", sampleRef(), 0.9)
	r.SetVerdict("宿主入口词为恶意扫描源", 0.7)

	if r.Verdict == nil {
		t.Fatal("SetVerdict 之后 verdict 段不应为空")
	}
	if r.Verdict.GeneratedBy != GeneratedByModel {
		t.Fatalf("verdict.generated_by 期望 %q，得到 %q", GeneratedByModel, r.Verdict.GeneratedBy)
	}
	if r.Verdict.SourceRef != nil {
		t.Fatal("模型生成段不得携带溯源四元组")
	}

	m := mustJSONMap(t, r.Verdict)
	if m["generated_by"] != GeneratedByModel {
		t.Fatalf("$.data.verdict.generated_by 期望 %q，得到 %v", GeneratedByModel, m["generated_by"])
	}
	for _, k := range provenanceKeys {
		if _, ok := m[k]; ok {
			t.Errorf("模型段不得出现 %q（与事实段数据结构区分，契约点）: %v", k, m)
		}
	}
}

func TestResult_FactVsModelSeparated(t *testing.T) {
	var r Result
	r.AddFact("高危", sampleRef(), 0.9)
	r.AddFact("近 30 天有外联", SourceRef{
		Capability: "ip_timeline",
		Source:     "GET /ip/timeline",
		Field:      "events",
		FetchedAt:  fetchedAt(),
	}, 0.8)
	r.SetVerdict("宿主入口词为恶意扫描源", 0.7)

	if err := r.Validate(); err != nil {
		t.Fatalf("装配结果应通过校验: %v", err)
	}

	m := mustJSONMap(t, r)
	facts, ok := m["facts"].([]any)
	if !ok || len(facts) != 2 {
		t.Fatalf("facts 期望 2 条，得到 %v", m["facts"])
	}
	for i, f := range facts {
		fm := f.(map[string]any)
		if fm["generated_by"] != GeneratedByEngine {
			t.Errorf("facts[%d].generated_by 期望 %q，得到 %v", i, GeneratedByEngine, fm["generated_by"])
		}
		for _, k := range provenanceKeys {
			if _, ok := fm[k]; !ok {
				t.Errorf("facts[%d] 缺少溯源字段 %q: %v", i, k, fm)
			}
		}
	}

	verdict, ok := m["verdict"].(map[string]any)
	if !ok {
		t.Fatalf("verdict 段缺失: %v", m)
	}
	if verdict["generated_by"] != GeneratedByModel {
		t.Errorf("verdict.generated_by 期望 %q，得到 %v", GeneratedByModel, verdict["generated_by"])
	}
	for _, k := range provenanceKeys {
		if _, ok := verdict[k]; ok {
			t.Errorf("verdict 不得出现 %q（两类不可混淆，契约点）: %v", k, verdict)
		}
	}
}

// ：模型不可用走降级 ⇒ 不给出模型段（$.data 无 model 段）。
func TestResult_DegradedOmitsModelSection(t *testing.T) {
	var r Result
	r.AddFact("高危", sampleRef(), 0.9)

	m := mustJSONMap(t, r)
	if _, ok := m["verdict"]; ok {
		t.Errorf("未生成宿主入口词结论时不应出现 verdict 段: %v", m)
	}
}

func TestResult_ValidateRejectsUntraceableFact(t *testing.T) {
	bare := ConclusionItem{Value: "高危", GeneratedBy: GeneratedByEngine, Confidence: 0.9}
	if err := bare.Validate(); err == nil {
		t.Fatal("事实性结论缺溯源四元组应校验失败（禁止无溯源的事实结论）")
	}

	incomplete := NewFactConclusion("高危", SourceRef{Capability: "ip_detail", Source: "GET /ip/detail"}, 0.9)
	if err := incomplete.Validate(); err == nil {
		t.Fatal("溯源四元组不完整（缺 field / fetched_at）应校验失败")
	}

	var r Result
	r.Facts = append(r.Facts, bare)
	if err := r.Validate(); err == nil {
		t.Fatal("Result.Validate 应传导事实结论的溯源校验失败")
	}
}

func TestResult_ValidateRejectsFabricatedModelProvenance(t *testing.T) {
	ref := sampleRef()
	bad := ConclusionItem{Value: "宿主入口词为恶意", SourceRef: &ref, GeneratedBy: GeneratedByModel, Confidence: 0.7}
	if err := bad.Validate(); err == nil {
		t.Fatal("模型生成段携带溯源四元组应校验失败（不得伪造溯源）")
	}
}

func TestResult_ValidateRejectsUnknownGeneratedBy(t *testing.T) {
	bad := ConclusionItem{Value: "x", GeneratedBy: "llm-hallucination"}
	if err := bad.Validate(); err == nil {
		t.Fatalf("未知 generated_by=%q 应校验失败", bad.GeneratedBy)
	}
}

func TestGap_SingleAndMultipleCoexist(t *testing.T) {
	var r Result
	r.AddFact("高危", sampleRef(), 0.9)
	r.SetGap("模型不可用，未生成宿主入口词结论")
	r.AddSourceGap("homology_url", "未返回")
	r.AddSourceGap("related_mid", "取数超时")

	if err := r.Validate(); err != nil {
		t.Fatalf("缺口单复并存应通过校验: %v", err)
	}

	m := mustJSONMap(t, r)

	gap, ok := m["gap"].(map[string]any)
	if !ok {
		t.Fatalf("单数缺口段缺失: %v", m)
	}
	if gap["reason"] != "模型不可用，未生成宿主入口词结论" {
		t.Errorf("gap.reason 不符: %v", gap)
	}
	if _, ok := gap["source"]; ok {
		t.Errorf("单数 gap 只承载顶层原因，不应含 source: %v", gap)
	}

	gaps, ok := m["gaps"].([]any)
	if !ok || len(gaps) != 2 {
		t.Fatalf("复数缺口期望 2 条，得到 %v", m["gaps"])
	}
	first := gaps[0].(map[string]any)
	if first["source"] != "homology_url" || first["reason"] != "未返回" {
		t.Errorf("gaps[0] 期望 {homology_url, 未返回}，得到 %v", first)
	}
}

func TestGap_NoSubstitution(t *testing.T) {
	var single Result
	single.SetGap("单段缺口")
	if len(single.Gaps) != 0 {
		t.Errorf("单数缺口不得填充 gaps[]: %v", single.Gaps)
	}
	if err := single.Validate(); err != nil {
		t.Fatalf("单数缺口应通过校验: %v", err)
	}

	var multi Result
	multi.AddSourceGap("homology_url", "未返回")
	if multi.Gap != nil {
		t.Errorf("逐来源缺口不得填充单数 gap: %v", multi.Gap)
	}
	if err := multi.Validate(); err != nil {
		t.Fatalf("复数缺口应通过校验: %v", err)
	}
}

func TestGap_ValidateRejectsEmptyReason(t *testing.T) {
	var single Result
	single.SetGap("")
	if err := single.Validate(); err == nil {
		t.Fatal("gap.reason 为空应校验失败（禁止用空缺口冒充成功）")
	}

	badSource := Result{Gaps: []GapEntry{{Source: "", Reason: "未返回"}}}
	if err := badSource.Validate(); err == nil {
		t.Fatal("gaps[].source 为空应校验失败")
	}

	badReason := Result{Gaps: []GapEntry{{Source: "homology_url", Reason: ""}}}
	if err := badReason.Validate(); err == nil {
		t.Fatal("gaps[].reason 为空应校验失败")
	}
}
