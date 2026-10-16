package security

import "reflect"

// IsEmptyPayload 是「载荷没有任何需要脱敏的内容」的**唯一判定点**（ ⑤ 的 `rv.IsZero` 口径）。
//
// 收敛纪律（R3 知识重复 / T-FIX-09）：本判定原先在 `既有实现/capability.isEmptyData` 与
// `既有实现/masker.isEmptyMaskData` 里**逐字各写一份**（作者当时以「跨包测试/内部函数不可见」
// 为由接受重复，并在 `masker.go` 留下「将来上移到共享包」的债）。现上移为**唯一**实现：
// 两处调用方一律转调本函数，**不得**再写第二份、也不得保留各自的旧副本。
//
// 语义（**冻结，不得放宽**）：
//   - `nil` 接口 / `nil` 的指针·映射·切片·通道·函数 ⇒ 空；
//   - 其余按 `reflect.Value.IsZero`：零值结构体、空串、数值 0、`false` ⇒ 空；
//   - **非 nil 的空映射 / 空切片不算空**（`rv.IsZero` 为假）⇒ 仍要求 Masker 给出证据。
//
// ⚠️ 边界差异（**刻意保留，不是重复实现**）：出域边界的 `既有模型层.outletEmptyData` 判的是
// 「JSON 段载荷里有没有内容」，它对**非 nil 空容器**与**纯空白串**判为空（`len==0` / `TrimSpace`），
// 与本函数的 `rv.IsZero` 口径在这几类输入上**结论相反**。两者**不可合并**：任一方改成另一方的
// 语义都会改变生产可观察行为 —— 合并到本函数语义会让出域侧开始拒绝合法空容器（空结果出不去），
// 合并到 `len==0` 语义则会让本侧放行「非 nil 空容器」且让出域侧放行 `data:false`。
// 差异由 `既有模型层/empty_payload_boundary_test.go` 的**分歧见证用例**逐类固化（可失败），
// 并由 `既有模型层/guard_test.go:582` 的既有钉子保护出域侧语义。
//
// 故本函数只服务  ⑤ 的「data 非空而 applied 为空 ⇒ 拒」判据（capability 结果边界 + 响应边界）；
// 出域 JSON 边界继续使用它自己的谓词。
func IsEmptyPayload(value any) bool {
	if value == nil {
		return true
	}
	rv := reflect.ValueOf(value)
	switch rv.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Ptr, reflect.Slice:
		if rv.IsNil() {
			return true
		}
	}
	return rv.IsZero()
}
