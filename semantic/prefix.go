// Package semantic 是语义层的零依赖框架部分：把「前缀指令 + 内容」确定性解析为
// 结构化结果（指令 + 取值），不调模型、不访问网络、不依赖 宿主侧 任何包。
//
// 本文件只覆盖前缀指令解析。实体 / 意图 / 候选 / 不支持结构见 entity.go，
// route 判定（research / search / converse / clarify）由 宿主侧 侧 既有实现/semantic.go 承担。
package semantic

import (
	"strings"
	"unicode"
)

// 前缀指令取值（宿主契约  的 parsed.Cmd 枚举）。
const (
	CmdResearch = "research"
	CmdSearch   = "search"
)

// 前缀分隔符：半角与全角冒号等价，其余分隔符一律不识别。
const (
	halfColon = ":"
	fullColon = "："
)

// DefaultPrefixRules 是本包默认前缀词表：只用通用英文指令词，不含任何宿主的领域词。
// 前缀关键字属于调用方的领域语言（入口词由其产品契约冻结），而"关键字 → 指令"的解析机制、
// 半角/全角冒号等价、矛盾以显式指令为准这些规则是通用的 ⇒ 机制在包里、词表由调用方注入。
// 顺序即优先级，保证解析确定性。性。
// 只认入口命令/「搜索」两个关键字：避免把 URL / IPv6（`http://x`、`2001:db8::1`）
// 里的冒号误当成前缀分隔符。
var DefaultPrefixRules = []PrefixRule{
	{Keyword: "research", Cmd: CmdResearch},
	{Keyword: "search", Cmd: CmdSearch},
}

// PrefixRule 是一条「前缀关键字 → 指令」映射，由调用方注入。
type PrefixRule struct {
	Keyword string
	Cmd     string
}

// Parsed 是宿主契约  中冻结的 `parsed{cmd,value,conflict}` 落点。
//
// 字段名固定为 cmd / value（不是 command / content）；
// conflict 带 omitempty：无矛盾时该键不出现，以保证  的深等值断言成立。
type Parsed struct {
	Cmd      string `json:"cmd"`
	Value    string `json:"value"`
	Conflict string `json:"conflict,omitempty"`
}

// ParsePrefix 解析输入开头的可选前缀指令。
//
// 契约（，对齐 ）：
//   - 仅识别入口命令→ CmdResearch、「搜索」→ CmdSearch 两个前缀关键字；
//   - 半角冒号 ':' 与全角冒号 '：' 完全等价；
//   - 前缀与内容矛盾时以显式指令为准，Cmd 保持显式指令，并置非空 Conflict 提示，
//     不静默改判成 converse / clarify；
//   - 无前缀时返回 Cmd 为空、Value 为去空白后的原文，由调用方决定后续 route。
//
// 返回值 explicit 表示确实命中了前缀指令；未命中时调用方不应读取 Cmd。
// 本函数是纯函数：同一输入恒定返回同一结果，不读取任何外部状态。
func ParsePrefix(input string) (Parsed, bool) {
	return ParsePrefixWith(DefaultPrefixRules, input)
}

// ParsePrefixWith 用调用方注入的词表解析前缀（机制通用、词表归调用方）。
// 语义与 ParsePrefix 完全一致，仅关键字集合来自 rules；rules 为空 ⇒ 不识别任何前缀。
func ParsePrefixWith(rules []PrefixRule, input string) (Parsed, bool) {
	trimmed := strings.TrimSpace(input)
	for _, d := range rules {
		payload, ok := cutDirective(trimmed, d.Keyword)
		if !ok {
			continue
		}
		value := strings.TrimSpace(payload)
		parsed := Parsed{Cmd: d.Cmd, Value: value}
		if conflictsWithDirective(value) {
			parsed.Conflict = conflictNotice(d.Keyword)
		}
		return parsed, true
	}
	return Parsed{Cmd: "", Value: trimmed}, false
}

// cutDirective 在 s 以 `keyword + 冒号` 开头时，返回冒号之后的载荷。
func cutDirective(s, keyword string) (string, bool) {
	rest, found := strings.CutPrefix(s, keyword)
	if !found {
		return "", false
	}
	switch {
	case strings.HasPrefix(rest, halfColon):
		return rest[len(halfColon):], true
	case strings.HasPrefix(rest, fullColon):
		return rest[len(fullColon):], true
	}
	return "", false
}

// conflictsWithDirective 判断前缀载荷是否读起来像「自然语言诉求」而非指令取值。
//
// 判据是刻意保守且自足的（不依赖实体识别，故不引入  依赖）：
// 载荷含空白（成句）或含汉字（中文述求，如  的「帮我看下这个IP」）即视为矛盾。
// 命中只产生**提示**：Cmd 仍取显式指令，不改判、不阻断执行；误报代价仅为卡片多一行提示。
// 载荷为空不算矛盾——内容缺失由调用方走澄清（clarify），不是「与指令冲突」。
func conflictsWithDirective(payload string) bool {
	if payload == "" {
		return false
	}
	if strings.ContainsAny(payload, " \t\r\n") {
		return true
	}
	return hasHan(payload)
}

// hasHan 判断字符串是否含汉字（中文自然语言述求的信号）。
func hasHan(s string) bool {
	return strings.IndexFunc(s, func(r rune) bool { return unicode.Is(unicode.Han, r) }) >= 0
}

// conflictNotice 生成卡片上的「前缀与内容不一致」提示文案（ 要求非空）。
func conflictNotice(keyword string) string {
	return "前缀「" + keyword + "」与输入内容不一致，已按显式指令处理"
}
