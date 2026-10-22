package security

import (
	"context"
	"reflect"
	"testing"
)

// stdlibContextType 是 Masker 契约里唯一允许出现的上下文类型（ 推论 / 框架裁决）：
// 宿主侧在 宿主接入层 内以 per-request 闭包捕获 宿主框架上下文类型 后注入。
var stdlibContextType = reflect.TypeOf((*context.Context)(nil)).Elem()

var anyType = reflect.TypeOf((*any)(nil)).Elem()

func TestMasker_SignatureIsFrozen(t *testing.T) {
	var m Masker = func(ctx context.Context, sign string, data any) (masked any, applied []string, err error) {
		return data, nil, nil
	}
	if m == nil {
		t.Fatal("Masker 必须可被赋值/调用")
	}

	mt := reflect.TypeOf(m)
	if mt.NumIn() != 3 || mt.NumOut() != 3 {
		t.Fatalf("Masker 必须 3 入 3 出，实际 in=%d out=%d", mt.NumIn(), mt.NumOut())
	}
	if got := mt.In(0); got != stdlibContextType {
		t.Fatalf("第 1 入参必须是 stdlib context.Context（不得是 宿主框架上下文类型），实际 = %v", got)
	}
	if got := mt.In(1); got.Kind() != reflect.String {
		t.Fatalf("第 2 入参必须是 string sign，实际 = %v", got)
	}
	if got := mt.In(2); got != anyType {
		t.Fatalf("第 3 入参必须是 any data，实际 = %v", got)
	}
	if got := mt.Out(0); got != anyType {
		t.Fatalf("第 1 出参必须是 masked any，实际 = %v", got)
	}
	if got := mt.Out(1); got != reflect.TypeOf([]string(nil)) {
		t.Fatalf("第 2 出参必须是 applied []string（证据，而非只返回 error），实际 = %v", got)
	}
	if got := mt.Out(2); got != reflect.TypeOf((*error)(nil)).Elem() {
		t.Fatalf("第 3 出参必须是 err error，实际 = %v", got)
	}
}

// 空转（sign 被查过、规则被加载，但输入输出完全一致）⇒ applied 必须为空。
// 禁止用「被查过的 sign」「被加载的规则数」填充证据（ support 补）。
func TestAppliedEvidence_NoOpMaskYieldsNoEvidence(t *testing.T) {
	original := map[string]any{"email": "a@b.com", "role": "v1"}

	got, err := AppliedEvidence(original, map[string]any{"email": "a@b.com", "role": "v1"})
	if err != nil {
		t.Fatalf("AppliedEvidence 不应报错：%v", err)
	}
	if len(got) != 0 {
		t.Fatalf("输入输出一致时必须无证据，实际 = %v", got)
	}
}

func TestAppliedEvidence_RemovedAndRewritten(t *testing.T) {
	cases := []struct {
		name     string
		original any
		masked   any
		want     []string
	}{
		{
			name:     "字段被移除",
			original: map[string]any{"email": "a@b.com", "role": "v1"},
			masked:   map[string]any{"role": "v1"},
			want:     []string{"email"},
		},
		{
			name:     "字段被改写",
			original: map[string]any{"email": "a@b.com"},
			masked:   map[string]any{"email": "a***"},
			want:     []string{"email"},
		},
		{
			name:     "嵌套字段路径",
			original: map[string]any{"user": map[string]any{"email": "a@b.com", "role": "v1"}},
			masked:   map[string]any{"user": map[string]any{"role": "v1"}},
			want:     []string{"user.email"},
		},
		{
			name:     "数组元素被移除",
			original: map[string]any{"domains": []any{"a.com", "b.com", "c.com"}},
			masked:   map[string]any{"domains": []any{"a.com"}},
			want:     []string{"domains[1]", "domains[2]"},
		},
		{
			name:     "数组元素内字段被改写",
			original: map[string]any{"domains": []any{map[string]any{"domain": "a.com"}}},
			masked:   map[string]any{"domains": []any{map[string]any{"domain": "a***"}}},
			want:     []string{"domains[0].domain"},
		},
		{
			name:     "整段被移除",
			original: map[string]any{"user": map[string]any{"email": "a@b.com"}},
			masked:   nil,
			want:     []string{"$"},
		},
		{
			name:     "类型被改写（对象 → 标量）",
			original: map[string]any{"user": map[string]any{"email": "a@b.com"}},
			masked:   map[string]any{"user": "***"},
			want:     []string{"user"},
		},
		{
			name:     "输出新增字段不算「被移除或改写」",
			original: map[string]any{"role": "v1"},
			masked:   map[string]any{"role": "v1", "extra": "x"},
			want:     nil,
		},
		{
			name:     "nil 输入无字段可移除",
			original: nil,
			masked:   nil,
			want:     nil,
		},
		{
			name:     "超大整数被改写不得因精度丢失漏报",
			original: map[string]any{"imei": int64(861234567890123)},
			masked:   map[string]any{"imei": int64(861234567890124)},
			want:     []string{"imei"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := AppliedEvidence(tc.original, tc.masked)
			if err != nil {
				t.Fatalf("AppliedEvidence 报错：%v", err)
			}
			if len(tc.want) == 0 {
				if len(got) != 0 {
					t.Fatalf("期望无证据，实际 = %v", got)
				}
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("证据不符：want=%v got=%v", tc.want, got)
			}
		})
	}
}

// 证据必须确定性有序（审计与跨进程比对依赖同一顺序），且不重复。
func TestAppliedEvidence_OrderedAndDeduped(t *testing.T) {
	original := map[string]any{"z": 1, "a": 2, "m": 3, "nested": map[string]any{"b": 4}}
	masked := map[string]any{"nested": map[string]any{}}

	got, err := AppliedEvidence(original, masked)
	if err != nil {
		t.Fatalf("AppliedEvidence 报错：%v", err)
	}
	want := []string{"a", "m", "nested.b", "z"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("证据必须有序且去重：want=%v got=%v", want, got)
	}
}

// 与既有 AttrFilter 同口径：标签名是 json（controllers.AttrFilter → GetPermissonData("json", ...)），
// 因此结构体按其 JSON 外形参与比对，`json:"-"` 字段不参与。
func TestAppliedEvidence_UsesJSONShape(t *testing.T) {
	type profile struct {
		Email  string `json:"email"`
		Secret string `json:"-"`
	}

	got, err := AppliedEvidence(profile{Email: "a@b.com", Secret: "raw"}, map[string]any{"email": "a***"})
	if err != nil {
		t.Fatalf("AppliedEvidence 报错：%v", err)
	}
	if want := []string{"email"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("应按 JSON 外形比对：want=%v got=%v", want, got)
	}
}

// 无法按 JSON 外形比对的数据（出域 payload 不可能是这类值）必须显式报错，
// 而不是静默返回「空证据」把空转伪装成脱敏成功。
func TestAppliedEvidence_UnsupportedValueReturnsError(t *testing.T) {
	if _, err := AppliedEvidence(map[string]any{"ch": make(chan int)}, nil); err == nil {
		t.Fatal("不可比对的值必须返回错误，而不是空证据")
	}
}
