package security

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// ============================================================================
// 出域策略表（默认 deny）+ Auditor 接口
//   （最小必要子集）· （未覆盖域靠黑名单 + 最小化）· （审计证据接口）
//    出口职责链 /  fail-closed 边界 /  D-5 & D-24 /  跨模块契约
//   框架裁决 决策 6（未声明默认 deny）· 决策 7（审计不落原文，只落可交叉核验证据）
//   框架裁决 决策 2（证据式契约）· 决策 5（未覆盖域写死白名单、禁用 attr_filter）
//
// 与既有 `controllers.AttrFilter` 的关系（ / 框架裁决 · LESSONS 既有教训）：
// 既有脱敏是 **fail-open + 黑名单语义**（`sign` 未登记则不过滤、无 ctx / SkipEnv / 查询出错一律
// 原样返回，宿主接入层 .go:120-147），页面上后果是「字段可见」，出域后果是「数据泄露」，量级不同。
// 因此本表**刻意不复用**其语义、也不改写它，只在其之上叠加一层 **fail-closed** 判定：
// **未声明 ⇒ deny；无法判定 ⇒ 拒绝（返回 error，而不是放行）**。
// ============================================================================

// 出域判定失败时的哨兵错误（形态沿用 pkg/agentkit/model/endpoint.go 的「稳定标识 + errors.Is」约定，
// 使调用方既能用 errors.Is 做程序判定，也能把 err 直接作为 `LOG:egress.policy.missing` 的 reason 落日志）。
var (
	// ErrEgressSignBlank sign 为空 ⇒ 无从查表（fail-closed）。
	ErrEgressSignBlank = errors.New("egress.sign.blank: sign 为空，无法判定出域策略（默认 deny）")
	// ErrEgressPolicyMissing sign 未在本策略表中声明（D5 的核心判据：未声明默认 deny）。
	ErrEgressPolicyMissing = errors.New("egress.policy.missing: sign 未在出域策略表中声明（默认 deny）")
	// ErrEgressPolicyNotInjected 策略表未注入（nil / 零值）⇒ fail-closed，不因「没配就没限制」而放行。
	ErrEgressPolicyNotInjected = errors.New("egress.policy.not_injected: 出域策略表未注入（默认 deny）")
	// ErrEgressModeUnusable 已登记但模式不可判定（不可用模式 / 白名单为空）⇒ 拒绝，不放行。
	ErrEgressModeUnusable = errors.New("egress.mode.unusable: 出域策略模式不可判定（默认 deny）")

	// ErrEgressRuleInvalid 登记项非法（构造期即拒，不留运行期不可判定项）。
	ErrEgressRuleInvalid = errors.New("egress.rule.invalid: 出域策略登记项非法")
	// ErrEgressRuleBuiltinFrozen 内建策略项写死，不可被配置改写（不得降级模式、不得放宽白名单）。
	ErrEgressRuleBuiltinFrozen = errors.New("egress.rule.builtin_frozen: 内建出域策略项不可被配置改写")

	// ErrEgressHMACKeyInvalid 审计 HMAC 密钥缺失或非法 ⇒ 判失败不出域（框架裁决 · 决策 7：不做「跳过审计」）。
	ErrEgressHMACKeyInvalid = errors.New("egress.audit.hmac_key_invalid: 出域审计 HMAC 密钥缺失或非法")
	// ErrEgressEvidenceInvalid 审计证据不可判定（policy_sign 为空 / 计数为负 / 借用规则通道塞入原文）⇒ 拒绝。
	ErrEgressEvidenceInvalid = errors.New("egress.audit.evidence_invalid: 出域审计证据非法")
)

// EgressMode 是一条出域策略的脱敏模式（闭集，写死两种）。
//
//   - EgressModeAttrFilter：复用既有 `controllers.AttrFilter` 同一份代码与规则，**仅限 4 个已覆盖域**；
//     未登记即拒（未登记 sign 不等于「无规则=通过」）——判定必须结合「applied 证据 + 该 sign 的登记规则
//     逐条生效」核验（ / 框架裁决 判失败规则 ③④⑤⑥），本表只负责给出模式。
//   - EgressModeMinimalAllowlist：**未覆盖域写死**的最小化白名单，逐项列举可出域字段；
//     禁止未覆盖域使用 attr_filter（其黑名单语义对未登记 sign 等于空脱敏，D-24 / G2 安全评审 B2）。
type EgressMode string

const (
	// EgressModeAttrFilter 展示同口径脱敏（既有 AttrFilter 同一实现）。
	EgressModeAttrFilter EgressMode = "attr_filter"
	// EgressModeMinimalAllowlist 最小化白名单（未覆盖域写死，逐项列举）。
	EgressModeMinimalAllowlist EgressMode = "minimal_allowlist"
)

// EgressRule 是一条出域策略登记项：`sign → {attr_filter | minimal_allowlist}`。
type EgressRule struct {
	// Sign 是脱敏 sign（与既有 AttrFilter 的 sign 同源）。**不参与归一化**：查表为精确匹配。
	Sign string
	// Mode 见 EgressMode。
	Mode EgressMode
	// AllowFields 仅 minimal_allowlist 使用：可出域字段的**逐项白名单**（attr_filter 模式必须为空）。
	AllowFields []string
}

// EgressDecision 是一次出域策略判定结果（**只在放行时返回**；被拒时返回零值 + error）。
type EgressDecision struct {
	// Sign 命中的 sign。
	Sign string
	// Mode 该 sign 的脱敏模式。
	Mode EgressMode
	// AllowFields 是 minimal_allowlist 的字段白名单副本（attr_filter 模式为空）。调用方可以改它，不影响策略表。
	AllowFields []string
}

// EgressPolicy 是出域策略表（**默认 deny**，fail-closed）。
//
// 「默认 deny」是**代码结构上的事实**，不是文档承诺：
//
//	① 唯一判定入口 Decide 对「未声明 sign / 空 sign / 未注入策略表 / 模式不可判定」一律返回 error，
//	   不存在返回「放行」的其它路径，也不返回任何可被误当放行的中间结果；
//	② 内建表是包级**未导出**变量，外部包不可见、句柄不可写，随二进制发版；
//	③ 外部唯一的增项入口是 NewEgressPolicy 的 extra 变参，**只允许新增 minimal_allowlist 域**：
//	   内建项不可被改写（既不能把未覆盖域降级成 attr_filter，也不能放宽内建白名单），
//	   新 sign 不得使用 attr_filter（该模式仅限 4 个已覆盖域），空白名单 / 命中永不外发黑名单的
//	   白名单一律拒登；本包不导出任何 Set / Replace / Remove / Clear / Disable / AllowAll 形式的 API；
//	④ 以上由 TestGuard_EgressPolicyDefaultDeny 以「默认 deny 四态 + 内建表冻结 + 导出面 AST 扫描」固化。
//
// 策略表构造后**只读**（仅 map 读操作），可被多 goroutine 并发使用。
type EgressPolicy struct {
	rules map[string]EgressRule
}

// coveredEgressDomains 是 `AttrFilter` 已覆盖的 4 个域（D-2 / D-6：对外能力清单冻结在这 4 域）。
// 只有它们允许用 attr_filter 模式。
var coveredEgressDomains = []string{"ip", "domain", "url", "file"}

// builtinEgressRules 是内建的出域策略表（包级未导出：外部不可见、不可替换、随二进制发版）。
//
// 未覆盖域（midm2 / cert / cve / apt / virus / mobile / mac / eterprise / log / code / permission）
// **写死** minimal_allowlist 并**逐项列举**可出域字段（D-24 / ）；`mobile` 的 `imei` /
// `imei_clientid` / `clientid` 等设备标识**一律不出域**。白名单字段刻意只留
// 「入口命令必需且非个人标识」的维度；字段清单本身是人工产物（R5 已登记的残余风险），
// 任何增删都会让 TestEgressPolicy_BuiltinTableFrozen 失败。
//
//	追加：`log`（日志问答链路 `query_log`，sign 由 `既有实现/capability.CapabilityLog` 提供）
//
// 原先**漏登记**在本表 ⇒ `Decide("log")` 走默认 deny ⇒ 日志链路在生产上恒 fail-closed。
// 本条是**纯加法登记**：未改动既有 4 域与既有未覆盖域的任何一条。
//
//	追加：`code`（代码问答链路 `query_code`）与 `permission`（权限问答链路 `query_permission`），
//
// sign 分别由 `既有实现/capability.CapabilityCode` / `.CapabilityPermission` 提供。
// 二者原先同样**漏登记**在本表 ⇒ `Decide("code")` / `Decide("permission")` 走默认 deny
// ⇒ 两条链路在生产上恒 fail-closed（功能等于不存在）。同为**纯加法登记**：既有 4 域与既有
// 未覆盖域的每一条规则、每一个字段均未改动（由 TestEgressPolicy_BuiltinTableFrozen 的
// 共享登记表 wantAllowlists 与 TestGuard_EveryCapabilityHasEgressRule 双向钉住）。
// 字段来源与容器键理由见各自条目上方的注释（先读生产代码再定，未凭空猜测）。
var builtinEgressRules = []EgressRule{
	// 已覆盖域：attr_filter（未命中即拒，绝不视为「无规则=通过」）
	{Sign: "ip", Mode: EgressModeAttrFilter},
	{Sign: "domain", Mode: EgressModeAttrFilter},
	{Sign: "url", Mode: EgressModeAttrFilter},
	{Sign: "file", Mode: EgressModeAttrFilter},

	// 未覆盖域：写死 minimal_allowlist（禁止 attr_filter）
	{Sign: "midm2", Mode: EgressModeMinimalAllowlist, AllowFields: []string{
		"first_seen", "last_seen", "mid", "mid_type", "status", "update_time",
	}},
	{Sign: "cert", Mode: EgressModeMinimalAllowlist, AllowFields: []string{
		"cert_serial", "issuer", "not_after", "not_before", "sha1", "sha256", "subject", "update_time",
	}},
	{Sign: "cve", Mode: EgressModeMinimalAllowlist, AllowFields: []string{
		"cve_id", "cvss", "description", "published_at", "reference", "severity", "update_time",
	}},
	{Sign: "apt", Mode: EgressModeMinimalAllowlist, AllowFields: []string{
		"aliases", "family", "first_seen", "last_seen", "name", "target_industry", "update_time",
	}},
	{Sign: "virus", Mode: EgressModeMinimalAllowlist, AllowFields: []string{
		"engine", "family", "file_type", "first_seen", "md5", "sha1", "sha256", "signature", "update_time",
	}},
	{Sign: "mobile", Mode: EgressModeMinimalAllowlist, AllowFields: []string{
		"app_name", "first_seen", "last_seen", "os_version", "status", "update_time",
	}},
	{Sign: "mac", Mode: EgressModeMinimalAllowlist, AllowFields: []string{
		"first_seen", "last_seen", "oui", "update_time", "vendor",
	}},
	{Sign: "eterprise", Mode: EgressModeMinimalAllowlist, AllowFields: []string{
		"company_name", "industry", "location", "registered_at", "status", "update_time",
	}},
	// ：日志链路（`既有管理面/log.go` 的 `LogQA` 载荷）。字段**实测自该文件的载荷形状**，
	// 非推测：`LogQA` json:`items/scope/aggregate/empty` · `LogItem` json:`time/path/status`
	// · `LogAggregate` json:`value/metric/scope`。其中 `items` / `aggregate` 是**容器键**：
	// 最小化白名单按字段名**逐层精确比对**（`既有模型层/blacklist.go minimizeByAllowlist`：
	// 父键不在白名单 ⇒ 整个子树被整体移除）⇒ 容器键必须与叶子键一并列举，否则 `items[]` 被静默裁空。
	// 未列举的 `clarify` / `assumed_default` / `awaiting_confirm` / `gap` / `masked_fields`
	// 属控制面与证据字段，不在「最小必要子集」内。
	//
	// ⚠️ 如实登记不确定性：真实上游（日志微服务的日志助手 agent）回执**是否另含可脱敏字段**
	// （ 待裁决项 ③ 提到的 `content` / `ext` 一类）**尚未确认** —— 该端口的真实绑定属**装配补丁阶段**，
	// 届时须按真实回执字段集核对；若需增补，走与 `cert` 相同的「显式登记 + 逐项列举」流程
	//（本层不凭空猜测未见的字段名，也不为便利改用 attr_filter）。
	{Sign: "log", Mode: EgressModeMinimalAllowlist, AllowFields: []string{
		"aggregate", "empty", "items", "metric", "path", "scope", "status", "time", "value",
	}},
	// ：代码问答链路（`query_code`）。字段**逐条实测自 `既有管理面/code.go` 的实际产出**
	// （struct json tag），非推测。该链路出域 `data` 的两条**已登记候选形状**都逐项列举 ——
	// 装配补丁（`既有实现/tools` 的 `Deps.QueryCode` 真实绑定）尚未落定，任取其一都可能
	// 把另一种形状的父键判成「未列举」⇒ 静默裁掉整棵子树（漏登记比多登记危险得多）：
	//
	//	(A) 能力边界载荷 = `Result.data` = 端口回执 `CodeIndexAnswer`（`CodeIndex.Lookup` 的返回值）
	//	    json:`generated_at` · `stale` · `owners[]{author,commits,last_at}` ·
	//	    `call_chain[]{layer,file,symbol,detail}` · `dependencies[]` · `tab_apis[]`
	//	(B) 引擎响应载荷 = `CodeQA`（`admin.QueryCode` 的返回值）
	//	    json:`answer{owner,source}` · `limits[]` · `unanswerable` · `reason` ·
	//	    `index{generated_at,stale}` · `call_chain[]` · `dependencies[]` · `notice` ·
	//	    `latency{total,budget,within_budget}` · `gap{reason}` · `empty`
	//
	// `owners` / `call_chain` / `dependencies` / `tab_apis` / `answer` / `index` / `latency` / `gap`
	// 是**容器键**：最小化白名单按字段名**逐层精确比对**（`既有模型层/blacklist.go`
	// `minimizeByAllowlist`：父键不在白名单 ⇒ 整个子树被整体移除）⇒ 容器键必须与叶子键一并列举，
	// 否则 `call_chain[]` / `owners[]` / `latency{}` 会被**静默裁空且不报错**（功能不可用）。
	// 刻意**未**列举 `masked_fields`：它是脱敏证据，其**取值**是字段路径，而字段路径名本身会泄露
	// 语义（：审计侧只落哈希）⇒ 不进出域白名单（与  对 `log` 的同款处置）。
	// ⚠️ 与  的**有意差异**： 把 `gap` 一并当作控制面排除；本条目**保留** `gap` / `latency` /
	// `limits` / `unanswerable` / `reason` / `notice`，因为它们是 ~4.3-5 要求**回答本体**承载的
	// 语义（缺口原因 / 不可答原因 / 索引过期提示 / 限制说明）—— 把它们裁掉会让「取数缺口」在模型侧
	// 退化成「看起来成功的空结果」，正是  要消灭的那类**静默裁空**。
	{Sign: "code", Mode: EgressModeMinimalAllowlist, AllowFields: []string{
		"answer", "author", "budget", "call_chain", "commits", "dependencies", "detail",
		"empty", "file", "gap", "generated_at", "index", "last_at", "latency", "layer",
		"limits", "notice", "owner", "owners", "reason", "source", "stale", "symbol",
		"tab_apis", "total", "unanswerable", "within_budget",
	}},
	// ：权限问答链路（`query_permission`）。字段**逐条实测自 `既有管理面/permission.go`
	// 的实际产出**（struct json tag），非推测；与 `code` 条目同款，两条已登记候选形状都列举：
	//
	//	(A) 能力边界载荷 = `Result.data` = 端口回执 `PermissionDirectoryAnswer`
	//	    json:`attr_filter{role,scope,field_visibility[]{field,rule}}` ·
	//	    `expression{…同形…}`（两个并列容器：非 nil ⇔ 该口径给出了结论，D-3）
	//	(B) 引擎响应载荷 = `PermissionQA`（`admin.QueryPermission` 的返回值）
	//	    json:`permissions[]{field,rule}` · `roles[]{role,scope,field_visibility[]{field,rule}}` ·
	//	    `basis` · `warning` · `limits[]` · `latency{total,budget,within_budget}` ·
	//	    `gap{reason}` · `empty`
	//
	// 容器键（`attr_filter` / `expression` / `permissions` / `roles` / `field_visibility` /
	// `latency` / `gap`）必须显式列举，理由同 `code` 条目（父键缺失 ⇒ 整棵子树被静默移除）。
	// 刻意**未**列举 `masked_fields`（脱敏证据 / 字段路径，同 `code` 条目）；
	// 保留 `basis` / `warning` / `limits` / `latency` / `gap` 的理由亦同 `code` 条目（ 的
	// 「两套口径必须标注依据、不得混用」若把 `basis` / `warning` 裁掉，就等于让模型看不到依据）。
	{Sign: "permission", Mode: EgressModeMinimalAllowlist, AllowFields: []string{
		"attr_filter", "basis", "budget", "empty", "expression", "field", "field_visibility",
		"gap", "latency", "limits", "permissions", "reason", "role", "roles", "rule", "scope",
		"total", "warning", "within_budget",
	}},
}

// NewEgressPolicy 构造策略表 = 内建表 ∪ extra。
//
// extra 的合法形态**只有一种**：新 sign + minimal_allowlist + 非空白名单。其余一律构造即拒
// （fail-closed：宁可拒绝登记，也不留下运行期「不可判定」的策略项）：
//
//	① 内建 sign ⇒ ErrEgressRuleBuiltinFrozen（内建项写死：不得降级模式、不得放宽白名单）；
//	② 新 sign 用 attr_filter ⇒ ErrEgressRuleInvalid（该模式仅限 4 个已覆盖域，D-24）；
//	③ 白名单为空 / 全空白 / 含「永不外发」字段 ⇒ ErrEgressRuleInvalid；
//	④ sign 为空 / 带前后空白 / 模式不在闭集 / 重复登记 ⇒ ErrEgressRuleInvalid。
func NewEgressPolicy(extra ...EgressRule) (*EgressPolicy, error) {
	rules := make(map[string]EgressRule, len(builtinEgressRules)+len(extra))

	for _, rule := range builtinEgressRules {
		if _, err := egressRuleNormalize(rule); err != nil {
			// 内建常量非法属于编码错误：此处必须报错而不是静默跳过（跳过等于悄悄丢掉一条策略）。
			return nil, fmt.Errorf("security: egress policy: 内建策略表非法（编码错误）: %w", err)
		}
		rules[rule.Sign] = egressRuleCopy(rule)
	}

	for _, rule := range extra {
		// 内建项判定**先于**模式校验：对已登记的 sign，「内建项写死、不可被配置改写」比
		// 「该模式不适用」更准确地描述了拒绝原因（也避免用一条模式错误掩盖改写意图）。
		if sign := rule.Sign; strings.TrimSpace(sign) != "" && sign == strings.TrimSpace(sign) && egressIsBuiltinSign(sign) {
			return nil, egressBuiltinFrozenError(sign)
		}
		normalized, err := egressRuleNormalize(rule)
		if err != nil {
			return nil, err
		}
		if _, dup := rules[normalized.Sign]; dup {
			return nil, fmt.Errorf("%w: sign %q 被重复登记（登记项含义有歧义 ⇒ 拒绝）",
				ErrEgressRuleInvalid, normalized.Sign)
		}
		rules[normalized.Sign] = normalized
	}

	return &EgressPolicy{rules: rules}, nil
}

// BuiltinEgressRules 返回内建策略表的**只读副本**（按 sign 排序，确定性输出）。
//
// 用途：守卫测试据此与服务侧断言「4 域 attr_filter + 11 域 minimal_allowlist 逐项不变」，
// 调用方据此做启动自检与审计；改动返回值不会影响策略表。
func BuiltinEgressRules() []EgressRule {
	out := make([]EgressRule, 0, len(builtinEgressRules))
	for _, rule := range builtinEgressRules {
		out = append(out, egressRuleCopy(rule))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Sign < out[j].Sign })
	return out
}

// CoveredEgressDomains 返回 `AttrFilter` 已覆盖的 4 个域（只读副本，字典序）。
//
// 冻结依据：D-2 / D-6（`ip` / `domain` / `url` / `file`）——对外能力清单与 attr_filter 模式都只在这 4 域内。
func CoveredEgressDomains() []string {
	out := append([]string(nil), coveredEgressDomains...)
	sort.Strings(out)
	return out
}

// Decide 判定一个 sign 是否允许出域，并给出应采用的脱敏模式。
//
// fail-closed 契约：
//
//	未声明 sign / 空 sign / 策略表未注入 / 模式不可判定 ⇒ 返回 error，**且 decision 为零值**
//	（被拒时绝不返回 Mode 或白名单，避免调用方「只读 decision 不读 err」把拒绝当放行）。
//
// 查表为**精确匹配**，不做 trim / 大小写 / 别名归一：任何「放宽匹配」都等于放宽默认 deny。
func (p *EgressPolicy) Decide(sign string) (EgressDecision, error) {
	if p == nil || p.rules == nil {
		return EgressDecision{}, fmt.Errorf("%w（sign=%q）", ErrEgressPolicyNotInjected, sign)
	}
	if strings.TrimSpace(sign) == "" {
		return EgressDecision{}, fmt.Errorf("%w（sign=%q）", ErrEgressSignBlank, sign)
	}

	rule, ok := p.rules[sign]
	if !ok {
		// 未登记 ≠ 「无规则=通过」：这是默认 deny 的核心判据（ / 框架裁决 决策 5）。
		return EgressDecision{}, fmt.Errorf("%w（sign=%q；漏登记须补登记出域策略）",
			ErrEgressPolicyMissing, sign)
	}

	switch rule.Mode {
	case EgressModeAttrFilter:
		return EgressDecision{Sign: rule.Sign, Mode: rule.Mode}, nil
	case EgressModeMinimalAllowlist:
		if len(rule.AllowFields) == 0 {
			// 理论不可达（构造期已拒），保留运行期兜底：不可判定 ⇒ 拒绝。
			return EgressDecision{}, fmt.Errorf("%w（sign=%q 未逐项列举字段）", ErrEgressModeUnusable, sign)
		}
		return EgressDecision{
			Sign:        rule.Sign,
			Mode:        rule.Mode,
			AllowFields: append([]string(nil), rule.AllowFields...),
		}, nil
	default:
		return EgressDecision{}, fmt.Errorf("%w（sign=%q mode=%q）", ErrEgressModeUnusable, sign, string(rule.Mode))
	}
}

// egressBuiltinFrozenError 构造「内建策略项不可改写」的拒绝错误。
func egressBuiltinFrozenError(sign string) error {
	return fmt.Errorf("%w: sign %q（内建项写死：既不能把未覆盖域降级为 attr_filter，也不能把已覆盖域改成白名单、放宽白名单）",
		ErrEgressRuleBuiltinFrozen, sign)
}

// egressRuleNormalize 校验并规范化一条登记项：去空白 + 去重 + 排序白名单，非法即报错。
func egressRuleNormalize(rule EgressRule) (EgressRule, error) {
	sign := rule.Sign
	if strings.TrimSpace(sign) == "" || sign != strings.TrimSpace(sign) {
		return EgressRule{}, fmt.Errorf("%w: sign %q 为空或带前后空白（不做归一化，避免放宽默认 deny）",
			ErrEgressRuleInvalid, rule.Sign)
	}

	switch rule.Mode {
	case EgressModeAttrFilter:
		if !egressIsCoveredDomain(sign) {
			return EgressRule{}, fmt.Errorf(
				"%w: sign %q 不得使用 attr_filter —— 该模式仅用于已覆盖的 4 域 %v（未覆盖域用 attr_filter 等于空脱敏，D-24）",
				ErrEgressRuleInvalid, sign, CoveredEgressDomains())
		}
		if len(rule.AllowFields) > 0 {
			return EgressRule{}, fmt.Errorf("%w: sign %q 是 attr_filter 模式，不得携带白名单字段",
				ErrEgressRuleInvalid, sign)
		}
		return EgressRule{Sign: sign, Mode: EgressModeAttrFilter}, nil

	case EgressModeMinimalAllowlist:
		fields := egressAllowFieldsNormalize(rule.AllowFields)
		if len(fields) == 0 {
			return EgressRule{}, fmt.Errorf(
				"%w: sign %q 的 minimal_allowlist 未逐项列举可出域字段（空白名单不可判定 ⇒ 拒绝登记）",
				ErrEgressRuleInvalid, sign)
		}
		for _, field := range fields {
			// 复用  的键级黑名单判定（不复制规则）：白名单不得给「永不外发」字段开出口。
			if blacklistHit(field, nil) {
				return EgressRule{}, fmt.Errorf(
					"%w: sign %q 的白名单字段 %q 命中「永不外发」黑名单（白名单不得给敏感字段开出口）",
					ErrEgressRuleInvalid, sign, field)
			}
		}
		return EgressRule{Sign: sign, Mode: EgressModeMinimalAllowlist, AllowFields: fields}, nil

	default:
		return EgressRule{}, fmt.Errorf("%w: sign %q 的模式 %q 不在 {%s, %s} 闭集内",
			ErrEgressRuleInvalid, sign, string(rule.Mode), EgressModeAttrFilter, EgressModeMinimalAllowlist)
	}
}

// egressAllowFieldsNormalize 规范化白名单：去空白、丢弃空项、去重、排序。
func egressAllowFieldsNormalize(fields []string) []string {
	if len(fields) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(fields))
	out := make([]string, 0, len(fields))
	for _, field := range fields {
		trimmed := strings.TrimSpace(field)
		if trimmed == "" {
			continue
		}
		if _, dup := seen[trimmed]; dup {
			continue
		}
		seen[trimmed] = struct{}{}
		out = append(out, trimmed)
	}
	if len(out) == 0 {
		return nil
	}
	sort.Strings(out)
	return out
}

// egressRuleCopy 深拷贝一条登记项（列表字段必须拷贝：内建表不得被调用方改到）。
func egressRuleCopy(rule EgressRule) EgressRule {
	rule.AllowFields = append([]string(nil), rule.AllowFields...)
	return rule
}

// egressIsCoveredDomain 判定 sign 是否属 `AttrFilter` 已覆盖的 4 个域（精确匹配）。
func egressIsCoveredDomain(sign string) bool {
	for _, domain := range coveredEgressDomains {
		if sign == domain {
			return true
		}
	}
	return false
}

// egressIsBuiltinSign 判定 sign 是否为内建登记项（精确匹配）。
func egressIsBuiltinSign(sign string) bool {
	for _, rule := range builtinEgressRules {
		if sign == rule.Sign {
			return true
		}
	}
	return false
}

// ============================================================================
// Auditor：出域审计的外发证据接口
//
// 审计为什么**不落原文**：审计长期留存且读者更多，落 prompt/completion 原文等于把一次受控出域
// 复制成一份不受控的本地副本，使脱敏与黑名单全部失效；而未覆盖域（D-2）本就可能含未脱敏字段。
// 但「只落自报字段」无法证明「外发内容确实脱敏」⇒ 证据必须是可**交叉核验**的摘要：
//
//	sent_hmac（带服务端密钥的 HMAC-SHA256）/ sent_bytes / masker_applied[]（规则 sign + 字段名哈希）
//	/ policy_sign / fence_hits
//
// 校验方持有同一密钥即可独立重算 sent_hmac，把「模型到底看到了什么」变成可证明的事实。
// ============================================================================

// EgressAudit 是一次出域的外发证据。
//
// **结构上不承载原文**：五个字段全部是摘要，没有任何字段能容纳 payload / prompt / completion 原文，
// 也没有 any / map / []byte 这类「什么都能装」的类型 —— 因此「不落原文」不是纪律要求而是类型事实
// （TestEgressPolicy_AuditEvidenceCarriesNoRawText 用 reflect 扫描字段集合与类型 + 功能断言双重固化）。
//
// 字段全部**未导出**：包外唯一构造入口是 NewEgressEvidence，它只产出摘要并丢弃原文，
// 因而外部无法把明文塞进证据（与 LESSONS 既有教训「applied 证据只有一个落点」同源思路）。
// 零值不是有效证据（Verifiable 为假）——实现方必须检查，否则等于伪造一次成功审计。
type EgressAudit struct {
	policySign    string
	sentBytes     int
	sentHMAC      string
	maskerApplied []string
	fenceHits     int
}

// egressHMACKeyMinBytes 是审计 HMAC 密钥的最小长度（128 bit）。
// 密钥过短同样视为「非法」：低熵密钥可被枚举反推，等于把字段名哈希变回明文。
const egressHMACKeyMinBytes = 16

// egressFieldNameFingerprintPrefix 标记「字段名哈希」条目，使 masker_applied[] 中
// 「规则 sign（明文）」与「字段名哈希」可区分（ 统一口径）。
const egressFieldNameFingerprintPrefix = "fn:"

// egressFieldNameDomainSep 是字段名哈希的域分隔前缀：避免「字段名哈希」与「payload HMAC」
// 在同一密钥下互相碰撞（同一密钥服务两个用途时必须做域分隔）。
const egressFieldNameDomainSep = "security.egress.field:"

// egressAppliedRuleSignMaxBytes 是规则 sign 条目的长度上限。
const egressAppliedRuleSignMaxBytes = 64

// Auditor 承载一次出域的外发证据。
//
// 契约（实现方即 宿主侧 既有模型层 的出域审计）：
//
//   - 只能落 audit 里的摘要字段，**不得**把 payload / prompt / completion 原文或字段明文路径写进日志
//     （字段路径名本身会泄露语义，如 `imei_clientid` ⇒ 只落字段名哈希）；
//   - 落盘失败必须**返回错误**：出域职责链里「审计」在「调用」之前，失败即不出域（fail-closed，
//     不因「审计挂了」就把数据发出去）；
//   - 本接口是**唯一**的证据投递面：它拿不到原文，也无从「要求调用方补一份原文」。
type Auditor interface {
	Record(ctx context.Context, audit EgressAudit) error
}

// NewEgressEvidence 构造一次出域的外发证据：**只**产出摘要，原文就地丢弃。
//
// 入参与语义：
//
//   - policySign：本次出域依据的策略 sign（来自 EgressPolicy.Decide）——不落原文，故为空即拒；
//   - payload：**即将出域的实际字节**。只用于算 sent_bytes 与 sent_hmac，函数返回后不再持有，
//     证据里不存在它的任何副本；校验方持同一密钥重算 HMAC-SHA256(key, payload) 即可交叉核验；
//   - hmacKey：服务端密钥（来自配置/环境）。**缺失或过短（< 128 bit）⇒ 返回错误、判失败不出域**，
//     不做「跳过审计」（框架裁决 · 决策 7）；
//   - appliedRules / appliedFieldPaths：来自脱敏证据（masker 的 applied，唯一来源 AppliedEvidence，
//     LESSONS 既有教训）。规则 sign 保留明文（非敏感），字段路径**一律哈希**；字段路径为空即视为没有
//     可落证据（空转必须表现为空 —— 与  兜底判据同源）。
//
// fail-closed：policy_sign 为空 / fence_hits 为负 / 规则项不是「标识符形状」（例如被塞进数字串或含空白
// 的原文）一律返回错误，而不是「尽力落一条」。
func NewEgressEvidence(
	policySign string,
	payload []byte,
	hmacKey []byte,
	appliedRules []string,
	appliedFieldPaths []string,
	fenceHits int,
) (EgressAudit, error) {
	sign := strings.TrimSpace(policySign)
	if sign == "" {
		return EgressAudit{}, fmt.Errorf("%w: policy_sign 为空（无法判定本次出域依据哪条策略）",
			ErrEgressEvidenceInvalid)
	}
	if len(hmacKey) < egressHMACKeyMinBytes {
		return EgressAudit{}, fmt.Errorf(
			"%w: 长度 %d < %d 字节，无法产出可交叉核验的 sent_hmac（判失败不出域）",
			ErrEgressHMACKeyInvalid, len(hmacKey), egressHMACKeyMinBytes)
	}
	if fenceHits < 0 {
		return EgressAudit{}, fmt.Errorf("%w: fence_hits 为负（%d）", ErrEgressEvidenceInvalid, fenceHits)
	}

	applied, err := egressAppliedEvidence(hmacKey, appliedRules, appliedFieldPaths)
	if err != nil {
		return EgressAudit{}, err
	}

	return EgressAudit{
		policySign:    sign,
		sentBytes:     len(payload),
		sentHMAC:      egressHMACSHA256(hmacKey, payload),
		maskerApplied: applied,
		fenceHits:     fenceHits,
	}, nil
}

// egressAppliedEvidence 汇总 masker_applied[]：规则 sign 保留明文，字段路径**一律哈希**，
// 结果排序去重；规则项不是标识符形状即拒绝（该通道是证据里唯一的明文通道）。
func egressAppliedEvidence(hmacKey []byte, appliedRules, appliedFieldPaths []string) ([]string, error) {
	applied := make([]string, 0, len(appliedRules)+len(appliedFieldPaths))
	for _, rule := range appliedRules {
		trimmed := strings.TrimSpace(rule)
		if trimmed == "" {
			continue
		}
		if !egressRuleSignShapeOK(trimmed) {
			return nil, fmt.Errorf(
				"%w: 规则项 %q 不是标识符形状（首字符须为字母/下划线，长度 ≤ %d）—— 拒绝借规则通道落原文",
				ErrEgressEvidenceInvalid, trimmed, egressAppliedRuleSignMaxBytes)
		}
		applied = append(applied, trimmed)
	}
	for _, path := range appliedFieldPaths {
		trimmed := strings.TrimSpace(path)
		if trimmed == "" {
			continue
		}
		applied = append(applied, egressFieldNameFingerprint(hmacKey, trimmed))
	}
	sort.Strings(applied)
	return dedupeSorted(applied), nil
}

// PolicySign 返回本次出域依据的策略 sign。
func (a EgressAudit) PolicySign() string { return a.policySign }

// SentBytes 返回实际出域 payload 的字节数（可与 HTTPDoer 侧实际请求体比对）。
func (a EgressAudit) SentBytes() int { return a.sentBytes }

// SentHMAC 返回 payload 的 HMAC-SHA256（十六进制）。持同一密钥方可独立重算做交叉核验。
func (a EgressAudit) SentHMAC() string { return a.sentHMAC }

// MaskerApplied 返回脱敏证据的**副本**：规则 sign 明文 + 字段名哈希（形如 `fn:<hex>`）。
func (a EgressAudit) MaskerApplied() []string {
	return append([]string(nil), a.maskerApplied...)
}

// FenceHits 返回本次出域被围栏的不可信数据段总数（来自 security.Fence.TotalHits）。
func (a EgressAudit) FenceHits() int { return a.fenceHits }

// Verifiable 判定本条证据是否可用于交叉核验。
//
// 零值（或任何缺了 policy_sign / sent_hmac 的证据）**不可核验**：实现方必须拒绝落这种记录，
// 否则「审计留痕」会退化为一个看起来成功、实际什么都证明不了的空壳（框架裁决 · 决策 2 的同款判据）。
func (a EgressAudit) Verifiable() bool {
	return a.policySign != "" && a.sentHMAC != "" && a.sentBytes >= 0 && a.fenceHits >= 0
}

// egressHMACSHA256 返回 HMAC-SHA256(key, data) 的十六进制摘要。
func egressHMACSHA256(key, data []byte) string {
	mac := hmac.New(sha256.New, key)
	mac.Write(data)
	return hex.EncodeToString(mac.Sum(nil))
}

// egressFieldNameFingerprint 返回字段路径的**带密钥哈希**（带域分隔与 `fn:` 标记）。
//
// 为什么用 HMAC 而不是裸哈希：字段路径名本身会泄露语义（`imei_clientid`），
// 低熵路径的裸哈希可被枚举反推 ⇒ 一律带服务端密钥。
func egressFieldNameFingerprint(key []byte, fieldPath string) string {
	return egressFieldNameFingerprintPrefix + egressHMACSHA256(key, []byte(egressFieldNameDomainSep+fieldPath))
}

// egressRuleSignShapeOK 判定规则项是否为「标识符形状」：首字符为字母/下划线，
// 其余为字母、数字或 `.` `-` `_` `:`，长度 1..egressAppliedRuleSignMaxBytes。
//
// 这不是「格式洁癖」：规则项是证据里唯一的明文通道，形状校验使外部无法借它把
// 数字串（IMEI）/ 含空白文本 / 超长原文塞进审计。
func egressRuleSignShapeOK(sign string) bool {
	if len(sign) == 0 || len(sign) > egressAppliedRuleSignMaxBytes {
		return false
	}
	for i, r := range sign {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r == '_':
			// 首字符与后续字符均允许
		case (r >= '0' && r <= '9') || r == '.' || r == '-' || r == ':':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// dedupeSorted 抹掉已排序切片中的相邻重复项。
func dedupeSorted(sorted []string) []string {
	if len(sorted) < 2 {
		return sorted
	}
	out := sorted[:1]
	for _, item := range sorted[1:] {
		if item != out[len(out)-1] {
			out = append(out, item)
		}
	}
	return out
}
