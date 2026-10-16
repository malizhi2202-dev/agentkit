package tools

import (
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
)

// Schema 构造相关的失败。
var (
	// ErrSchemaUnsupported 表示给了非结构体，或结构体里出现不支持的字段类型。
	ErrSchemaUnsupported = errors.New("tools: 参数样例必须是结构体，且字段类型在支持范围内")
	// ErrSchemaEmpty 表示结构体没有任何可导出字段（一个"没有参数"的工具声明通常是漏写）。
	ErrSchemaEmpty = errors.New("tools: 参数结构体没有可导出字段")
)

// 支持的标量与数组元素类型；不在此列（map、嵌套结构体、interface、时间等）一律拒绝。
//
// 为什么宁可拒绝也不"尽力生成"：schema 与处理函数解出的结构体**必须同源**，
// 静默降级（比如把 map 写成 object 却不给 properties）会让模型按错误的形状填参数，
// 错误只会在运行期以"参数不合法"的形式冒出来，比注册期就报错难查得多。
// 需要复杂形状时显式手写 `Tool.Params`，本函数不冒充通用 JSON-Schema 生成器。
func scalarSchema(t reflect.Type) (map[string]any, bool) {
	switch t.Kind() {
	case reflect.String:
		return map[string]any{"type": "string"}, true
	case reflect.Bool:
		return map[string]any{"type": "boolean"}, true
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return map[string]any{"type": "integer"}, true
	case reflect.Float32, reflect.Float64:
		return map[string]any{"type": "number"}, true
	}
	return nil, false
}

// SchemaFor 从"处理函数实际解码的结构体"生成 JSON-Schema 对象，用作 `Tool.Params`。
//
// 这是 route↔tool 同源的支点：HTTP 侧解码的是这个结构体，模型侧看到的参数形状也由它生成，
// 两者之间没有第二份声明可以漂移。
//
// 取值口径：
//   - 字段名取 `json` tag 的名字（去掉 `,omitempty` 等选项）；`json:"-"` 与未导出字段跳过；
//   - 没有 `omitempty` 的字段进 `required`（模型必须给），有 `omitempty` 的为可选；
//   - `additionalProperties: false`：多给的键在解码层就被拒，schema 也不承认它；
//   - 指针字段按其指向类型生成；切片只支持标量元素（生成 `array` + `items`）；
//   - 匿名（内嵌）字段一律拒绝：提升字段的可见名与 JSON 名的对应关系不适合隐式推断。
//
// 不生成 `description`：文档字符串在这里没有可靠来源，编出来比留空更坏。
func SchemaFor(sample any) (map[string]any, error) {
	t := reflect.TypeOf(sample)
	for t != nil && t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	if t == nil || t.Kind() != reflect.Struct {
		return nil, fmt.Errorf("%w：实得 %T", ErrSchemaUnsupported, sample)
	}
	props := map[string]any{}
	var required []string
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		// 内嵌字段一律先拒（含未导出的匿名类型）：`encoding/json` 根本不提升未导出内嵌字段，
		// 静默跳过会让"我明明写了这个字段"变成运行期的参数缺失。宁可注册期说清楚。
		if f.Anonymous {
			return nil, fmt.Errorf("%w：字段 %s 是内嵌结构体（无论是否导出都请拆成显式具名字段）", ErrSchemaUnsupported, f.Name)
		}
		if !f.IsExported() {
			continue
		}
		name, opts, hasTag := jsonName(f)
		if name == "-" && hasTag {
			continue
		}
		ft := f.Type
		for ft.Kind() == reflect.Ptr {
			ft = ft.Elem()
		}
		sch, ok := scalarSchema(ft)
		if !ok {
			if ft.Kind() == reflect.Slice {
				et, eok := scalarSchema(ft.Elem())
				if !eok {
					return nil, fmt.Errorf("%w：字段 %s 是 []%s，元素类型不支持", ErrSchemaUnsupported, f.Name, ft.Elem())
				}
				sch = map[string]any{"type": "array", "items": et}
			} else {
				return nil, fmt.Errorf("%w：字段 %s 是 %s，只支持标量与标量切片", ErrSchemaUnsupported, f.Name, ft)
			}
		}
		props[name] = sch
		if !strings.Contains(opts, "omitempty") {
			required = append(required, name)
		}
	}
	if len(props) == 0 {
		return nil, fmt.Errorf("%w：%s", ErrSchemaEmpty, t.Name())
	}
	sort.Strings(required)
	out := map[string]any{
		"type":                 "object",
		"properties":           props,
		"additionalProperties": false,
	}
	if len(required) > 0 {
		out["required"] = required
	}
	return out, nil
}

// RequiredFromSchema 从生成出的 schema 里取必填键（同源比对用：路由侧与工具侧都从同一份 schema 出发）。
func RequiredFromSchema(schema map[string]any) []string {
	raw, ok := schema["required"].([]string)
	if !ok {
		return nil
	}
	out := append([]string(nil), raw...)
	sort.Strings(out)
	return out
}

func jsonName(f reflect.StructField) (name, opts string, hasTag bool) {
	tag, hasTag := f.Tag.Lookup("json")
	if !hasTag {
		return f.Name, "", false
	}
	parts := strings.Split(tag, ",")
	n := strings.TrimSpace(parts[0])
	if n == "" {
		n = f.Name
	}
	return n, strings.Join(parts[1:], ","), true
}
