package semantic

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

// asJSONMap 把结构序列化再反序列化为 map，用于按**线上 JSON 键**断言契约
// 。
func asJSONMap(t *testing.T, v any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("json.Unmarshal(%s): %v", raw, err)
	}
	return out
}

// ：多意图解析结构 —— 实体 + 诉求（多意图并存）齐备且可执行。
func TestUnderstanding_MultiIntentParse(t *testing.T) {
	u := Understanding{
		Entities: []Entity{{Type: "ip", Value: "1.2.3.4"}},
		Intents:  []Intent{IntentCorrelationAnalysis, IntentPreliminaryReport},
	}

	// `$.data.entities[0]=={type:"ip",value:"1.2.3.4"}`（深等值，键名固定 type/value）
	got := asJSONMap(t, u)
	entities, ok := got["entities"].([]any)
	if !ok || len(entities) == 0 {
		t.Fatalf("entities = %#v，期望非空数组", got["entities"])
	}
	wantEntity := map[string]any{"type": "ip", "value": "1.2.3.4"}
	if !reflect.DeepEqual(entities[0], wantEntity) {
		t.Fatalf("entities[0] = %#v，期望 %#v", entities[0], wantEntity)
	}

	// `$.data.intents ⊇ {correlation_analysis, preliminary_report}`（多意图并存）
	intents, ok := got["intents"].([]any)
	if !ok || len(intents) != 2 {
		t.Fatalf("intents = %#v，期望恰 2 个并存意图", got["intents"])
	}
	for _, want := range []Intent{IntentCorrelationAnalysis, IntentPreliminaryReport} {
		if !u.HasIntent(want) {
			t.Fatalf("intents 缺少 %q：%#v", want, got["intents"])
		}
	}

	// 解析结果进入工具调用：无候选、无 unsupported ⇒ 可执行
	if !u.Executable() {
		t.Fatal("多意图实体解析结果应允许进入编排")
	}
	if got["unsupported"] != false {
		t.Fatalf("unsupported = %#v，期望 false", got["unsupported"])
	}
}

// ：多实体 / 歧义 ⇒ 候选列表待确认，**不擅自选一个执行**。
func TestUnderstanding_AmbiguousCandidatesNotExecuted(t *testing.T) {
	u := Understanding{
		Candidates: []Candidate{
			{Type: "ip", Value: "1.1.1.1"},
			{Type: "ip", Value: "2.2.2.2"},
		},
	}

	// `$.data.candidates.length>1`
	got := asJSONMap(t, u)
	candidates, ok := got["candidates"].([]any)
	if !ok || len(candidates) <= 1 {
		t.Fatalf("candidates = %#v，期望 length>1", got["candidates"])
	}
	wantFirst := map[string]any{"type": "ip", "value": "1.1.1.1"}
	if !reflect.DeepEqual(candidates[0], wantFirst) {
		t.Fatalf("candidates[0] = %#v，期望 %#v", candidates[0], wantFirst)
	}

	// 不擅自选一个执行：候选不得同时出现在 entities 里（executed_steps 恒为 0）
	if len(u.Entities) != 0 {
		t.Fatalf("候选被当成实体执行：entities = %#v", u.Entities)
	}
	if u.Executable() {
		t.Fatal("存在待确认候选时不得进入编排（executed_steps 必须为 0）")
	}
}

// ：诉求不被能力范围覆盖 ⇒ unsupported + 统一文案 + 能力范围列表，**不猜测执行**。
func TestUnderstanding_UnsupportedNoGuess(t *testing.T) {
	scope := []Mechanism{
		{Name: "correlation", Label: "关联分析"},
		{Name: "report", Label: "初步报告"},
	}
	u := Understanding{Unsupported: UnsupportedFor(scope)}

	got := asJSONMap(t, u)
	if got["unsupported"] != true {
		t.Fatalf("unsupported = %#v，期望 true", got["unsupported"])
	}
	// 文案逐字冻结
	if got["notice"] != UnsupportedNotice {
		t.Fatalf("notice = %#v，期望 %q", got["notice"], UnsupportedNotice)
	}
	if UnsupportedNotice != "暂不支持该诉求" {
		t.Fatalf("UnsupportedNotice = %q，与 契约点 冻结文案不一致", UnsupportedNotice)
	}
	// 当前能力范围列表非空，且每项可映射出 name/label
	mechanisms, ok := got["mechanisms"].([]any)
	if !ok || len(mechanisms) == 0 {
		t.Fatalf("mechanisms = %#v，期望非空能力范围列表", got["mechanisms"])
	}
	if !reflect.DeepEqual(mechanisms[0], map[string]any{"name": "correlation", "label": "关联分析"}) {
		t.Fatalf("mechanisms[0] = %#v", mechanisms[0])
	}
	if !u.IsUnsupported() {
		t.Fatal("IsUnsupported() 应为 true")
	}

	// 不猜测执行
	if u.Executable() {
		t.Fatal("诉求不支持时不得进入编排（executed_steps 必须为 0）")
	}
}

// 无候选且未被判不支持时（如对话 / 澄清路径）允许进入编排。
func TestUnderstanding_EmptyIsExecutable(t *testing.T) {
	if !(Understanding{}).Executable() {
		t.Fatal("空理解结果（对话路径）应允许进入编排")
	}
	if (Understanding{}).IsUnsupported() {
		t.Fatal("零值不得被判为 unsupported")
	}
	if (Understanding{}).HasIntent(IntentCorrelationAnalysis) {
		t.Fatal("无诉求时 HasIntent 必须为 false")
	}
}

// fakeParser 是注入 EntityParser 的测试替身：按白名单给出类型，其余返回空类型。
type fakeParser struct {
	types map[string]string
}

func (p fakeParser) EntityType(_ context.Context, value string) (string, error) {
	return p.types[value], nil
}

// compile-time：EntityParser 必须能用**纯 stdlib** 实现（零 宿主依赖红线 / 框架裁决）。
var _ EntityParser = fakeParser{}

type failingParser struct{}

func (failingParser) EntityType(context.Context, string) (string, error) {
	return "", errors.New("下游不可达")
}

func TestParseEntities_InjectedParser(t *testing.T) {
	parser := fakeParser{types: map[string]string{"1.2.3.4": "ip", "evil.com": "domain"}}

	got, err := ParseEntities(context.Background(), parser, []string{"1.2.3.4", "asdf@@@", "evil.com"})
	if err != nil {
		t.Fatalf("ParseEntities: %v", err)
	}
	want := []Entity{{Type: "ip", Value: "1.2.3.4"}, {Type: "domain", Value: "evil.com"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("entities = %#v，期望 %#v（识别不出的取值被跳过、顺序保持）", got, want)
	}
	for _, e := range got {
		if e.Type == "" || e.Value == "" {
			t.Fatalf("产出伪实体：%#v", e)
		}
	}
}

func TestParseEntities_ParserErrorPropagates(t *testing.T) {
	if _, err := ParseEntities(context.Background(), failingParser{}, []string{"1.2.3.4"}); err == nil {
		t.Fatal("识别过程真实失败必须上抛，不得静默当作「不是实体」")
	}
}

func TestParseEntities_NilParserFailsClosed(t *testing.T) {
	if _, err := ParseEntities(context.Background(), nil, []string{"1.2.3.4"}); !errors.Is(err, ErrNoEntityParser) {
		t.Fatalf("未注入 EntityParser 应 fail-closed 返回 ErrNoEntityParser，实际 %v", err)
	}
}
