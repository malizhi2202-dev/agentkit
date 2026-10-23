// Package security 承载 agentkit 的安全契约（脱敏证据 / 永不外发黑名单 / 不可信数据围栏 / 出域策略）。
//
// 本包与 agentkit 的其它子包一样**零 宿主依赖**（ / D1）：
// 只 import 标准库，上下文类型一律用 stdlib context.Context——宿主侧在
// 宿主接入层 内以 per-request 闭包捕获 宿主框架上下文类型 后注入（框架裁决）。
package security

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
)

// rootPath 是「整段 payload 被移除或改写」时使用的证据路径。
const rootPath = "$"

// Masker 是脱敏函数契约（ / 框架裁决 决策 2）。
//
// 三个出参缺一不可：
//   - masked：脱敏后的数据；
//   - applied：**本次实际被移除或改写的字段/规则**清单（证据），由输入与输出比对得出，
//     唯一被认可的来源是 AppliedEvidence；
//   - err：脱敏过程本身的错误。
//
// applied 的语义边界（ support 补，写死）：
//
//	✅ 只允许填「实际被移除或改写」的字段路径
//	❌ 禁止填「被查过的 sign」「被加载的规则数」「被遍历过的字段」等过程量
//
// 理由：调用方要靠 applied 区分「脱敏成功」与「空转」。若用过程量填充，
// 「data 非空而实际什么都没改」的 fail-open 态会带着非空 applied 蒙过兜底判据
// （框架裁决 判失败规则 ③④⑤⑥）。空转必须表现为空 applied。
//
// 签名只用 stdlib context.Context，不得出现 宿主框架上下文类型（ 推论）。
type Masker func(ctx context.Context, sign string, data any) (masked any, applied []string, err error)

// AppliedEvidence 由**输入与输出比对**得出 applied 证据：返回本次实际被移除或改写的字段路径清单。
//
// 口径（与既有 controllers.AttrFilter 同源）：
//   - 比对按 JSON 外形进行（既有实现同样以 tagName "json" 取值：宿主接入层 .go:142 →
//     GetPermissonData("json", ...)），因此 struct 用其 json tag 命名，`json:"-"` 字段不参与比对；
//   - 路径语法沿用既有过滤器树的层级写法（既有实现目录 attr_filter.go 的 tree）：
//     嵌套为 `a.b.c`，数组元素带具体下标 `domains[0].domain`，整段被移除时为 `$`；
//   - 只统计「被移除」（原输入有、输出没有）与「被改写」（叶子值或类型不同）的路径；
//     输出中新增的字段不算证据；
//   - 数字用 json.Number 解析，避免 float64 精度丢失把「超大整数被改写」误判为未变化（IMEI 等）；
//   - 结果按字典序排序且不重复；无任何变化时返回 nil。
//
// original 为 nil 时表示本次没有数据需要脱敏，返回空证据且不报错（既非脱敏成功也非空转）。
// 无法按 JSON 外形比对的值（如 chan/func）返回错误，而不是静默返回空证据——
// 后者会把「无法判定」伪装成「没有变化」。
func AppliedEvidence(original, masked any) ([]string, error) {
	if original == nil {
		return nil, nil
	}

	in, err := toJSONShape(original)
	if err != nil {
		return nil, fmt.Errorf("security: applied evidence: original: %w", err)
	}
	out, err := toJSONShape(masked)
	if err != nil {
		return nil, fmt.Errorf("security: applied evidence: masked: %w", err)
	}

	applied := make([]string, 0, 4)
	diffPath(rootPath, in, out, &applied)
	if len(applied) == 0 {
		return nil, nil
	}
	sort.Strings(applied)
	return applied, nil
}

// toJSONShape 把任意值按 JSON 外形规整成 map[string]any / []any / 标量，供确定性比对。
// 数字保留为 json.Number，避免 float64 精度丢失。
func toJSONShape(value any) (any, error) {
	if value == nil {
		return nil, nil
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var shaped any
	if err = decoder.Decode(&shaped); err != nil {
		return nil, err
	}
	return shaped, nil
}

// diffPath 递归比对同一路径上的输入与输出，把「被移除或改写」的字段路径追加进 applied。
func diffPath(path string, original, masked any, applied *[]string) {
	if originalMap, ok := original.(map[string]any); ok {
		maskedMap, ok := masked.(map[string]any)
		if !ok {
			*applied = append(*applied, path)
			return
		}
		for key, value := range originalMap {
			child := childPath(path, key)
			maskedValue, exists := maskedMap[key]
			if !exists {
				*applied = append(*applied, child)
				continue
			}
			diffPath(child, value, maskedValue, applied)
		}
		return
	}

	if originalArr, ok := original.([]any); ok {
		maskedArr, ok := masked.([]any)
		if !ok {
			*applied = append(*applied, path)
			return
		}
		shared := len(originalArr)
		if len(maskedArr) < shared {
			shared = len(maskedArr)
		}
		for i := 0; i < shared; i++ {
			diffPath(indexPath(path, i), originalArr[i], maskedArr[i], applied)
		}
		for i := shared; i < len(originalArr); i++ {
			*applied = append(*applied, indexPath(path, i))
		}
		return
	}

	if !reflect.DeepEqual(original, masked) {
		*applied = append(*applied, path)
	}
}

func childPath(path, key string) string {
	if path == rootPath {
		return key
	}
	return path + "." + key
}

func indexPath(path string, index int) string {
	if path == rootPath {
		return fmt.Sprintf("[%d]", index)
	}
	return fmt.Sprintf("%s[%d]", path, index)
}
