package authz

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"agentkit/kitctx"
)

// 注册表相关的可识别错误。
var (
	// ErrPermissionKeyRequired 表示权限点没有键（无法被引用，也就无法被校验）。
	ErrPermissionKeyRequired = errors.New("authz: 权限点 Key 不能为空")
	// ErrPermissionLabelRequired 表示权限点没有可读标签。
	ErrPermissionLabelRequired = errors.New("authz: 权限点 Label 不能为空")
	// ErrPermissionDuplicate 表示键重复：两份同名声明会让"这条权限是谁给的"不可判定。
	ErrPermissionDuplicate = errors.New("authz: 权限点 Key 重复")
	// ErrDefaultRolesRequired 表示默认角色为空（见本包 doc 第 3 条：这会与 allows 的"空=公开"接上，成 fail-open）。
	ErrDefaultRolesRequired = errors.New("authz: 权限点必须声明至少一个默认角色（公开接口请不挂权限点）")
	// ErrUnknownPermission 表示引用的权限点不存在。**必须报错而不是返回空角色**：
	// 空角色在授权谓词里等于"谁都能用"，把键名写错变成放开权限，是不可接受的失效方向。
	ErrUnknownPermission = errors.New("authz: 未知权限点")
)

// PermissionPoint 是一条权限声明。字段即文档（沿用日志微服务注册表的取舍：
// 权限表的价值一半在运行时查得到，一半在代码里读得懂）。
type PermissionPoint struct {
	// Key 是权限点标识，注册表内唯一，供接口/工具引用。
	Key string
	// Label 是人读名称（管理面展示用；可见性由宿主执行"仅 admin"口径）。
	Label string
	// Scope 是作用域，取值闭集见 Scope 常量。
	Scope Scope
	// DefaultRoles 是该权限点默认授予的角色，必须非空且全部落在注入闭集内。
	DefaultRoles []kitctx.Role
	// Enforced 说明这条权限由谁执行（直接执行 / 汇聚到某个执行口）。
	Enforced Enforcement
	// Note 是补充说明（历史包袱、例外、为什么这样授权）。
	Note string
}

// Registry 是权限点注册表：有序切片 + 一次性建索引 + 单一校验入口。
//
// 与 `tools.Registry` 的分工：本表只管"权限点 ⇒ 角色集合"的声明与校验；
// 裁剪与执行授权仍只有 `tools` 那一个谓词。两者之间由 `RequiredRolesFor` 这一条转换口衔接（P5）。
type Registry struct {
	roles  *RoleSet
	points []PermissionPoint
	byKey  map[string]PermissionPoint
}

// NewRegistry 构造注册表。roles 必须非 nil（忘了注入闭集 ⇒ 直接失败，不给你一张"看起来能用"的空表）。
// 列表逐个走 Register，因此与手工注册的校验完全同一条路径（不存在"批量走宽松、单个走严格"两套口径）。
func NewRegistry(roles *RoleSet, list ...PermissionPoint) (*Registry, error) {
	if roles == nil {
		return nil, fmt.Errorf("%w：必须注入角色闭集（框架不猜宿主的角色词汇表）", ErrRoleSetEmpty)
	}
	r := &Registry{roles: roles, byKey: make(map[string]PermissionPoint, len(list))}
	for _, p := range list {
		if err := r.Register(p); err != nil {
			return nil, err
		}
	}
	return r, nil
}

// Register 校验并登记一条权限声明。任何一条不合法 ⇒ 整条被拒，注册表不变。
func (r *Registry) Register(p PermissionPoint) error {
	if r == nil {
		return errors.New("authz: 注册表为 nil")
	}
	if err := r.validate(p); err != nil {
		return err
	}
	if r.byKey == nil {
		r.byKey = make(map[string]PermissionPoint)
	}
	key := strings.TrimSpace(p.Key)
	if _, dup := r.byKey[key]; dup {
		return fmt.Errorf("%w：%q", ErrPermissionDuplicate, key)
	}
	p.DefaultRoles = append([]kitctx.Role(nil), p.DefaultRoles...) // 拷贝：调用方之后改切片不得影响本表
	r.byKey[key] = p
	r.points = append(r.points, p)
	return nil
}

// validate 是唯一的校验入口（闭集校验集中一处，别处不再判第二遍）。
func (r *Registry) validate(p PermissionPoint) error {
	if strings.TrimSpace(p.Key) == "" {
		return ErrPermissionKeyRequired
	}
	if strings.TrimSpace(p.Label) == "" {
		return fmt.Errorf("%w：%q 缺可读标签", ErrPermissionLabelRequired, p.Key)
	}
	if !p.Scope.Valid() {
		return fmt.Errorf("%w：%q 的 Scope=%q（闭集 = %q|%q）",
			ErrScopeUnknown, p.Key, string(p.Scope), ScopePlatform, ScopeProject)
	}
	if !p.Enforced.Valid() {
		return fmt.Errorf("%w：%q 的 Enforced=%+v", ErrEnforcementUnknown, p.Key, p.Enforced)
	}
	if len(p.DefaultRoles) == 0 {
		return fmt.Errorf("%w：%q", ErrDefaultRolesRequired, p.Key)
	}
	if err := r.roles.validateRoles(p.DefaultRoles); err != nil {
		return fmt.Errorf("%w：%q 的 DefaultRoles：%v", ErrRoleOutsideSet, p.Key, err)
	}
	return nil
}

// All 返回全部权限点（**注册顺序**，副本）。
func (r *Registry) All() []PermissionPoint {
	if r == nil {
		return nil
	}
	out := make([]PermissionPoint, 0, len(r.points))
	for _, p := range r.points {
		q := p
		q.DefaultRoles = append([]kitctx.Role(nil), p.DefaultRoles...)
		out = append(out, q)
	}
	return out
}

// Keys 返回全部键（注册顺序）。
func (r *Registry) Keys() []string {
	if r == nil {
		return nil
	}
	out := make([]string, 0, len(r.points))
	for _, p := range r.points {
		out = append(out, p.Key)
	}
	return out
}

// SortedKeys 返回字典序键列表（内省/快照比对用；顺序敏感的断言一律用它，避免把注册顺序当语义）。
func (r *Registry) SortedKeys() []string {
	out := append([]string(nil), r.Keys()...)
	sort.Strings(out)
	return out
}

// Lookup 按键查权限点。第二个返回值表示是否存在（**不**用它判授权 —— 见 doc 第 1 条）。
func (r *Registry) Lookup(key string) (PermissionPoint, bool) {
	if r == nil {
		return PermissionPoint{}, false
	}
	p, ok := r.byKey[strings.TrimSpace(key)]
	return p, ok
}

// Known 报告键是否存在。
func (r *Registry) Known(key string) bool {
	_, ok := r.Lookup(key)
	return ok
}

// RolesFor 返回权限点的角色集合（副本）。未知键 ⇒ **报错**，不返回空切片。
func (r *Registry) RolesFor(key string) ([]kitctx.Role, error) {
	p, ok := r.Lookup(key)
	if !ok {
		return nil, fmt.Errorf("%w：%q", ErrUnknownPermission, key)
	}
	return append([]kitctx.Role(nil), p.DefaultRoles...), nil
}

// RequiredRolesFor 把权限点角色转成 `tools.Tool.RequiredRoles` 需要的字符串切片（P5 的唯一衔接口）。
//
// 未知键 ⇒ 报错且返回 nil：调用方拿到 error 必须**拒绝生成该工具声明**，
// 而不是"给个空 RequiredRoles 继续跑" —— 后者等于把笔误变成放开权限。
func (r *Registry) RequiredRolesFor(key string) ([]string, error) {
	roles, err := r.RolesFor(key)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(roles))
	for _, role := range roles {
		out = append(out, string(role))
	}
	return out, nil
}

// Roles 返回注入的角色闭集（不是副本：闭集本身按构造只读）。
func (r *Registry) Roles() *RoleSet {
	if r == nil {
		return nil
	}
	return r.roles
}

// Len 返回权限点数量（nil 安全）。
func (r *Registry) Len() int {
	if r == nil {
		return 0
	}
	return len(r.points)
}
