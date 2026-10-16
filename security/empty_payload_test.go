package security

import "testing"

// tfix09ZeroStruct 是一个「全零值」结构体： ⑤ 口径下它算「无内容」。
type tfix09ZeroStruct struct {
	A string `json:"a"`
	B int    `json:"b"`
}

// tfix09NonZeroStruct 是带任一非零字段的结构体：算「有内容」。
type tfix09NonZeroStruct struct {
	A string `json:"a"`
}

// TestIsEmptyPayload_SingleTruth 固化  ⑤「什么算空」的**唯一判定点**语义（T-FIX-09 收敛点）。
//
// 正例（空 ⇒ 无需 evidence）与负例（非空 ⇒ 必须要求 evidence）必须严格区分：
// 负例里最关键的两条是「非 nil 空 map」「非 nil 空 slice」—— 它们**不是**空载荷，
// 若把它们判成空，就会把「data 非空而 applied 为空」的 fail-open 态放行（ ⑤ 被绕过）。
func TestIsEmptyPayload_SingleTruth(t *testing.T) {
	var nilPtr *int
	var nilMap map[string]any
	var nilSlice []any
	var nilChan chan int
	var nilFunc func()

	emptyCases := []struct {
		name  string
		value any
	}{
		{"nil 接口", nil},
		{"nil 指针", nilPtr},
		{"nil 映射", nilMap},
		{"nil 切片", nilSlice},
		{"nil 通道", nilChan},
		{"nil 函数", nilFunc},
		{"零值结构体", tfix09ZeroStruct{}},
		{"空字符串", ""},
		{"数值零", 0},
		{"布尔假", false},
	}
	for _, testCase := range emptyCases {
		t.Run("空/"+testCase.name, func(t *testing.T) {
			if !IsEmptyPayload(testCase.value) {
				t.Fatalf("IsEmptyPayload(%s) = false，want true（节 ⑤：零值/无内容 ⇒ 无内容可泄露）", testCase.name)
			}
		})
	}

	nonEmptyCases := []struct {
		name  string
		value any
	}{
		{"非 nil 空 map（关键负例）", map[string]any{}},
		{"非 nil 空 slice（关键负例）", []any{}},
		{"非 nil 空 map[string]string", map[string]string{}},
		{"纯空白串（关键负例）", "  "},
		{"有内容的 map", map[string]any{"value": "1.2.3.4"}},
		{"有内容的切片", []any{"a"}},
		{"非空字符串", "1.2.3.4"},
		{"非零数值", 1},
		{"布尔真", true},
		{"非零值结构体", tfix09NonZeroStruct{A: "x"}},
		{"指向零值的非 nil 指针", func() any { value := 0; return &value }()},
	}
	for _, testCase := range nonEmptyCases {
		t.Run("非空/"+testCase.name, func(t *testing.T) {
			if IsEmptyPayload(testCase.value) {
				t.Fatalf("IsEmptyPayload(%s) = true，want false："+
					"非 nil 空容器 / 纯空白串 / 有内容的值都不是空载荷，"+
					"判成空会放行「data 非空而 applied 为空」（节 ⑤ 绕过）", testCase.name)
			}
		})
	}
}

// TestIsEmptyPayload_ZeroValueIsNotContainerEmptiness 单独钉住「零值 ≠ 容器空」这条最容易写错的分界：
// 非 nil 空容器（IsZero 为假）必须与 nil 容器（IsZero 为真）给出**相反**结论。
func TestIsEmptyPayload_ZeroValueIsNotContainerEmptiness(t *testing.T) {
	if !IsEmptyPayload([]byte(nil)) {
		t.Fatal("nil 切片必须判为空")
	}
	if IsEmptyPayload([]byte{}) {
		t.Fatal("非 nil 空切片 []byte{} 不得判为空：nil 与非 nil 空切片在本口径下结论必须相反")
	}
}
