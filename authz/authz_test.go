package authz

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"agentkit/kitctx"
)

func mustRoles(t *testing.T, roles ...kitctx.Role) *RoleSet {
	t.Helper()
	s, err := NewRoleSet(roles...)
	if err != nil {
		t.Fatalf("NewRoleSet(%v) 失败：%v", roles, err)
	}
	return s
}

func point(key string, roles ...kitctx.Role) PermissionPoint {
	return PermissionPoint{
		Key:          key,
		Label:        key + " 的可读名",
		Scope:        ScopePlatform,
		DefaultRoles: roles,
		Enforced:     DirectEnforcement(),
	}
}

// ---- 角色闭集：注入的是值域，不是校验规则 ----

func TestVocab_RoleSetRejectsBadInput(t *testing.T) {
	cases := []struct {
		name  string
		roles []kitctx.Role
		is    error
	}{
		{"空闭集必须显式失败", nil, ErrRoleSetEmpty},
		{"空白值", []kitctx.Role{"support", "  "}, ErrRoleBlank},
		{"带首尾空白的值", []kitctx.Role{" support "}, ErrRoleBlank},
		{"重复值（破坏最长优先约定）", []kitctx.Role{"v2-1", "v2-1"}, ErrRoleDuplicate},
		{"合法集合", []kitctx.Role{"v2-1", "auditor", "admin"}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, err := NewRoleSet(c.roles...)
			if c.is == nil {
				if err != nil {
					t.Fatalf("合法闭集不该失败：%v", err)
				}
				return
			}
			if !errors.Is(err, c.is) {
				t.Fatalf("期望 %v，实得 %v", c.is, err)
			}
			if s != nil {
				t.Fatal("失败时不得返回半构造的闭集")
			}
		})
	}
}

func TestVocab_RoleSetNilIsFailClosed(t *testing.T) {
	var s *RoleSet // 忘了注入
	if s.Known("admin") || s.Known("") {
		t.Fatal("nil 闭集必须什么都不认识（fail-closed）")
	}
	if s.All() != nil || s.Len() != 0 {
		t.Fatal("nil 闭集的 All/Len 必须是空值")
	}
	if err := s.validateRoles([]kitctx.Role{"admin"}); !errors.Is(err, ErrRoleOutsideSet) {
		t.Fatalf("nil 闭集校验角色应判越界，实得 %v", err)
	}
}

func TestVocab_RoleSetKeepsInjectionOrder(t *testing.T) {
	s := mustRoles(t, "v2-1", "auditor", "support-2", "admin", "v2") // 宿主按最长优先排
	if got := s.All(); !reflect.DeepEqual([]kitctx.Role(got), []kitctx.Role{"v2-1", "auditor", "support-2", "admin", "v2"}) {
		t.Fatalf("注入顺序必须保留（最长优先是宿主的语义）：%v", got)
	}
	all := s.All()
	all[0] = "篡改" // 改副本不得影响表
	if s.All()[0] != "v2-1" {
		t.Fatal("All() 必须是副本语义")
	}
	if !s.Known("v2-1") || s.Known("v2-2") || s.Known("V2-1") {
		t.Fatal("Known 判定不符合闭集（大小写敏感、未知即否）")
	}
}

// ---- Scope / Enforced 闭集 ----

func TestVocab_ScopeClosedSet(t *testing.T) {
	valid := []Scope{ScopePlatform, ScopeProject}
	for _, s := range valid {
		if !s.Valid() {
			t.Errorf("合法 Scope %q 被判非法", s)
		}
	}
	for _, bad := range []Scope{"", "Platform", "project ", "global", "tenant", "user"} {
		if bad.Valid() {
			t.Errorf("闭集外 Scope %q 被判合法 ⇒ 闭集形同虚设", bad)
		}
	}
}

func TestVocab_EnforcementGrammar(t *testing.T) {
	cases := []struct {
		raw     string
		wantErr bool
		want    Enforcement
	}{
		{"direct", false, Enforcement{Kind: EnforcementDirect}},
		{"  direct  ", false, Enforcement{Kind: EnforcementDirect}},
		{"funnel:engine_hook", false, Enforcement{Kind: EnforcementFunnel, Via: "engine_hook"}},
		{"funnel:  spaced  ", false, Enforcement{Kind: EnforcementFunnel, Via: "spaced"}},
		{"funnel:", true, Enforcement{}},
		{"funnel:   ", true, Enforcement{}},
		{"funnel", true, Enforcement{}},   // 少了分隔符不算 funnel
		{"FUNNEL:x", true, Enforcement{}}, // 大小写敏感（闭集不靠猜）
		{"", true, Enforcement{}},
		{"inherit", true, Enforcement{}},                                        // 未登记的说法
		{"direct:extra", true, Enforcement{}},                                   // direct 不接受参数
		{"funnel:a:b", false, Enforcement{Kind: EnforcementFunnel, Via: "a:b"}}, // via 保留剩余部分
	}
	for _, c := range cases {
		t.Run("parse:"+c.raw, func(t *testing.T) {
			got, err := ParseEnforcement(c.raw)
			if c.wantErr {
				if !errors.Is(err, ErrEnforcementUnknown) {
					t.Fatalf("期望文法错误，实得 %v / %+v", err, got)
				}
				if got != (Enforcement{}) {
					t.Fatal("解析失败时不得返回半构造值")
				}
				return
			}
			if err != nil {
				t.Fatalf("解析失败：%v", err)
			}
			if got != c.want {
				t.Fatalf("解析结果 %+v，期望 %+v", got, c.want)
			}
			if !got.Valid() {
				t.Fatal("合法解析结果应自洽")
			}
			// 往返：文本 → 结构 → 文本 必须同形（内省与快照比对依赖它）
			again, err := ParseEnforcement(got.String())
			if err != nil || again != got {
				t.Fatalf("往返不一致：%q → %+v", got.String(), again)
			}
		})
	}
}

func TestVocab_EnforcementSelfConsistency(t *testing.T) {
	if (Enforcement{Kind: EnforcementDirect, Via: "x"}).Valid() {
		t.Fatal("direct 带 Via 属不自洽，必须判非法")
	}
	if (Enforcement{Kind: EnforcementFunnel}).Valid() {
		t.Fatal("funnel 无 Via 属不自洽，必须判非法")
	}
	if (Enforcement{Kind: "whatever"}).Valid() {
		t.Fatal("类别闭集外必须判非法")
	}
}

// ---- 注册表：校验与查询 ----

func TestRegistry_RejectsEveryKindOfBadPoint(t *testing.T) {
	roles := mustRoles(t, "support", "admin")
	cases := []struct {
		name  string
		p     PermissionPoint
		is    error
		check string
	}{
		{"空 Key", point("", "admin"), ErrPermissionKeyRequired, ""},
		{"只有空白的 Key", PermissionPoint{Key: " \t", Label: "x"}, ErrPermissionKeyRequired, ""},
		{"无 Label", PermissionPoint{Key: "k", Scope: ScopePlatform, DefaultRoles: []kitctx.Role{"admin"}, Enforced: DirectEnforcement()}, ErrPermissionLabelRequired, `"k"`},
		{"Scope 越界", func() PermissionPoint {
			p := point("k", "admin")
			p.Scope = "global"
			return p
		}(), ErrScopeUnknown, "闭集"},
		{"Enforced 越界", func() PermissionPoint {
			p := point("k", "admin")
			p.Enforced = Enforcement{Kind: "inherit"}
			return p
		}(), ErrEnforcementUnknown, ""},
		{"默认角色为空（fail-open 防线）", point("k"), ErrDefaultRolesRequired, "公开接口请不挂权限点"},
		{"角色不在闭集内", point("k", "support", "root"), ErrRoleOutsideSet, `"root"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, err := NewRegistry(roles)
			if err != nil {
				t.Fatalf("构造空表失败：%v", err)
			}
			before := r.Len()
			err = r.Register(c.p)
			if !errors.Is(err, c.is) {
				t.Fatalf("期望 %v，实得 %v", c.is, err)
			}
			if r.Len() != before {
				t.Fatalf("被拒的声明不得进入注册表：%d → %d", before, r.Len())
			}
			if c.check != "" && !strings.Contains(err.Error(), c.check) {
				t.Fatalf("错误应点名 %q，实得 %v", c.check, err)
			}
		})
	}
}

func TestRegistry_RejectsDuplicateKeyAndNilRoles(t *testing.T) {
	roles := mustRoles(t, "admin")
	if _, err := NewRegistry(nil, point("k", "admin")); !errors.Is(err, ErrRoleSetEmpty) {
		t.Fatalf("未注入角色闭集必须失败，实得 %v", err)
	}
	r, err := NewRegistry(roles, point("perm.a", "admin"), point("perm.b", "admin"))
	if err != nil {
		t.Fatalf("合法注册失败：%v", err)
	}
	if err := r.Register(point("perm.a", "admin")); !errors.Is(err, ErrPermissionDuplicate) {
		t.Fatalf("重名必须判失败，实得 %v", err)
	}
	if r.Len() != 2 {
		t.Fatalf("重名拒绝后数量应不变，实得 %d", r.Len())
	}
	// 列表构造与逐个 Register 必须同一条校验路径：坏条目 ⇒ 整体失败、不留半成品。
	bad, err := NewRegistry(roles, point("ok", "admin"), point("bad"))
	if !errors.Is(err, ErrDefaultRolesRequired) || bad != nil {
		t.Fatalf("批量注册遇到坏条目应返回 nil 注册表，实得 %v / %v", bad, err)
	}
}

func TestRegistry_LookupAndRoleResolution(t *testing.T) {
	roles := mustRoles(t, "auditor", "support", "admin")
	p := PermissionPoint{
		Key:          "engine.permission.query",
		Label:        "权限口径问答",
		Scope:        ScopeProject,
		DefaultRoles: []kitctx.Role{"auditor", "admin"},
		Enforced:     Enforcement{Kind: EnforcementFunnel, Via: "engine_hook"},
		Note:         "两口径冲突面在此收敛",
	}
	r, err := NewRegistry(roles, p)
	if err != nil {
		t.Fatalf("注册失败：%v", err)
	}
	got, ok := r.Lookup("engine.permission.query")
	if !ok || got.Label != p.Label || got.Enforced.String() != "funnel:engine_hook" {
		t.Fatalf("Lookup 结果不符：%+v", got)
	}
	if _, ok := r.Lookup("不存在的键"); ok {
		t.Fatal("未知键不得返回存在")
	}
	if !r.Known("engine.permission.query") || r.Known("") {
		t.Fatal("Known 判定不符")
	}
	if strings.Join(r.Keys(), ",") != "engine.permission.query" {
		t.Fatalf("Keys 顺序应为注册顺序，实得 %v", r.Keys())
	}

	gotRoles, err := r.RolesFor("engine.permission.query")
	if err != nil || !reflect.DeepEqual([]kitctx.Role(gotRoles), []kitctx.Role{"auditor", "admin"}) {
		t.Fatalf("RolesFor 结果不符：%v / %v", gotRoles, err)
	}
	gotStrings, err := r.RequiredRolesFor("engine.permission.query")
	if err != nil || strings.Join(gotStrings, ",") != "auditor,admin" {
		t.Fatalf("RequiredRolesFor 应给出 tools 需要的字符串切片：%v / %v", gotStrings, err)
	}

	// ⚠️ 关键反证：未知权限点必须**报错且不返回空角色**。
	// 空 RequiredRoles 在 tools.allows 里等于"任意已判定角色可用"⇒ 返回空就是 fail-open。
	emptyRoles, err := r.RolesFor("写错的键")
	if !errors.Is(err, ErrUnknownPermission) {
		t.Fatalf("未知权限点必须判 ErrUnknownPermission，实得 %v", err)
	}
	if len(emptyRoles) != 0 {
		t.Fatalf("失败时不得带出角色，实得 %v", emptyRoles)
	}
	emptyStr, err := r.RequiredRolesFor("写错的键")
	if !errors.Is(err, ErrUnknownPermission) || emptyStr != nil {
		t.Fatalf("衔接口失败时必须给出 nil，实得 %v / %v", emptyStr, err)
	}
}

func TestRegistry_DeclaredDataIsCopied(t *testing.T) {
	roles := mustRoles(t, "admin", "support")
	src := point("perm.x", "admin", "support")
	r, err := NewRegistry(roles, src)
	if err != nil {
		t.Fatalf("注册失败：%v", err)
	}
	src.DefaultRoles[0] = "篡改" // 调用方事后改切片
	if got := r.All()[0].DefaultRoles[0]; got != "admin" {
		t.Fatalf("注册表必须存副本，实得 %q", got)
	}
	out := r.All()
	out[0].Label = "改副本"
	if r.All()[0].Label == "改副本" {
		t.Fatal("All() 必须是深拷贝语义")
	}
	if r.Roles() == nil || r.Roles().Len() != 2 {
		t.Fatal("Roles() 应返回注入的闭集")
	}
	var nilReg *Registry
	if nilReg.Len() != 0 || nilReg.All() != nil || nilReg.Known("x") {
		t.Fatal("nil 注册表必须全部返回空值（fail-closed）")
	}
	if _, ok := nilReg.Lookup("x"); ok {
		t.Fatal("nil 注册表不得认为任何权限点存在")
	}
	if err := nilReg.Register(src); err == nil {
		t.Fatal("nil 注册表 Register 必须判失败")
	}
}

func TestRegistry_SortedKeysForStableSnapshot(t *testing.T) {
	roles := mustRoles(t, "admin")
	r, err := NewRegistry(roles, point("b.perm", "admin"), point("a.perm", "admin"), point("c.perm", "admin"))
	if err != nil {
		t.Fatalf("注册失败：%v", err)
	}
	if got := strings.Join(r.SortedKeys(), ","); got != "a.perm,b.perm,c.perm" {
		t.Fatalf("SortedKeys 必须字典序，实得 %q", got)
	}
	if got := strings.Join(r.Keys(), ","); got != "b.perm,a.perm,c.perm" {
		t.Fatalf("Keys 必须是注册顺序，实得 %q", got)
	}
}

// ---- 框架内**不得出现角色字面量**（镜像引擎侧已有的 AST 纪律）----

// forbiddenRoleLiterals 是本测试**用来查找**的角色字面量。
//
// 它们出现在测试文件里是合法的：测试要构造假值域；被禁的是实现文件 ——
// 框架一旦写死某个部署的角色表，就不再可插拔（这正是本包存在的理由）。
var forbiddenRoleLiterals = map[string]bool{
	"v1": true, "v2": true, "v2-1": true, "support": true, "auditor": true, "support-2": true, "v4": true,
	"admin": true, "L0": true, "L1": true, "L2": true, "L3": true, "L4": true,
}

// forEachImplFile 对本包**实现文件**（排除 `_test.go`）逐个做 AST 检查。
// 两个断言共用它，避免"扫描口径"在测试里写两遍（L-014）。
func forEachImplFile(t *testing.T, fn func(fileName string, fset *token.FileSet, f *ast.File)) {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("读取包目录失败：%v", err)
	}
	checked := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("解析 %s 失败：%v", name, err)
		}
		checked++
		fn(name, fset, f)
	}
	if checked == 0 {
		t.Fatal("没有检查任何实现文件 ⇒ 本断言空转")
	}
	t.Logf("已检查实现文件数：%d", checked)
}

func TestAuthz_NoRoleLiteralInSource(t *testing.T) {
	forEachImplFile(t, func(name string, fset *token.FileSet, f *ast.File) {
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			v, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			if forbiddenRoleLiterals[v] {
				t.Errorf("%s:%d 出现角色字面量 %q ⇒ 框架把某个部署的角色表写死了，"+
					"闭集必须由宿主经 NewRoleSet 注入", name, fset.Position(lit.Pos()).Line, v)
			}
			return true
		})
	})
	// 反向自证：查找表本身必须非空且含 admin（否则上面的断言可以随便被清空）
	if !forbiddenRoleLiterals["admin"] || len(forbiddenRoleLiterals) < 8 {
		t.Fatalf("角色字面量查找表被削弱（%d 项）⇒ 守卫失效", len(forbiddenRoleLiterals))
	}
}

// TestAuthz_NoSecondAuthorizationSurface 把「本包不判定授权」从注释变成可机械核验的断言。
//
// 为什么需要它：这条是整个权限层最容易悄悄失守的地方 —— 只要有人在本包加一个
// `Allowed(role string) bool`，框架内就出现第二套"角色 ⇒ 有权"的口径，
// 而它和 `tools.allows` 迟早漂移（REVIEW-2 节C-1 的真实成因就是三份枚举一份守卫）。
func TestAuthz_NoSecondAuthorizationSurface(t *testing.T) {
	denyPrefixes := []string{"Allow", "Can", "May", "Grant", "Revoke", "Permit", "Authorize", "Check", "Verify", "Has", "Must", "Should"}
	if len(denyPrefixes) == 0 {
		t.Fatal("deny 表被清空 ⇒ 断言空转")
	}
	// ⚠️ 指针接收者的方法**只**挂在指针类型上：用 .Elem 取到值类型会得到空方法集，
	// 于是下面的 deny 扫描与白名单比对全部空转（本测试第一版就是这么错的，靠 len<8 自证抓到）。
	targets := []reflect.Type{
		reflect.TypeOf((*Registry)(nil)), // *Registry
		reflect.TypeOf((*RoleSet)(nil)),  // *RoleSet
		reflect.TypeOf(PermissionPoint{}),
		reflect.TypeOf(Enforcement{}),
		reflect.TypeOf(Scope("")),
	}
	scanned := 0
	for _, ty := range targets {
		scanned += ty.NumMethod()
		for i := 0; i < ty.NumMethod(); i++ {
			n := ty.Method(i).Name
			for _, d := range denyPrefixes {
				if strings.HasPrefix(n, d) {
					t.Errorf("%s.%s 听起来像授权判定 ⇒ 授权谓词只允许存在于 tools.Registry.allows", ty, n)
				}
			}
		}
	}
	if scanned < 15 {
		t.Fatalf("反射扫到的方法总数只有 %d ⇒ 类型/接收者取错了，本断言在空转", scanned)
	}

	// 导出面白名单：新增导出方法必须在此写明"它只做声明/查询/转换"的理由。
	want := []string{"All", "Keys", "Known", "Len", "Lookup", "Register", "RequiredRolesFor", "Roles", "RolesFor", "SortedKeys"}
	got := exportedMethods(reflect.TypeOf((*Registry)(nil)))
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Registry 导出面变化：\n  期望 %v\n  实得 %v\n若确属新增，请说明它为何不构成第二套授权口径，并同步本白名单", want, got)
	}
	if len(got) < 8 {
		t.Fatalf("导出面白名单被削弱（%d 项）⇒ 上面的比对可能空转", len(got))
	}

	// 角色类型只有一个：实现文件里不得再声明名为 Role 的类型（一律用 kitctx.Role）。
	forEachImplFile(t, func(name string, fset *token.FileSet, f *ast.File) {
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.TYPE {
				continue
			}
			for _, spec := range gd.Specs {
				ts, _ := spec.(*ast.TypeSpec)
				if ts != nil && ts.Name.Name == "Role" {
					t.Errorf("%s:%d 重复声明角色类型 Role ⇒ 角色类型必须只有 kitctx.Role 一个",
						name, fset.Position(ts.Pos()).Line)
				}
			}
		}
	})
}

func exportedMethods(ty reflect.Type) []string {
	out := make([]string, 0, ty.NumMethod())
	for i := 0; i < ty.NumMethod(); i++ {
		if m := ty.Method(i); m.IsExported() {
			out = append(out, m.Name)
		}
	}
	return out
}
