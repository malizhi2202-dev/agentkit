package security

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
)

// ============================================================================
//  测试集：出域策略表（默认 deny，fail-closed）+ Auditor 接口（不落原文）
//
// 命名约定：全部以 TestEgressPolicy_ 前缀（TASK 的 verify 正则 `TestEgressPolicy|TestGuard_EgressPolicyDefaultDeny`），
// 使「默认 deny」与「审计不落原文」两条硬安全点都在官方 verify 命令内被执行。
//
// 依据：（最小必要子集）· （未覆盖域靠黑名单 + 最小化）· （审计证据接口）
//       ·  D-5 & D-24 /  · ADR-001 决策 6/7 · ADR-008 决策 2/5
// ============================================================================

// wantAttrFilterSigns 是**写死**的 4 个已覆盖域（D-2 / D-6：对外清单冻结在 4 域）。
var wantAttrFilterSigns = []string{"domain", "file", "ip", "url"}

// wantAllowlists 是**写死**的未覆盖域最小化白名单（D-24：逐项列举，禁止 attr_filter）。
//
// ⚠️ 这份期望表刻意与实现独立书写：任何「顺手放宽/悄悄增删字段」都会让本测试失败，
// 从而逼出一轮显式评审（白名单是人工列举产物，R5 已登记）。
//
// 这是「多一处即 FAIL」的**共享登记表**（L-046/L-049 契约 1）：新增 sign 只能**追加式登记**
// 并注明字段来源，不得放宽断言、不得自建旁路。 追加的 `log`（日志链路 `query_log`）字段来源 =
// `既有管理面/log.go` 的**实际载荷形状**：`LogQA` json:`items/scope/aggregate/empty` ·
// `LogItem` json:`time/path/status` · `LogAggregate` json:`value/metric/scope`。`items` / `aggregate`
// 是**容器键**：最小化按字段名逐层精确比对（父键不在白名单 ⇒ 整个子树被移除，
// `既有模型层/blacklist.go minimizeByAllowlist`）⇒ 容器键必须与叶子键并列，否则 `items[]` 被静默裁空。
// 未列举的 `clarify`/`assumed_default`/`awaiting_confirm`/`gap`/`masked_fields` 是控制面与证据字段。
//
//	追加 `code`（代码链路 `query_code`）与 `permission`（权限链路 `query_permission`），字段来源 =
//
// `既有管理面/code.go` / `permission.go` 的**实际产出**（逐条 json tag）：两条链路出域 `data`
// 的**两条已登记候选形状**（能力边界载荷 = `Result.data` = 端口回执 `CodeIndexAnswer` /
// `PermissionDirectoryAnswer`；引擎响应载荷 = `CodeQA` / `PermissionQA`）都逐项列举 ——
// 装配补丁未落定前任取其一都可能把另一种形状的父键判成未列举而静默裁掉子树。
// 容器键（`code`：owners/call_chain/dependencies/tab_apis/answer/index/latency/gap；
// `permission`：attr_filter/expression/permissions/roles/field_visibility/latency/gap）
// 必须与叶子键并列（L-052 契约 2）。两者都**未**列举 `masked_fields`（脱敏证据 / 字段路径，
// ：审计侧只落哈希），这一点与  对 `log` 的处置同款。
var wantAllowlists = map[string][]string{
	"apt":  {"aliases", "family", "first_seen", "last_seen", "name", "target_industry", "update_time"},
	"cert": {"cert_serial", "issuer", "not_after", "not_before", "sha1", "sha256", "subject", "update_time"},
	"code": {
		"answer", "author", "budget", "call_chain", "commits", "dependencies", "detail",
		"empty", "file", "gap", "generated_at", "index", "last_at", "latency", "layer",
		"limits", "notice", "owner", "owners", "reason", "source", "stale", "symbol",
		"tab_apis", "total", "unanswerable", "within_budget",
	},
	"cve":       {"cve_id", "cvss", "description", "published_at", "reference", "severity", "update_time"},
	"eterprise": {"company_name", "industry", "location", "registered_at", "status", "update_time"},
	"log":       {"aggregate", "empty", "items", "metric", "path", "scope", "status", "time", "value"},
	"mac":       {"first_seen", "last_seen", "oui", "update_time", "vendor"},
	"midm2":     {"first_seen", "last_seen", "mid", "mid_type", "status", "update_time"},
	"mobile":    {"app_name", "first_seen", "last_seen", "os_version", "status", "update_time"},
	"permission": {
		"attr_filter", "basis", "budget", "empty", "expression", "field", "field_visibility",
		"gap", "latency", "limits", "permissions", "reason", "role", "roles", "rule", "scope",
		"total", "warning", "within_budget",
	},
	"virus": {"engine", "file_type", "family", "first_seen", "md5", "sha1", "sha256", "signature", "update_time"},
}

// egressBuiltinSignIndex 用 `BuiltinEgressRules` 的**实际返回值**构造 sign → 登记项索引。
//
// L-044 契约 7：判定「某 sign 是否已登记」只能依据实际符号表（这里）或 AST，
// **不得**文本 grep —— 本文件的注释与字符串里必然出现这些 sign 名。
func egressBuiltinSignIndex() map[string]EgressRule {
	rules := BuiltinEgressRules()
	index := make(map[string]EgressRule, len(rules))
	for _, rule := range rules {
		index[rule.Sign] = rule
	}
	return index
}

// wantDeviceIdentifiers 是 mobile 域**一律不得出域**的设备标识（ 明写）。
var wantDeviceIdentifiers = []string{"clientid", "imei", "imei_clientid"}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

// assertDenied 断言「拒绝 + 原因 + 不返回任何可用策略」三件事同时成立。
//
// 特别断言 decision 为零值：被拒时若返回了 Mode/AllowFields，调用方可能「只读 decision 不读 err」
// 从而把一次拒绝当成放行 —— 这是默认 deny 最容易被绕开的写法。
func assertDenied(t *testing.T, decision EgressDecision, err error, want error) {
	t.Helper()
	if err == nil {
		t.Fatalf("必须拒绝（fail-closed），实际放行：decision=%+v", decision)
	}
	if !errors.Is(err, want) {
		t.Fatalf("拒绝原因应为 %v，实际 %v", want, err)
	}
	if decision.Sign != "" || decision.Mode != "" || len(decision.AllowFields) != 0 {
		t.Fatalf("被拒时不得返回任何可用策略（否则可被误当放行）：%+v", decision)
	}
}

// ---------------------------------------------------------------------------
// 硬安全点 ①：未声明的 sign 默认 deny（不是默认放行）
// ---------------------------------------------------------------------------

func TestEgressPolicy_DefaultDenyForUndeclaredSign(t *testing.T) {
	policy, err := NewEgressPolicy()
	if err != nil {
		t.Fatalf("NewEgressPolicy() 不应报错: %v", err)
	}

	t.Run("空 sign", func(t *testing.T) {
		for _, sign := range []string{"", "   ", "\t\n"} {
			decision, err := policy.Decide(sign)
			assertDenied(t, decision, err, ErrEgressSignBlank)
		}
	})

	t.Run("未声明 sign", func(t *testing.T) {
		// 含「形近 / 大小写 / 带空格 / 上位聚合 sign」等常见误用形态：
		// 一律**不做归一化**后放行 —— 任何放宽匹配都等于放宽默认 deny。
		for _, sign := range []string{"user", "entity", "ip2", "IP", "Domain", " ip ", "urls", "mid"} {
			decision, err := policy.Decide(sign)
			assertDenied(t, decision, err, ErrEgressPolicyMissing)
		}
	})

	t.Run("策略表未注入", func(t *testing.T) {
		var nilPolicy *EgressPolicy
		decision, err := nilPolicy.Decide("ip")
		assertDenied(t, decision, err, ErrEgressPolicyNotInjected)

		var zeroPolicy EgressPolicy
		decision, err = zeroPolicy.Decide("ip")
		assertDenied(t, decision, err, ErrEgressPolicyNotInjected)
	})
}

// ---------------------------------------------------------------------------
// 内建表冻结：4 域 attr_filter + N 域 minimal_allowlist
//   N 由 wantAllowlists 决定（ 后为 11：9 既有 + code + permission）：已覆盖域恒为 4 项。
// ---------------------------------------------------------------------------

func TestEgressPolicy_BuiltinTableFrozen(t *testing.T) {
	rules := BuiltinEgressRules()

	got := make(map[string]EgressRule, len(rules))
	for _, rule := range rules {
		if _, dup := got[rule.Sign]; dup {
			t.Fatalf("内建表出现重复 sign: %q", rule.Sign)
		}
		got[rule.Sign] = rule
	}

	if len(got) != len(wantAttrFilterSigns)+len(wantAllowlists) {
		t.Fatalf("内建表条目数应为 %d（4 已覆盖 + %d 未覆盖），实际 %d",
			len(wantAttrFilterSigns)+len(wantAllowlists), len(wantAllowlists), len(got))
	}

	for _, sign := range wantAttrFilterSigns {
		rule, ok := got[sign]
		if !ok {
			t.Fatalf("已覆盖域 %q 必须内建登记", sign)
		}
		if rule.Mode != EgressModeAttrFilter {
			t.Fatalf("%q 必须用 attr_filter 模式，实际 %q", sign, rule.Mode)
		}
		if len(rule.AllowFields) != 0 {
			t.Fatalf("%q 用 attr_filter 模式，不得携带白名单字段：%v", sign, rule.AllowFields)
		}
	}

	for sign, fields := range wantAllowlists {
		rule, ok := got[sign]
		if !ok {
			t.Fatalf("未覆盖域 %q 必须内建登记（写死 minimal_allowlist）", sign)
		}
		if rule.Mode != EgressModeMinimalAllowlist {
			t.Fatalf("%q 必须写死 minimal_allowlist（禁止 attr_filter），实际 %q", sign, rule.Mode)
		}
		if len(rule.AllowFields) == 0 {
			t.Fatalf("%q 必须逐项列举可出域字段，实际为空", sign)
		}
		if !reflect.DeepEqual(sortedCopy(rule.AllowFields), sortedCopy(fields)) {
			t.Fatalf("%q 白名单被改动：\n want=%v\n got =%v", sign, sortedCopy(fields), sortedCopy(rule.AllowFields))
		}
	}

	// 返回的是副本：改它不能改到内建表。
	for i := range rules {
		rules[i].Sign = "tampered"
		rules[i].AllowFields = append(rules[i].AllowFields, "imei")
	}
	if again := BuiltinEgressRules(); reflect.DeepEqual(again, rules) {
		t.Fatal("BuiltinEgressRules 必须返回副本，外部修改不得影响内建表")
	}
}

func TestEgressPolicy_CoveredDomainsFrozenToFour(t *testing.T) {
	got := CoveredEgressDomains()
	if !reflect.DeepEqual(got, wantAttrFilterSigns) {
		t.Fatalf("已覆盖域必须恰为 4 个 %v（契约点 / D-6 冻结），实际 %v", wantAttrFilterSigns, got)
	}
	// 返回副本
	got[0] = "tampered"
	if again := CoveredEgressDomains(); reflect.DeepEqual(again, got) {
		t.Fatal("CoveredEgressDomains 必须返回副本")
	}
}

// ---------------------------------------------------------------------------
// ：日志链路出域 sign（`log`）登记 —— 纯加法，两侧都不得空转
//
// 背景： 登记了 `capability.CapabilityLog = "log"` 而未登记出域规则 ⇒ 未登记期的
// `Decide("log")` 默认 deny（`ErrEgressPolicyMissing`）⇒ 日志链路在生产上恒 fail-closed。
// 本守卫同时钉住**两侧**：`log` 不再被默认拒绝 **且** 未登记 sign / 空 sign 仍拒绝、
// 4 域清单未被波及（D-2/D-6 冻结）。
// ---------------------------------------------------------------------------

func TestGuard_EgressPolicyLogSignRegistered(t *testing.T) {
	policy, err := NewEgressPolicy()
	if err != nil {
		t.Fatalf("NewEgressPolicy() 不应报错: %v", err)
	}

	// 前提核对用**实际符号表**（`BuiltinEgressRules` 的返回值），不是文本 grep（L-044 契约 7）。
	registered := egressBuiltinSignIndex()
	if _, ok := registered["log"]; !ok {
		t.Fatal("内建出域策略表必须登记 sign \"log\"（漏登记 ⇒ Decide(\"log\") 默认 deny ⇒ 日志链路恒 fail-closed）")
	}

	t.Run("①Decide(log) 返回有效判定，不再是默认拒绝", func(t *testing.T) {
		decision, err := policy.Decide("log")
		if err != nil {
			t.Fatalf("log 已登记，不得被默认拒绝（漏登记的后果是日志链路不可用）: %v", err)
		}
		if decision.Sign != "log" || decision.Mode != EgressModeMinimalAllowlist {
			t.Fatalf("log 判定应为 {sign=log, mode=minimal_allowlist}，实际 %+v", decision)
		}
		want, ok := wantAllowlists["log"]
		if !ok || len(want) == 0 {
			t.Fatal("测试前提失效：wantAllowlists 未登记 log（共享登记表的追加式登记缺失）")
		}
		if !reflect.DeepEqual(sortedCopy(decision.AllowFields), sortedCopy(want)) {
			t.Fatalf("log 白名单不符：want=%v got=%v", sortedCopy(want), sortedCopy(decision.AllowFields))
		}
		// 白名单不得给「永不外发」字段开出口（复用  的黑名单判定，不复制规则：R3）。
		for _, field := range decision.AllowFields {
			if blacklistHit(field, nil) {
				t.Fatalf("log 白名单字段 %q 命中「永不外发」黑名单（白名单不得给敏感字段开出口）", field)
			}
		}
	})

	t.Run("②未登记 sign 仍默认拒绝（含 log 的近形形态）", func(t *testing.T) {
		signs := make([]string, 0, len(registered))
		for sign := range registered {
			signs = append(signs, sign)
		}
		sort.Strings(signs)
		// 实测证据：这一行就是「我核对了实际表」的凭据（条数随表增长，跑 -v 可见）。
		t.Logf("BuiltinEgressRules() 实测内建 sign（%d 条）：%v", len(signs), signs)

		// 近形形态（大小写 / 复数 / 拼接 / 前后空白）必须仍被拒 —— 新条目不得放宽精确匹配。
		for _, sign := range []string{"never_declared_sign", "logs", "LOG", "Log", "log2", " log ", "log "} {
			if _, ok := registered[sign]; ok {
				t.Fatalf("测试前提失效：sign %q 已被登记，不能再作为「未登记」判据（请另选探针 sign）", sign)
			}
			decision, err := policy.Decide(sign)
			assertDenied(t, decision, err, ErrEgressPolicyMissing)
		}
	})

	t.Run("③CoveredEgressDomains 仍恰为 4 项且不含 log", func(t *testing.T) {
		got := CoveredEgressDomains()
		if len(got) != 4 || !reflect.DeepEqual(got, wantAttrFilterSigns) {
			t.Fatalf("已覆盖域必须仍恰为 4 项 %v（D-2/D-6 冻结，log 属未覆盖域），实际 %v",
				wantAttrFilterSigns, got)
		}
		for _, domain := range got {
			if domain == "log" {
				t.Fatal("log 不得进入 4 个已覆盖域（D-2/D-6：对外能力清单冻结）")
			}
		}
	})

	t.Run("④空 sign 仍拒绝", func(t *testing.T) {
		for _, sign := range []string{"", "   ", "\t\n"} {
			decision, err := policy.Decide(sign)
			assertDenied(t, decision, err, ErrEgressSignBlank)
		}
	})

	t.Run("⑤log 成内建项 ⇒ 配置不得改写（纯加法的另一半）", func(t *testing.T) {
		for _, weakened := range []EgressRule{
			{Sign: "log", Mode: EgressModeAttrFilter},
			{Sign: "log", Mode: EgressModeMinimalAllowlist, AllowFields: []string{"imei"}},
		} {
			if _, err := NewEgressPolicy(weakened); !errors.Is(err, ErrEgressRuleBuiltinFrozen) {
				t.Fatalf("log 是内建项 ⇒ extra 不得降级模式 / 放宽白名单，实际 %v", err)
			}
		}
	})
}

// ---------------------------------------------------------------------------
// ：admin 另外两条新链路（`code` / `permission`）的出域 sign 登记 —— 纯加法，两侧都不得空转
//
// 背景： 登记了 `capability.CapabilityCode = "code"`、 登记了
// `capability.CapabilityPermission = "permission"`，两条都**未**登记出域规则 ⇒ 未登记期的
// `Decide(sign)` 默认 deny（`ErrEgressPolicyMissing`）⇒ 链路在生产上恒 fail-closed。
//
// 本守卫与  的 `TestGuard_EgressPolicyLogSignRegistered` **同形**（逐 sign 钉住同一组判据），
// 差别只在两点：① 用一份判据循环覆盖两个 sign（R3：同一判据不写两遍）；
// ② 追加一条  专属判据「容器键必须显式列举」（L-052 契约 2）—— 它的表 `t72ContainerKeys`
// 逐条列出每个 sign 上**必须**出现的父键（父键不在白名单 ⇒ 整棵子树被静默移除）。
// ---------------------------------------------------------------------------

// t72ContainerKeys 是  两个 sign 上**必须显式列举**的容器键（父键不在白名单 ⇒ 子树被整体移除）。
//
// 与 wantAllowlists 的关系不是重复登记：wantAllowlists 钉「逐项一致」，本表钉「**为什么**这些键
// 必须在」—— 它们各自带一整棵子树（数组 / 对象），缺一个就是一次**静默裁空**（无报错、功能不可用）。
var t72ContainerKeys = map[string][]string{
	"code": {
		"answer", "call_chain", "dependencies", "gap", "index", "latency", "owners", "tab_apis",
	},
	"permission": {
		"attr_filter", "expression", "field_visibility", "gap", "latency", "permissions", "roles", "scope",
	},
}

// t72NearFormSigns 是  两个 sign 的**近形形态**（大小写 / 复数 / 拼接 / 前后空白 / 上位词）：
// 必须在**运行时**核实它们不在表内才继续判 deny（若将来有人登记了同名 sign，断言会先红，
// 提示另选探针 sign，**不会静默弱化**）。**未用文本 grep 判定**（L-044 契约 7）。
var t72NearFormSigns = []string{
	"never_declared_sign", "codes", "CODE", "Code", "code2", " code ", "code ",
	"permissions", "Permission", "PERMISSION", "permission2", " permission ", "role",
}

func TestGuard_EgressPolicyAdminChainSignsRegistered(t *testing.T) {
	policy, err := NewEgressPolicy()
	if err != nil {
		t.Fatalf("NewEgressPolicy() 不应报错: %v", err)
	}

	// 前提核对用**实际符号表**（`BuiltinEgressRules` 的返回值），不是文本 grep（L-044 契约 7）。
	registered := egressBuiltinSignIndex()
	if len(registered) < 15 {
		t.Fatalf("内建出域策略表只有 %d 条（任务 后应 ≥ 15：4 域 attr_filter + 11 域 minimal_allowlist）"+
			"⇒ 本守卫判定基础过薄", len(registered))
	}
	for _, sign := range t72NearFormSigns {
		if _, ok := registered[sign]; ok {
			t.Fatalf("测试前提失效：sign %q 已被登记，不能再作为「未登记」判据（请另选探针 sign）", sign)
		}
	}

	for _, sign := range []string{"code", "permission"} {
		sign := sign
		t.Run(sign, func(t *testing.T) {
			rule, ok := registered[sign]
			if !ok {
				t.Fatalf("内建出域策略表必须登记 sign %q（漏登记 ⇒ Decide(%q) 默认 deny ⇒ 该链路恒 fail-closed）",
					sign, sign)
			}
			want, ok := wantAllowlists[sign]
			if !ok || len(want) == 0 {
				t.Fatal("测试前提失效：wantAllowlists 未登记 " + sign + "（共享登记表的追加式登记缺失）")
			}

			t.Run("①Decide 返回有效判定，不再是默认拒绝", func(t *testing.T) {
				decision, err := policy.Decide(sign)
				if err != nil {
					t.Fatalf("%q 已登记，不得被默认拒绝（漏登记的后果是该链路不可用）: %v", sign, err)
				}
				if decision.Sign != sign || decision.Mode != EgressModeMinimalAllowlist {
					t.Fatalf("%q 判定应为 {sign=%s, mode=minimal_allowlist}，实际 %+v", sign, sign, decision)
				}
				if !reflect.DeepEqual(sortedCopy(decision.AllowFields), sortedCopy(want)) {
					t.Fatalf("%q 白名单不符：want=%v got=%v", sign, sortedCopy(want), sortedCopy(decision.AllowFields))
				}
				// 白名单不得给「永不外发」字段开出口（复用  的黑名单判定，不复制规则：R3）。
				for _, field := range decision.AllowFields {
					if blacklistHit(field, nil) {
						t.Fatalf("%q 白名单字段 %q 命中「永不外发」黑名单（白名单不得给敏感字段开出口）", sign, field)
					}
				}
			})

			t.Run("②容器键必须显式列举（父键缺失 ⇒ 整棵子树被静默移除）", func(t *testing.T) {
				have := make(map[string]struct{}, len(rule.AllowFields))
				for _, field := range rule.AllowFields {
					have[field] = struct{}{}
				}
				for _, container := range t72ContainerKeys[sign] {
					if _, ok := have[container]; !ok {
						t.Fatalf("%q 的白名单缺容器键 %q —— 最小化白名单逐层精确比对"+
							"（既有模型层/blacklist.go minimizeByAllowlist：父键不在白名单 ⇒ 整棵子树被整体移除）"+
							"⇒ 该子树会被**静默裁空且不报错**（L-052 契约 2）", sign, container)
					}
				}
			})

			t.Run("③未登记 / 近形 sign 仍默认拒绝", func(t *testing.T) {
				signs := make([]string, 0, len(registered))
				for seen := range registered {
					signs = append(signs, seen)
				}
				sort.Strings(signs)
				t.Logf("BuiltinEgressRules() 实测内建 sign（%d 条）：%v", len(signs), signs)

				// 近形形态必须仍被拒 —— 新条目不得放宽精确匹配。
				for _, probe := range t72NearFormSigns {
					if _, ok := registered[probe]; ok {
						t.Fatalf("测试前提失效：近形 sign %q 已被登记（请另选探针 sign）", probe)
					}
					decision, err := policy.Decide(probe)
					assertDenied(t, decision, err, ErrEgressPolicyMissing)
				}
			})

			t.Run("④CoveredEgressDomains 仍恰为 4 项且不含该 sign", func(t *testing.T) {
				got := CoveredEgressDomains()
				if len(got) != 4 || !reflect.DeepEqual(got, wantAttrFilterSigns) {
					t.Fatalf("已覆盖域必须仍恰为 4 项 %v（D-2/D-6 冻结，%q 属未覆盖域），实际 %v",
						wantAttrFilterSigns, sign, got)
				}
				for _, domain := range got {
					if domain == sign {
						t.Fatalf("%q 不得进入 4 个已覆盖域（D-2/D-6：对外能力清单冻结）", sign)
					}
				}
			})

			t.Run("⑤空 sign 仍拒绝", func(t *testing.T) {
				for _, blank := range []string{"", "   ", "\t\n"} {
					decision, err := policy.Decide(blank)
					assertDenied(t, decision, err, ErrEgressSignBlank)
				}
			})

			t.Run("⑥成为内建项 ⇒ 配置不得改写（纯加法的另一半）", func(t *testing.T) {
				for _, weakened := range []EgressRule{
					{Sign: sign, Mode: EgressModeAttrFilter},
					{Sign: sign, Mode: EgressModeMinimalAllowlist, AllowFields: []string{"imei"}},
				} {
					if _, err := NewEgressPolicy(weakened); !errors.Is(err, ErrEgressRuleBuiltinFrozen) {
						t.Fatalf("%q 是内建项 ⇒ extra 不得降级模式 / 放宽白名单，实际 %v", sign, err)
					}
				}
			})
		})
	}
}

func TestEgressPolicy_DecideReturnsDeclaredMode(t *testing.T) {
	policy, err := NewEgressPolicy()
	if err != nil {
		t.Fatalf("NewEgressPolicy() 不应报错: %v", err)
	}

	for _, sign := range wantAttrFilterSigns {
		decision, err := policy.Decide(sign)
		if err != nil {
			t.Fatalf("%q 已声明，不应被拒: %v", sign, err)
		}
		if decision.Sign != sign || decision.Mode != EgressModeAttrFilter {
			t.Fatalf("%q 判定应为 attr_filter，实际 %+v", sign, decision)
		}
		if len(decision.AllowFields) != 0 {
			t.Fatalf("%q（attr_filter）不得返回白名单：%v", sign, decision.AllowFields)
		}
	}

	for sign, fields := range wantAllowlists {
		decision, err := policy.Decide(sign)
		if err != nil {
			t.Fatalf("%q 已声明，不应被拒: %v", sign, err)
		}
		if decision.Sign != sign || decision.Mode != EgressModeMinimalAllowlist {
			t.Fatalf("%q 判定应为 minimal_allowlist，实际 %+v", sign, decision)
		}
		if !reflect.DeepEqual(sortedCopy(decision.AllowFields), sortedCopy(fields)) {
			t.Fatalf("%q 白名单不符：want=%v got=%v", sign, sortedCopy(fields), sortedCopy(decision.AllowFields))
		}
	}
}

// TestEgressPolicy_UncoveredAllowlistsExcludeNeverSendFields 复用  的黑名单规则做交叉核验：
// 出域白名单里**不得**出现任何「永不外发」字段（否则等于用白名单给敏感字段开出口）。
//
// 这里刻意调用同包的 blacklistHit（ 交付）而不是手写一份敏感词表 —— R3 知识重复 / L-019 冻结 ABI。
func TestEgressPolicy_UncoveredAllowlistsExcludeNeverSendFields(t *testing.T) {
	policy, err := NewEgressPolicy()
	if err != nil {
		t.Fatalf("NewEgressPolicy() 不应报错: %v", err)
	}
	for sign := range wantAllowlists {
		got, err := policy.Decide(sign)
		if err != nil {
			t.Fatalf("%q 应可判定: %v", sign, err)
		}
		for _, field := range got.AllowFields {
			if blacklistHit(field, nil) {
				t.Fatalf("未覆盖域 %q 的白名单字段 %q 命中「永不外发」黑名单（白名单不得给敏感字段开出口）", sign, field)
			}
		}
	}

	// mobile 明写：设备标识一律不出域。
	decision, err := policy.Decide("mobile")
	if err != nil {
		t.Fatalf("mobile 应可判定: %v", err)
	}
	for _, forbidden := range wantDeviceIdentifiers {
		for _, field := range decision.AllowFields {
			if field == forbidden || strings.Contains(field, forbidden) {
				t.Fatalf("mobile 白名单不得含设备标识 %q（设计文档），实际 %v", forbidden, decision.AllowFields)
			}
		}
	}
}

func TestEgressPolicy_DecisionAllowlistIsReadOnlyCopy(t *testing.T) {
	policy, err := NewEgressPolicy()
	if err != nil {
		t.Fatalf("NewEgressPolicy() 不应报错: %v", err)
	}
	first, err := policy.Decide("cert")
	if err != nil {
		t.Fatalf("cert 应可判定: %v", err)
	}
	original := sortedCopy(first.AllowFields)
	first.AllowFields[0] = "imei"

	second, err := policy.Decide("cert")
	if err != nil {
		t.Fatalf("cert 应可判定: %v", err)
	}
	if !reflect.DeepEqual(sortedCopy(second.AllowFields), original) {
		t.Fatalf("Decide 必须返回副本，外部改动不得污染策略表：want=%v got=%v", original, second.AllowFields)
	}
}

// ---------------------------------------------------------------------------
// 配置只能「新增未覆盖域」：不能降级模式、不能改写内建项、不能关闭默认 deny
// ---------------------------------------------------------------------------

func TestEgressPolicy_ExtraRulesCannotWeakenBuiltin(t *testing.T) {
	cases := []struct {
		name string
		rule EgressRule
		want error
	}{
		{
			name: "未覆盖域降级为 attr_filter",
			rule: EgressRule{Sign: "mobile", Mode: EgressModeAttrFilter},
			want: ErrEgressRuleBuiltinFrozen,
		},
		{
			name: "已覆盖域改写成 minimal_allowlist",
			rule: EgressRule{Sign: "ip", Mode: EgressModeMinimalAllowlist, AllowFields: []string{"ip"}},
			want: ErrEgressRuleBuiltinFrozen,
		},
		{
			name: "内建未覆盖域想放宽白名单",
			rule: EgressRule{Sign: "cert", Mode: EgressModeMinimalAllowlist, AllowFields: []string{"imei"}},
			want: ErrEgressRuleBuiltinFrozen,
		},
		{
			name: "新增 sign 用 attr_filter（仅限 4 个已覆盖域）",
			rule: EgressRule{Sign: "mobile_extra", Mode: EgressModeAttrFilter},
			want: ErrEgressRuleInvalid,
		},
		{
			name: "新增 sign 模式非法",
			rule: EgressRule{Sign: "mobile_extra", Mode: EgressMode("")},
			want: ErrEgressRuleInvalid,
		},
		{
			name: "新增 sign 白名单为空",
			rule: EgressRule{Sign: "mobile_extra", Mode: EgressModeMinimalAllowlist},
			want: ErrEgressRuleInvalid,
		},
		{
			name: "新增 sign 白名单全为空白",
			rule: EgressRule{Sign: "mobile_extra", Mode: EgressModeMinimalAllowlist, AllowFields: []string{"", "  "}},
			want: ErrEgressRuleInvalid,
		},
		{
			name: "新增 sign 白名单含永不外发字段",
			rule: EgressRule{Sign: "mobile_extra", Mode: EgressModeMinimalAllowlist, AllowFields: []string{"imei_clientid"}},
			want: ErrEgressRuleInvalid,
		},
		{
			name: "新增 sign 为空",
			rule: EgressRule{Sign: "  ", Mode: EgressModeMinimalAllowlist, AllowFields: []string{"a"}},
			want: ErrEgressRuleInvalid,
		},
		{
			name: "新增 sign 带前后空白（不做归一化）",
			rule: EgressRule{Sign: " mobile_extra ", Mode: EgressModeMinimalAllowlist, AllowFields: []string{"a"}},
			want: ErrEgressRuleInvalid,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			policy, err := NewEgressPolicy(tc.rule)
			if err == nil {
				t.Fatalf("非法登记必须构造即拒（fail-closed），实际成功: %+v", policy)
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("错误应可用 errors.Is 判定为 %v，实际 %v", tc.want, err)
			}
		})
	}
}

func TestEgressPolicy_ExtraRulesAdditiveOnly(t *testing.T) {
	extra := EgressRule{Sign: "mobile_extra", Mode: EgressModeMinimalAllowlist, AllowFields: []string{"app_name", "app_name", " first_seen "}}
	policy, err := NewEgressPolicy(extra)
	if err != nil {
		t.Fatalf("合法新增未覆盖域不应报错: %v", err)
	}

	decision, err := policy.Decide("mobile_extra")
	if err != nil {
		t.Fatalf("新增域应可判定: %v", err)
	}
	if decision.Mode != EgressModeMinimalAllowlist {
		t.Fatalf("新增域模式应为 minimal_allowlist，实际 %q", decision.Mode)
	}
	// 归一化：去空白 + 去重 + 排序
	if want := []string{"app_name", "first_seen"}; !reflect.DeepEqual(decision.AllowFields, want) {
		t.Fatalf("白名单应去空白/去重/排序：want=%v got=%v", want, decision.AllowFields)
	}

	// 内建项一条都没被改动
	if !reflect.DeepEqual(CoveredEgressDomains(), wantAttrFilterSigns) {
		t.Fatalf("新增登记不得改动已覆盖域集合: %v", CoveredEgressDomains())
	}
	if _, err := policy.Decide("mobile"); err != nil {
		t.Fatalf("内建未覆盖域应保持可用: %v", err)
	}
	if _, err := policy.Decide("unknown_domain"); !errors.Is(err, ErrEgressPolicyMissing) {
		t.Fatalf("未声明 sign 仍须默认 deny，实际 %v", err)
	}

	// 重复登记同一 sign 属歧义 ⇒ 拒绝
	if _, err := NewEgressPolicy(extra, extra); !errors.Is(err, ErrEgressRuleInvalid) {
		t.Fatalf("重复登记同一 sign 应被拒，实际 %v", err)
	}
}

func TestEgressPolicy_ConcurrentDecide(t *testing.T) {
	policy, err := NewEgressPolicy(EgressRule{Sign: "mobile_extra", Mode: EgressModeMinimalAllowlist, AllowFields: []string{"app_name"}})
	if err != nil {
		t.Fatalf("NewEgressPolicy() 不应报错: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			signs := []string{"ip", "mobile", "mobile_extra", "unknown", ""}
			for _, sign := range signs {
				decision, err := policy.Decide(sign)
				if sign == "unknown" || sign == "" {
					if err == nil {
						t.Errorf("并发下未声明 sign %q 也必须被拒", sign)
					}
					continue
				}
				if err != nil {
					t.Errorf("并发下已声明 sign %q 不应被拒: %v", sign, err)
				}
				_ = decision.AllowFields
			}
		}(i)
	}
	wg.Wait()
}

// ---------------------------------------------------------------------------
// 硬安全点 ②：审计证据只落「可交叉核验的摘要」，绝不落原文
// ---------------------------------------------------------------------------

func auditSnapshot(a EgressAudit) string {
	raw, err := json.Marshal(map[string]any{
		"policy_sign":    a.PolicySign(),
		"sent_bytes":     a.SentBytes(),
		"sent_hmac":      a.SentHMAC(),
		"masker_applied": a.MaskerApplied(),
		"fence_hits":     a.FenceHits(),
	})
	if err != nil {
		panic(err)
	}
	return string(raw)
}

func TestEgressPolicy_AuditEvidenceCarriesNoRawText(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef") // 32 字节，≥ 128bit
	payload := []byte(`{"imei":"861234567890123","token":"sk-live-abcdef","host":"internal.internal.example","ip":"1.2.3.4"}`)
	rawSecrets := []string{"861234567890123", "sk-live-abcdef", "internal.internal.example", "1.2.3.4", `"imei"`, `"token"`}
	// 字段路径名本身也会泄露语义⇒ 只落哈希。
	fieldPaths := []string{"imei", "imei_clientid", "$.token"}

	t.Run("结构面：字段集合与类型都装不下原文", func(t *testing.T) {
		typ := reflect.TypeOf(EgressAudit{})
		wantFields := []struct {
			name string
			kind reflect.Kind
		}{
			{"policySign", reflect.String},
			{"sentBytes", reflect.Int},
			{"sentHMAC", reflect.String},
			{"maskerApplied", reflect.Slice},
			{"fenceHits", reflect.Int},
		}
		if typ.NumField() != len(wantFields) {
			t.Fatalf("EgressAudit 必须恰有 %d 个字段（新增字段等于新增泄露面，须显式评审），实际 %d",
				len(wantFields), typ.NumField())
		}
		for i, want := range wantFields {
			field := typ.Field(i)
			if field.Name != want.name {
				t.Fatalf("第 %d 个字段应为 %q，实际 %q", i, want.name, field.Name)
			}
			if field.Type.Kind() != want.kind {
				t.Fatalf("字段 %q 类型应为 %v，实际 %v", want.name, want.kind, field.Type.Kind())
			}
			// any / []byte / map / 指针 都是「什么都能装」的字段类型 ⇒ 不得出现
			switch field.Type.Kind() {
			case reflect.Interface, reflect.Map, reflect.Ptr, reflect.Uint8:
				t.Fatalf("字段 %q 的类型 %v 可承载任意原文，违反「审计不落原文」", want.name, field.Type)
			}
		}
		if typ.Field(3).Type.Elem().Kind() != reflect.String {
			t.Fatalf("maskerApplied 必须是 []string，实际 %v", typ.Field(3).Type)
		}
	})

	t.Run("功能面：原文与字段路径明文都不出现在证据里", func(t *testing.T) {
		evidence, err := NewEgressEvidence("ip", payload, key, []string{"ip"}, fieldPaths, 3)
		if err != nil {
			t.Fatalf("合法输入不应报错: %v", err)
		}

		snapshot := auditSnapshot(evidence)
		for _, secret := range rawSecrets {
			if strings.Contains(snapshot, secret) {
				t.Fatalf("审计证据出现了原文/字段明文 %q —— 不落原文被破坏：%s", secret, snapshot)
			}
		}
		for _, path := range fieldPaths {
			if strings.Contains(snapshot, `"`+path+`"`) {
				t.Fatalf("审计证据出现字段路径明文 %q（只允许字段名哈希）：%s", path, snapshot)
			}
		}
		// 规则 sign 按  保留明文（非敏感），字段路径一律哈希
		applied := evidence.MaskerApplied()
		if len(applied) != 1+len(fieldPaths) {
			t.Fatalf("masker_applied 应有 1 条规则 + %d 条字段名哈希，实际 %v", len(fieldPaths), applied)
		}
		if !containsString(applied, "ip") {
			t.Fatalf("规则 sign 应保留明文，实际 %v", applied)
		}
		for _, path := range fieldPaths {
			for _, entry := range applied {
				if entry == path {
					t.Fatalf("字段路径 %q 不得以明文进入证据：%v", path, applied)
				}
			}
		}
		if !evidence.Verifiable() {
			t.Fatal("由构造器产出的证据必须是可核验的")
		}
	})

	t.Run("交叉核验：sent_hmac 可被持密钥方独立重算", func(t *testing.T) {
		evidence, err := NewEgressEvidence("ip", payload, key, nil, nil, 5)
		if err != nil {
			t.Fatalf("合法输入不应报错: %v", err)
		}
		mac := hmac.New(sha256.New, key)
		mac.Write(payload)
		if want := hex.EncodeToString(mac.Sum(nil)); evidence.SentHMAC() != want {
			t.Fatalf("sent_hmac 必须等于 HMAC-SHA256(key, payload)：want=%s got=%s", want, evidence.SentHMAC())
		}
		if evidence.SentBytes() != len(payload) {
			t.Fatalf("sent_bytes 应为 %d，实际 %d", len(payload), evidence.SentBytes())
		}
		if evidence.PolicySign() != "ip" || evidence.FenceHits() != 5 {
			t.Fatalf("policy_sign / fence_hits 不符：%s", auditSnapshot(evidence))
		}

		// 换密钥 ⇒ 证据不同（证明 HMAC 真的带服务端密钥）
		other, err := NewEgressEvidence("ip", payload, []byte("ffffffffffffffffffffffffffffffff"), nil, nil, 5)
		if err != nil {
			t.Fatalf("合法输入不应报错: %v", err)
		}
		if other.SentHMAC() == evidence.SentHMAC() {
			t.Fatal("不同密钥必须产生不同 sent_hmac（否则密钥未生效）")
		}
	})

	t.Run("无法核验的证据与非法输入一律拒绝", func(t *testing.T) {
		var zero EgressAudit
		if zero.Verifiable() {
			t.Fatal("零值证据不得被判为可核验（否则等于伪造一次成功审计）")
		}

		for _, tc := range []struct {
			name string
			key  []byte
		}{
			{"密钥缺失 nil", nil},
			{"密钥为空", []byte{}},
			{"密钥过短 8 字节", []byte("12345678")},
			{"密钥过短 15 字节", []byte("123456789012345")},
		} {
			if _, err := NewEgressEvidence("ip", payload, tc.key, nil, nil, 0); !errors.Is(err, ErrEgressHMACKeyInvalid) {
				t.Fatalf("%s ⇒ 必须判失败（密钥缺失不做「跳过审计」），实际 %v", tc.name, err)
			}
		}

		for _, tc := range []struct {
			name       string
			policySign string
			ruleSigns  []string
			fenceHits  int
		}{
			{"policy_sign 为空", "  ", []string{"ip"}, 0},
			{"fence_hits 为负", "ip", []string{"ip"}, -1},
			{"规则项是数字串（原文伪装成规则）", "ip", []string{"861234567890123"}, 0},
			{"规则项含空白", "ip", []string{"sk live secret"}, 0},
			{"规则项首字符非字母/下划线", "ip", []string{"-abc"}, 0},
			{"规则项超长", "ip", []string{strings.Repeat("a", 65)}, 0},
		} {
			if _, err := NewEgressEvidence(tc.policySign, payload, key, tc.ruleSigns, nil, tc.fenceHits); !errors.Is(err, ErrEgressEvidenceInvalid) {
				t.Fatalf("%s ⇒ 必须判为不可判定并拒绝，实际 %v", tc.name, err)
			}
		}
	})
}

// fakeAuditor 是 Auditor 的测试替身：只收证据、只落摘要，验证「实现方拿不到原文」。
type fakeAuditor struct {
	mu      sync.Mutex
	records []EgressAudit
	err     error
}

func (f *fakeAuditor) Record(ctx context.Context, audit EgressAudit) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.records = append(f.records, audit)
	return f.err
}

var _ Auditor = (*fakeAuditor)(nil)

func TestEgressPolicy_AuditorInterfaceCarriesOnlyEvidence(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	payload := []byte(`{"imei":"861234567890123"}`)

	evidence, err := NewEgressEvidence("ip", payload, key, []string{"ip"}, []string{"imei"}, 1)
	if err != nil {
		t.Fatalf("合法输入不应报错: %v", err)
	}

	auditor := &fakeAuditor{}
	if err := auditor.Record(context.Background(), evidence); err != nil {
		t.Fatalf("Record 不应报错: %v", err)
	}
	if len(auditor.records) != 1 {
		t.Fatalf("应落 1 条记录，实际 %d", len(auditor.records))
	}
	if snapshot := auditSnapshot(auditor.records[0]); strings.Contains(snapshot, "861234567890123") {
		t.Fatalf("审计实现收到的记录里出现了原文：%s", snapshot)
	}

	// 审计失败必须**可被调用方感知**（出域职责链「审计」步在「调用」之前：失败即不出域）。
	//
	// 修前缺陷（REVIEW-2-quality.md 节B.4.1 F-05）：原文在这里设 `auditor.err` 再调
	// `auditor.Record(...)`，而 `fakeAuditor.Record` 的实现就是 `return f.err` ⇒ 断言打在
	// **测试替身**上、必然非 nil；生产「审计失败 ⇒ 不出域」的职责链**一行未走**。
	//
	// 本层（证据构造）能拥有的性质是：密钥缺失/过短、`policy_sign` 为空、`fence_hits` 为负
	// 一律**返回 error**（否则「审计」步会带着一份不可核验的假证据继续出域）。
	// 逐条用生产 `NewEgressEvidence` 判定：期望哨兵是独立事实，不由被测值派生。
	for _, bad := range []struct {
		name      string
		sign      string
		badKey    []byte
		fenceHits int
		wantErr   error
	}{
		{name: "密钥缺失", sign: "ip", badKey: nil, fenceHits: 1, wantErr: ErrEgressHMACKeyInvalid},
		{name: "密钥过短(<128bit)", sign: "ip", badKey: []byte("short"), fenceHits: 1, wantErr: ErrEgressHMACKeyInvalid},
		{name: "policy_sign 为空", sign: "", badKey: key, fenceHits: 1, wantErr: ErrEgressEvidenceInvalid},
		{name: "fence_hits 为负", sign: "ip", badKey: key, fenceHits: -1, wantErr: ErrEgressEvidenceInvalid},
	} {
		failed, err := NewEgressEvidence(bad.sign, payload, bad.badKey, []string{"ip"}, []string{"imei"}, bad.fenceHits)
		if err == nil {
			t.Fatalf("%s：审计证据构造必须判失败（否则审计步会带着假证据继续出域）", bad.name)
		}
		if !errors.Is(err, bad.wantErr) {
			t.Fatalf("%s：期望哨兵 %v，实际 %v", bad.name, bad.wantErr, err)
		}
		if failed.Verifiable() {
			t.Fatalf("%s：失败路径不得产出「看起来可核验」的证据", bad.name)
		}
	}

	// 职责链上的落点（**生产装配**，不是测试替身）：
	// `既有模型层/t12_trusted_zone_e2e_test.go` 的 `TestEgress_AuditFailureAbortsBeforeEgress`
	// —— 注入会失败的 `EgressAuditor` ⇒ `Outlet.Run` 中止且 **零出网**，另以合法审计装配件做交叉对照。
}

func containsString(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// 守卫：默认 deny 的结构性固化（行为 + 内建表 + 导出面 AST 扫描）
// ---------------------------------------------------------------------------

// egressForbiddenExportWords 是「默认放行 / 旁路 / 可关闭」语义词。
// 策略表**不得**导出任何带这些词的符号 —— 一旦存在，就等于给默认 deny 留了一个关闭开关。
var egressForbiddenExportWords = []string{
	"bypass", "allowall", "allowany", "allowdefault", "defaultallow", "bydefault", "permitall",
	"skip", "optional", "disable", "enable", "remove", "replace", "clear", "reset", "unrestricted",
}

func TestGuard_EgressPolicyDefaultDeny(t *testing.T) {
	t.Run("默认 deny 三态", func(t *testing.T) {
		policy, err := NewEgressPolicy()
		if err != nil {
			t.Fatalf("NewEgressPolicy() 不应报错: %v", err)
		}
		var nilPolicy *EgressPolicy
		var zero EgressPolicy
		for _, probe := range []struct {
			name   string
			policy *EgressPolicy
			sign   string
			want   error
		}{
			{"未声明 sign", policy, "never_declared", ErrEgressPolicyMissing},
			{"空 sign", policy, "", ErrEgressSignBlank},
			{"策略表 nil", nilPolicy, "ip", ErrEgressPolicyNotInjected},
			{"策略表零值", &zero, "ip", ErrEgressPolicyNotInjected},
		} {
			decision, err := probe.policy.Decide(probe.sign)
			if err == nil {
				t.Fatalf("%s：必须拒绝（默认 deny），实际放行 %+v", probe.name, decision)
			}
			if !errors.Is(err, probe.want) {
				t.Fatalf("%s：原因应为 %v，实际 %v", probe.name, probe.want, err)
			}
		}
	})

	t.Run("内建表：4 域 attr_filter + N 域 minimal_allowlist", func(t *testing.T) {
		rules := BuiltinEgressRules()
		var attrFilter, allowlist []string
		for _, rule := range rules {
			switch rule.Mode {
			case EgressModeAttrFilter:
				attrFilter = append(attrFilter, rule.Sign)
			case EgressModeMinimalAllowlist:
				if len(rule.AllowFields) == 0 {
					t.Fatalf("%q 是 minimal_allowlist 却未列举字段", rule.Sign)
				}
				allowlist = append(allowlist, rule.Sign)
			default:
				t.Fatalf("%q 模式非法：%q", rule.Sign, rule.Mode)
			}
			for _, field := range rule.AllowFields {
				if blacklistHit(field, nil) {
					t.Fatalf("%q 的白名单字段 %q 命中永不外发黑名单", rule.Sign, field)
				}
			}
		}
		sort.Strings(attrFilter)
		sort.Strings(allowlist)
		if !reflect.DeepEqual(attrFilter, wantAttrFilterSigns) {
			t.Fatalf("attr_filter 只允许 4 个已覆盖域：want=%v got=%v", wantAttrFilterSigns, attrFilter)
		}
		wantSigns := make([]string, 0, len(wantAllowlists))
		for sign := range wantAllowlists {
			wantSigns = append(wantSigns, sign)
		}
		sort.Strings(wantSigns)
		if !reflect.DeepEqual(allowlist, wantSigns) {
			t.Fatalf("未覆盖域必须写死 minimal_allowlist：want=%v got=%v", wantSigns, allowlist)
		}
		// 未覆盖域**不得**使用 attr_filter 模式（D-24）
		for sign := range wantAllowlists {
			if _, err := NewEgressPolicy(EgressRule{Sign: sign, Mode: EgressModeAttrFilter}); !errors.Is(err, ErrEgressRuleBuiltinFrozen) {
				t.Fatalf("未覆盖域 %q 尝试 attr_filter 必须被拒，实际 %v", sign, err)
			}
		}
	})

	t.Run("导出面 AST 扫描：无「默认放行 / 旁路」入口", func(t *testing.T) {
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, "egress_policy.go", nil, 0)
		if err != nil {
			t.Fatalf("解析 egress_policy.go 失败: %v", err)
		}
		var exported []string
		for _, decl := range file.Decls {
			switch node := decl.(type) {
			case *ast.FuncDecl:
				if node.Recv == nil {
					if node.Name.IsExported() {
						exported = append(exported, node.Name.Name)
					}
					continue
				}
				if node.Name.IsExported() {
					exported = append(exported, node.Name.Name)
				}
			case *ast.GenDecl:
				for _, spec := range node.Specs {
					switch s := spec.(type) {
					case *ast.TypeSpec:
						if s.Name.IsExported() {
							exported = append(exported, s.Name.Name)
						}
					case *ast.ValueSpec:
						for _, name := range s.Names {
							if name.IsExported() {
								exported = append(exported, name.Name)
							}
						}
					}
				}
			}
		}
		if len(exported) == 0 {
			t.Fatal("AST 扫描未取到任何导出符号，守卫失效")
		}
		lowered := make([]string, 0, len(exported))
		for _, name := range exported {
			lowered = append(lowered, strings.ToLower(name))
		}
		for _, word := range egressForbiddenExportWords {
			for _, name := range lowered {
				if strings.Contains(name, word) {
					t.Fatalf("导出符号 %q 含「默认放行/旁路/可关闭」语义词 %q —— 出域策略不得存在开关入口",
						name, word)
				}
			}
		}
	})
}
