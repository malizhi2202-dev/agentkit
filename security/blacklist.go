package security

import (
	"fmt"
	"strings"
	"unicode"
)

// ============================================================================
// 永不外发黑名单
//
// 语义：命中即**整体移除该 key**，不做哈希 / 截断 / 变形 —— 变形形式同样是泄露面
// （ADR-001 决策 5：「哈希或截断形式同样移除（不是替代方案）」）。
//
// 「不可关闭」是**代码结构上的事实**，不是文档承诺：
//
//	① 内建规则表是本文件的包级**未导出**变量（外部包不可见、无可写句柄），随二进制发版；
//	   进程内不存在「读配置 → 关掉黑名单」的路径；
//	② 唯一出口 ApplyBlacklist 的签名里**没有任何开关/选项参数**（无 bool、无指针、
//	   无 options/config 形参），调用方无法用参数或配置让内建集合失效；
//	③ 唯一能表达「增项」的位置是 ApplyBlacklist 的 extraFields 变参，**只追加、永不替换**：
//	   传 nil / 空切片 / 空串 / 无关名都不会减少内建集合，也无法把它置空关闭；
//	④ 本包不导出任何 Set / Replace / Remove / Clear / Reset / Disable / Enable / Optional
//	   形式的 API；以上四条由 TestGuard_BlacklistNotOptional 以「导出面扫描 + AST 扫描 +
//	   行为单调性」三重固化。
//
// 与既有 `controllers.AttrFilter` 的关系（ / ADR-008 决策 8）：AttrFilter 是
// **值级脱敏**且 fail-open、规则来自 DB；本黑名单是**键级整体移除**且不可关闭，只在其**之上**
// 叠加，不复制也不替换既有规则。
//
// 口径边界（登记，见 -SUMMARY）：本文件只做「字段名命中即整体移除」的键级判定；
// 「值级」的内部 host 字面量（如 `*.internal.example`）判定属  的端点白名单
// （禁公网 + 私网段运行期二次校验）与最小化白名单层，不在这里做值内容嗅探。
// ============================================================================

// blacklistWords 是「永不外发」的**词级**命中集合：字段名规范化后按 `_` 分词，
// 任一**整词**命中即整体移除该 key。
//
// 用整词而非子串，是为了避免误伤复数与统计量（`max_tokens` / `total_tokens` / `tokens`
// 都不是 `token`），从而让黑名单既不可关闭、又不过度牺牲可入口命令性。
var blacklistWords = []string{
	// A. IMEI 原始值（设备标识原文，含 imei_clientid / imei_hash 等派生键名）
	"imei",
	"clientid",
	// B. 内部 token / 凭据
	"token",
	"secret",
	"password",
	"passwd",
	"credential",
	"credentials",
	"authorization",
	"cookie",
	"bearer",
	"apikey",
	"accesskey",
	"privatekey",
	"secretkey",
	"sessionkey",
}

// blacklistFragments 对**紧凑形态**（去分隔符、全小写）做包含匹配，
// 覆盖被分隔符拆开或连写的写法（`api_key` / `APIKey` → `apikey`、`client_secret` → `clientsecret`）。
// 刻意**不含** `token`：紧凑包含 `token` 会误伤 `tokens` / `maxtokens`。
var blacklistFragments = []string{
	"secret",
	"password",
	"passwd",
	"credential",
	"authorization",
	"bearer",
	"cookie",
	"apikey",
	"accesskey",
	"privatekey",
	"secretkey",
	"sessionkey",
}

// blacklistPrefixes 对紧凑形态做**前缀**匹配（IMEI / clientid 族），
// 覆盖 `IMEIClientID` 这类缩写连写；刻意不做包含匹配，否则 `time_index` 会因含 `imei` 子串被误伤。
var blacklistPrefixes = []string{"imei", "clientid"}

// blacklistCompactSuffixes 对紧凑形态做**后缀**匹配（`accesstoken` / `idtoken`），
// 同样不做包含匹配，以免误伤 `tokens` / `maxtokens`。
var blacklistCompactSuffixes = []string{"token"}

// blacklistHostScopeWords × blacklistHostTargetWords：字段名同时含「内部范围词」与
// 「主机目标词」时视为内部 host（`internal_host` / `inner_domain` / `private_ip` / `intranet_url`）。
var (
	blacklistHostScopeWords  = []string{"internal", "intranet", "inner", "private", "corp"}
	blacklistHostTargetWords = []string{"host", "hostname", "domain", "url", "uri", "ip", "addr", "address", "endpoint", "dns", "gateway", "proxy", "node", "server"}
)

// ApplyBlacklist 应用「永不外发」黑名单：命中字段**整体移除**（不落哈希/截断/掩码）。
//
// 三个出参：
//   - masked：移除后的 JSON 外形数据（新对象，**不修改入参**）；
//   - removed：本次**实际被移除**的字段路径清单（供 `LOG:egress.removed[]`），
//     唯一来源是 AppliedEvidence（LESSONS L-012 /  support），无命中时为 nil；
//   - err：数据无法按 JSON 外形比对时返回错误（fail-closed：调用方必须据此拒绝出域，
//     不得把「不可判定」当成「没有敏感字段」）。
//
// extraFields 是**唯一的配置入口**，语义是**只增不减**（DESIGN D6「配置仅可增不可减」）：
// 按规范化后的字段名**精确**追加；传 nil / 空切片 / 空串 / 无关名都不会减少内建集合，
// 也无法把黑名单置空关闭 —— 因为本函数没有任何开关形参，包内也不存在减项 API。
func ApplyBlacklist(data any, extraFields ...string) (masked any, removed []string, err error) {
	if data == nil {
		return nil, nil, nil
	}

	shape, err := toJSONShape(data)
	if err != nil {
		return nil, nil, fmt.Errorf("security: blacklist: %w", err)
	}

	pruned := blacklistPrune(shape, blacklistExtras(extraFields))

	// 证据的唯一合法来源是 AppliedEvidence：禁止此处手写 diff，
	// 也禁止用「检查过的字段数」等过程量填充（LESSONS L-012）。
	removed, err = AppliedEvidence(shape, pruned)
	if err != nil {
		return nil, nil, fmt.Errorf("security: blacklist: applied evidence: %w", err)
	}
	return pruned, removed, nil
}

// blacklistExtras 规范化增项字段名；只有非空名字会被登记（空/空白 = 无操作，不等于关闭）。
func blacklistExtras(extraFields []string) map[string]struct{} {
	if len(extraFields) == 0 {
		return nil
	}
	extras := make(map[string]struct{}, len(extraFields))
	for _, field := range extraFields {
		if normalized := blacklistNormalizeKey(field); normalized != "" {
			extras[normalized] = struct{}{}
		}
	}
	if len(extras) == 0 {
		return nil
	}
	return extras
}

// blacklistNormalizeKey 把字段名归一为「小写 + 下划线分词」：分隔符（`-` `.` 空格 `/` `:` 等）
// 折叠为 `_`，驼峰边界插入 `_`，首尾 `_` 去除。`IMEI` / `imei-ClientId` / `ClientId`
// 因而分别归一为 `imei` / `imei_client_id` / `client_id`。
func blacklistNormalizeKey(key string) string {
	normalized := make([]rune, 0, len(key))
	for _, r := range key {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			if unicode.IsUpper(r) && len(normalized) > 0 && blacklistIsLowerOrDigit(normalized[len(normalized)-1]) {
				normalized = append(normalized, '_')
			}
			normalized = append(normalized, unicode.ToLower(r))
			continue
		}
		if len(normalized) > 0 && normalized[len(normalized)-1] != '_' {
			normalized = append(normalized, '_')
		}
	}
	return strings.Trim(string(normalized), "_")
}

func blacklistIsLowerOrDigit(r rune) bool {
	return unicode.IsLower(r) || unicode.IsDigit(r)
}

// blacklistPrune 递归生成**新**的 JSON 外形副本，命中字段整体删除。
//
// 只有「删除」一种动作：绝不写入哈希 / 截断 / 掩码等替代值（ADR-001 决策 5），
// 也不重命名 key。未命中的标量子树按引用共享（只读，不再改写），因此不会改到调用方数据。
func blacklistPrune(node any, extras map[string]struct{}) any {
	switch value := node.(type) {
	case map[string]any:
		pruned := make(map[string]any, len(value))
		for key, child := range value {
			if blacklistHit(key, extras) {
				continue
			}
			pruned[key] = blacklistPrune(child, extras)
		}
		return pruned
	case []any:
		pruned := make([]any, len(value))
		for i, child := range value {
			pruned[i] = blacklistPrune(child, extras)
		}
		return pruned
	default:
		return node
	}
}

// blacklistHit 报告字段名是否属于「永不外发」集合（内建规则 ∪ 配置增项）。
func blacklistHit(key string, extras map[string]struct{}) bool {
	normalized := blacklistNormalizeKey(key)
	if normalized == "" {
		return false
	}
	if _, ok := extras[normalized]; ok {
		return true
	}
	return blacklistBuiltinHit(normalized)
}

func blacklistBuiltinHit(normalized string) bool {
	words := strings.Split(normalized, "_")
	compact := strings.ReplaceAll(normalized, "_", "")

	for _, word := range words {
		for _, forbidden := range blacklistWords {
			if word == forbidden {
				return true
			}
		}
	}
	for _, fragment := range blacklistFragments {
		if strings.Contains(compact, fragment) {
			return true
		}
	}
	for _, prefix := range blacklistPrefixes {
		if strings.HasPrefix(compact, prefix) {
			return true
		}
	}
	for _, suffix := range blacklistCompactSuffixes {
		if strings.HasSuffix(compact, suffix) {
			return true
		}
	}
	return blacklistInternalHostHit(words)
}

func blacklistInternalHostHit(words []string) bool {
	inScope, isHostTarget := false, false
	for _, word := range words {
		if !inScope {
			inScope = blacklistContains(blacklistHostScopeWords, word)
		}
		if !isHostTarget {
			isHostTarget = blacklistContains(blacklistHostTargetWords, word)
		}
	}
	return inScope && isHostTarget
}

func blacklistContains(candidates []string, want string) bool {
	for _, candidate := range candidates {
		if candidate == want {
			return true
		}
	}
	return false
}
