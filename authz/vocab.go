package authz

import (
	"errors"
	"fmt"
	"strings"

	"github.com/malizhi2202-dev/agentkit/kitctx"
)

// 值域相关的可识别错误（都以 %w 包装，调用方可 errors.Is 判定）。
var (
	// ErrRoleSetEmpty 表示注入的闭集为空：没有角色表时任何权限点都无法校验，必须显式失败。
	ErrRoleSetEmpty = errors.New("authz: 角色闭集为空")
	// ErrRoleBlank 表示闭集里出现空白值。
	ErrRoleBlank = errors.New("authz: 角色值不能为空")
	// ErrRoleDuplicate 表示闭集里出现重复值（重复会让"顺序即最长优先"的宿主约定失效）。
	ErrRoleDuplicate = errors.New("authz: 角色闭集内有重复值")
	// ErrRoleOutsideSet 表示权限点声明的角色不在注入闭集内。
	ErrRoleOutsideSet = errors.New("authz: 角色不在闭集内")
	// ErrScopeUnknown 表示 Scope 取值不在闭集内。
	ErrScopeUnknown = errors.New("authz: Scope 取值不在闭集内")
	// ErrEnforcementUnknown 表示 Enforced 写法不符合文法。
	ErrEnforcementUnknown = errors.New("authz: Enforced 写法不合法")
)

// RoleSet 是**注入**的角色值域闭集。
//
// 为什么是注入而不是框架内写死：框架要在多个部署里复用，角色词汇表属于宿主
// （宿主侧 用 既有实现目录  的 v1/v2/v2-1/…/admin，别的应用未必是这套）。
// 但"必须落在这套闭集内"由框架强制 —— 注入的是**值域**，不是**校验规则**。
//
// 语义要点：
//   - 顺序保留（宿主可按"最长优先"排序，与引擎链路里 permissionRoles 的同款纪律一致）；
//   - **nil 接收者一律判"不认识"**（fail-closed：忘了注入闭集 ⇒ 什么都不给，而不是什么都给）；
//   - 比较是精确相等，不做大小写/空白归一（构造期已拒绝带空白的值，声明期同样拒绝）。
type RoleSet struct {
	values []kitctx.Role
	index  map[kitctx.Role]bool
}

// NewRoleSet 用宿主给定的角色构造闭集。空集、空白值、重复值都直接判失败。
func NewRoleSet(roles ...kitctx.Role) (*RoleSet, error) {
	if len(roles) == 0 {
		return nil, ErrRoleSetEmpty
	}
	s := &RoleSet{values: make([]kitctx.Role, 0, len(roles)), index: make(map[kitctx.Role]bool, len(roles))}
	for _, r := range roles {
		if strings.TrimSpace(string(r)) == "" {
			return nil, fmt.Errorf("%w：%d 号位为空", ErrRoleBlank, len(s.values))
		}
		if string(r) != strings.TrimSpace(string(r)) {
			return nil, fmt.Errorf("%w：%q 含首尾空白（会造成比较与展示不一致）", ErrRoleBlank, r)
		}
		if s.index[r] {
			return nil, fmt.Errorf("%w：%q", ErrRoleDuplicate, r)
		}
		s.values = append(s.values, r)
		s.index[r] = true
	}
	return s, nil
}

// Known 报告角色是否在闭集内。nil 接收者返回 false。
func (s *RoleSet) Known(role kitctx.Role) bool {
	if s == nil {
		return false
	}
	return s.index[role]
}

// All 返回闭集（**注入顺序**的副本，调用方改动不影响本表）。
func (s *RoleSet) All() []kitctx.Role {
	if s == nil {
		return nil
	}
	return append([]kitctx.Role(nil), s.values...)
}

// Len 返回闭集大小（nil 安全）。
func (s *RoleSet) Len() int {
	if s == nil {
		return 0
	}
	return len(s.values)
}

// validateRoles 检查一组角色是否全部落在闭集内，并逐项点名越界值（不静默剔除）。
func (s *RoleSet) validateRoles(roles []kitctx.Role) error {
	for _, r := range roles {
		if !s.Known(r) {
			return fmt.Errorf("%w：%q（闭集内共 %d 个取值）", ErrRoleOutsideSet, r, s.Len())
		}
	}
	return nil
}

// Scope 是权限点的作用域闭集（沿用日志微服务权限注册表的口径）。
type Scope string

// Scope 的两个合法取值。新增取值必须同时进 Valid 的枚举与测试的闭集断言，
// 否则 TestVocab_ScopeClosedSet 会红（闭集不靠注释守着）。
const (
	// ScopePlatform 是平台级权限（跨项目生效）。
	ScopePlatform Scope = "platform"
	// ScopeProject 是项目级权限（只在所属项目内生效）。
	ScopeProject Scope = "project"
)

// Valid 报告取值是否落在闭集内。
func (s Scope) Valid() bool {
	switch s {
	case ScopePlatform, ScopeProject:
		return true
	default:
		return false
	}
}

// EnforcementKind 是"这条权限到底有没有被执行"的类别闭集。
//
// 这条来自日志微服务注册表的 Enforced 字段：权限点写了不等于生效，
// 必须说清是**自己直接执行**还是**汇聚到某处执行**，否则"已管控"就是假的。
type EnforcementKind string

const (
	// EnforcementDirect 表示该权限点由其自身代码路径直接执行。
	EnforcementDirect EnforcementKind = "direct"
	// EnforcementFunnel 表示该权限点汇聚到 Via 指定的执行口。
	EnforcementFunnel EnforcementKind = "funnel"
)

// EnforcementViaPrefix 是文本形式里 funnel 汇聚键的分隔前缀（"funnel:<key>"）。
const EnforcementViaPrefix = "funnel:"

// Enforcement 是 Enforced 字段解析后的结构。
type Enforcement struct {
	Kind EnforcementKind
	// Via 是汇聚执行口的键；仅 EnforcementFunnel 使用，direct 时必须为空。
	Via string
}

// DirectEnforcement 构造"自身直接执行"。
func DirectEnforcement() Enforcement { return Enforcement{Kind: EnforcementDirect} }

// FunnelEnforcement 构造"汇聚到 via 执行"。via 空白直接判失败（构造不出非法值）。
func FunnelEnforcement(via string) (Enforcement, error) {
	if strings.TrimSpace(via) == "" {
		return Enforcement{}, fmt.Errorf("%w：funnel 缺少汇聚键", ErrEnforcementUnknown)
	}
	return Enforcement{Kind: EnforcementFunnel, Via: strings.TrimSpace(via)}, nil
}

// ParseEnforcement 解析文本形式（"direct" / "funnel:<key>"）。
//
// 提供文本形式是为了让注册表能像日志微服务那样**用字面量书写**（代码即文档），
// 但解析结果仍是结构体：闭集判定发生在这一处，别处不再解释这个文法。
func ParseEnforcement(raw string) (Enforcement, error) {
	switch v := strings.TrimSpace(raw); {
	case v == string(EnforcementDirect):
		return DirectEnforcement(), nil
	case strings.HasPrefix(v, EnforcementViaPrefix):
		return FunnelEnforcement(strings.TrimPrefix(v, EnforcementViaPrefix))
	default:
		return Enforcement{}, fmt.Errorf("%w：%q（只接受 %q 或 %q<key>）",
			ErrEnforcementUnknown, raw, EnforcementDirect, EnforcementViaPrefix)
	}
}

// Valid 报告结构是否落在类别闭集内且 Via 与类别自洽。
func (e Enforcement) Valid() bool {
	switch e.Kind {
	case EnforcementDirect:
		return e.Via == ""
	case EnforcementFunnel:
		return strings.TrimSpace(e.Via) != ""
	default:
		return false
	}
}

// String 回写为同形的文本形式（与 ParseEnforcement 构成往返，测试钉住）。
func (e Enforcement) String() string {
	if e.Kind == EnforcementFunnel {
		return EnforcementViaPrefix + e.Via
	}
	return string(e.Kind)
}
