package security

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"unicode"
)

// ---------------------------------------------------------------------------
// 测试辅助（本文件内私有，前缀统一用 blacklistTestGuard 以免与同包其它任务冲突）
// ---------------------------------------------------------------------------

func blacklistTestJSON(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(raw)
}

// blacklistTestCollectKeys 收集任意 JSON 外形里出现过的全部 key（任意层级）。
func blacklistTestCollectKeys(value any, out map[string]struct{}) {
	switch node := value.(type) {
	case map[string]any:
		for key, child := range node {
			out[key] = struct{}{}
			blacklistTestCollectKeys(child, out)
		}
	case []any:
		for _, child := range node {
			blacklistTestCollectKeys(child, out)
		}
	}
}

// blacklistTestCollectStrings 收集任意 JSON 外形里出现过的全部字符串值。
func blacklistTestCollectStrings(value any, out *[]string) {
	switch node := value.(type) {
	case map[string]any:
		for _, child := range node {
			blacklistTestCollectStrings(child, out)
		}
	case []any:
		for _, child := range node {
			blacklistTestCollectStrings(child, out)
		}
	case string:
		*out = append(*out, node)
	}
}

func blacklistTestIsHex(s string) bool {
	for _, r := range strings.ToLower(s) {
		if !strings.ContainsRune("0123456789abcdef", r) {
			return false
		}
	}
	return s != ""
}

// ---------------------------------------------------------------------------
// ：命中即完全移除（不含哈希 / 截断 / 变形）
// ---------------------------------------------------------------------------

func TestBlacklist_RemovesNeverSendFields(t *testing.T) {
	cases := []struct {
		name        string
		in          any
		wantJSON    string
		wantRemoved []string
	}{
		{
			name: "IMEI 原始值族（含派生键名与超大整数）",
			in: map[string]any{
				"imei":            "861234567890124",
				"imei_clientid":   "abc",
				"clientid":        "xyz",
				"device_imei_big": int64(861234567890124),
				"keep":            "v",
			},
			wantJSON:    `{"keep":"v"}`,
			wantRemoved: []string{"clientid", "device_imei_big", "imei", "imei_clientid"},
		},
		{
			name: "内部 token / 凭据族",
			in: map[string]any{
				"access_token":  "t",
				"client_secret": "s",
				"password":      "p",
				"api_key":       "k",
				"keep":          1,
			},
			wantJSON:    `{"keep":1}`,
			wantRemoved: []string{"access_token", "api_key", "client_secret", "password"},
		},
		{
			name: "内部 host 族",
			in: map[string]any{
				"internal_host": "h",
				"inner_domain":  "d",
				"private_ip":    "10.0.0.1",
				"keep":          true,
			},
			wantJSON:    `{"keep":true}`,
			wantRemoved: []string{"inner_domain", "internal_host", "private_ip"},
		},
		{
			name: "嵌套 map",
			in: map[string]any{
				"data": map[string]any{"user": map[string]any{"imei": "x", "name": "n"}},
			},
			wantJSON:    `{"data":{"user":{"name":"n"}}}`,
			wantRemoved: []string{"data.user.imei"},
		},
		{
			name: "数组元素内命中",
			in: map[string]any{
				"list": []any{
					map[string]any{"imei": "a", "n": 1},
					map[string]any{"n": 2},
				},
			},
			wantJSON:    `{"list":[{"n":1},{"n":2}]}`,
			wantRemoved: []string{"list[0].imei"},
		},
		{
			name: "键名大小写 / 分隔符 / 驼峰均归一",
			in: map[string]any{
				"IMEI":          "a",
				"ClientId":      "b",
				"imei-ClientId": "c",
				"internalHost":  "d",
				"keep":          "e",
			},
			wantJSON:    `{"keep":"e"}`,
			wantRemoved: []string{"ClientId", "IMEI", "imei-ClientId", "internalHost"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			masked, removed, err := ApplyBlacklist(tc.in)
			if err != nil {
				t.Fatalf("ApplyBlacklist 报错：%v", err)
			}
			if got := blacklistTestJSON(t, masked); got != tc.wantJSON {
				t.Fatalf("移除结果不符\n got = %s\nwant = %s", got, tc.wantJSON)
			}
			want := append([]string(nil), tc.wantRemoved...)
			sort.Strings(want)
			if !reflect.DeepEqual(removed, want) {
				t.Fatalf("removed 证据不符\n got = %v\nwant = %v", removed, want)
			}
		})
	}
}

// TestBlacklist_NoHashTruncateOrDeformation 固化 框架裁决 决策 5：
// 「哈希或截断形式同样移除（不是替代方案）」——输出里不得出现任何替代形态。
//
// 修前缺陷（REVIEW-2-quality.md 节B.4.1 F-08）：输入只有 `{"imei": imei}`，而前置断言已把
// `masked` 钉死为**空 map** ⇒ `keys` / `values` 恒空 ⇒「原值残留 / 截断 / 哈希 / 掩码」
// 四个负控分支**全部不可达**（负控空转）。本用例改为：
//   - 输出里放一个**幸存字段**，让扫描真的有键有值可判，并显式断言集合非空（反空转前置）；
//   - 命中键的移除结果与**独立字面 oracle** 逐项比对；
//   - 四个形态分支各配一个**构造样本正控**（判据 D），证明它们真的会命中。
func TestBlacklist_NoHashTruncateOrDeformation(t *testing.T) {
	const imei = "861234567890124"
	const survivor = "非敏感字段必须原样保留"

	masked, removed, err := ApplyBlacklist(map[string]any{"imei": imei, "note": survivor})
	if err != nil {
		t.Fatalf("ApplyBlacklist 报错：%v", err)
	}

	// 判据 B：键名不得出现 imei 变形。反空转前置：键集合非空（否则扫描在空集合上空转）。
	keys := map[string]struct{}{}
	blacklistTestCollectKeys(masked, keys)
	if len(keys) == 0 {
		t.Fatalf("判据前提：masked 必须含幸存字段的键，否则键名扫描空转（修前 F-08 的成因）")
	}
	for key := range keys {
		if strings.Contains(strings.ToLower(key), "imei") {
			t.Fatalf("出现 imei 变形键名 %q（不允许哈希/截断/重命名形态）", key)
		}
	}

	// 判据 C：值不得是「原值残留 / 截断 / 哈希 / 掩码」四形态之一。
	// 反空转前置：值集合非空（修前 `values` 恒空 ⇒ 四个分支一个都跑不到）。
	values := []string{}
	blacklistTestCollectStrings(masked, &values)
	if len(values) == 0 {
		t.Fatalf("判据前提：masked 必须含幸存字段的值，否则四形态负控空转（修前 F-08 的成因）")
	}
	for _, value := range values {
		if reason, bad := blacklistTestForbiddenForm(value, imei); bad {
			t.Fatalf("%s：%q", reason, value)
		}
	}

	// 判据 A（放在最后，避免它先失败而让判据 B/C 永远跑不到）：
	// 证据只报命中键；命中键整体移除后其余字段原样保留 —— 结果逐字节等于独立字面 oracle。
	if !reflect.DeepEqual(removed, []string{"imei"}) {
		t.Fatalf("removed = %v，want [imei]", removed)
	}
	want := map[string]any{"note": survivor}
	if !reflect.DeepEqual(masked, want) {
		t.Fatalf("IMEI 键须被整体移除且不留任何替代形态：\n got=%v\nwant=%v", masked, want)
	}

	// 判据 D（正控自检）：四个分支各自必须命中一个**构造样本**，证明判据 C 不是「恒不命中」的空转判据。
	for _, probe := range []struct {
		name  string
		value string
		want  string
	}{
		{name: "原值残留", value: "prefix-" + imei, want: "原值以任何形态残留"},
		{name: "截断残留", value: imei[:8] + "0000", want: "原值被截断后残留"},
		{name: "哈希形态", value: "0123456789abcdef0123456789abcdef", want: "出现哈希（MD5）形态替代值"},
		{name: "掩码形态", value: imei[:4] + "****" + imei[len(imei)-4:], want: "出现掩码形态替代值"},
	} {
		reason, bad := blacklistTestForbiddenForm(probe.value, imei)
		if !bad || reason != probe.want {
			t.Fatalf("扫描器对本应命中的形态漏判（name=%s value=%q）：reason=%q 命中=%v want=%q",
				probe.name, probe.value, reason, bad, probe.want)
		}
	}
}

// blacklistTestForbiddenForm 判定一个**输出字符串值**是否属于「原值残留 / 截断 / 哈希 / 掩码」四形态。
//
// 抽成函数有两个用途：① 在生产输出上逐值判定；② 用四个构造样本做正控自检，
// 证明四个分支真的会命中（否则「输出里恰好没有值」会让负控空转而判据仍然全绿，即 F-08 的成因）。
func blacklistTestForbiddenForm(value, imei string) (string, bool) {
	switch {
	case strings.Contains(value, imei):
		return "原值以任何形态残留", true
	case strings.Contains(value, imei[:8]) || strings.Contains(value, imei[len(imei)-8:]):
		return "原值被截断后残留", true
	case len(value) == 32 && blacklistTestIsHex(value):
		return "出现哈希（MD5）形态替代值", true
	case strings.Contains(value, "*"):
		return "出现掩码形态替代值", true
	}
	return "", false
}

// ---------------------------------------------------------------------------
// ：黑名单不可关闭（增项只可加、不可减、不可置空关闭）
// ---------------------------------------------------------------------------

func TestBlacklist_ExtraFieldsAreAdditiveOnly(t *testing.T) {
	payload := func() map[string]any {
		return map[string]any{"imei": "x", "custom_field_name_xyz": "y", "keep": "z"}
	}

	// 空 / 空串 / 无关增项都不能减少内建集合（不可置空关闭）
	for _, extras := range [][]string{nil, {}, {""}, {"   "}, {"unrelated_field"}} {
		masked, removed, err := ApplyBlacklist(payload(), extras...)
		if err != nil {
			t.Fatalf("extras=%v 报错：%v", extras, err)
		}
		if got := blacklistTestJSON(t, masked); got != `{"custom_field_name_xyz":"y","keep":"z"}` {
			t.Fatalf("extras=%v 让内建集合失效，masked=%s", extras, got)
		}
		if !reflect.DeepEqual(removed, []string{"imei"}) {
			t.Fatalf("extras=%v 时 removed=%v，内建命中必须仍然存在", extras, removed)
		}
	}

	// 增项生效：额外字段名按精确名追加
	masked, removed, err := ApplyBlacklist(payload(), "custom_field_name_xyz")
	if err != nil {
		t.Fatalf("报错：%v", err)
	}
	if got := blacklistTestJSON(t, masked); got != `{"keep":"z"}` {
		t.Fatalf("增项未生效：masked=%s", got)
	}
	if want := []string{"custom_field_name_xyz", "imei"}; !reflect.DeepEqual(removed, want) {
		t.Fatalf("removed=%v，want %v", removed, want)
	}

	// 增项不能「替换」内建集合：传一个无害字段名，内建命中必须仍在
	_, removed, err = ApplyBlacklist(payload(), "keep")
	if err != nil {
		t.Fatalf("报错：%v", err)
	}
	if !reflect.DeepEqual(removed, []string{"imei", "keep"}) {
		t.Fatalf("增项覆盖了内建集合：removed=%v", removed)
	}
}

// TestBlacklist_UnclosableByPayloadOrArgs 证明「没有任何入口能关掉它」：
// 连“看起来像开关”的 payload 键与增项参数都无法让内建命中消失。
func TestBlacklist_UnclosableByPayloadOrArgs(t *testing.T) {
	payload := map[string]any{
		"blacklist":         []any{},
		"enabled":           false,
		"disabled":          true,
		"skip_blacklist":    true,
		"blacklist_enabled": false,
		"no_blacklist":      true,
		"imei":              "x",
		"keep":              1,
	}
	masked, removed, err := ApplyBlacklist(payload, "enabled", "disabled", "blacklist", "")
	if err != nil {
		t.Fatalf("报错：%v", err)
	}
	if got := blacklistTestJSON(t, masked); got != `{"blacklist_enabled":false,"keep":1,"no_blacklist":true,"skip_blacklist":true}` {
		t.Fatalf("开关式 payload/增项让内建命消失：masked=%s", got)
	}
	if !reflect.DeepEqual(removed, []string{"blacklist", "disabled", "enabled", "imei"}) {
		t.Fatalf("removed=%v", removed)
	}
}

// ---------------------------------------------------------------------------
// 契约边界：nil / 标量 / 不可判定值 / 零副作用 / 并发
// ---------------------------------------------------------------------------

func TestBlacklist_NilAndScalar(t *testing.T) {
	masked, removed, err := ApplyBlacklist(nil)
	if err != nil || masked != nil || removed != nil {
		t.Fatalf("nil 输入应返回 (nil, nil, nil)，实际 (%v, %v, %v)", masked, removed, err)
	}

	masked, removed, err = ApplyBlacklist("plain")
	if err != nil || masked != "plain" || removed != nil {
		t.Fatalf("标量输入应原样返回且无证据，实际 (%v, %v, %v)", masked, removed, err)
	}

	masked, removed, err = ApplyBlacklist([]any{"a", "b"})
	if err != nil || removed != nil {
		t.Fatalf("无命中的数组应无证据，实际 (%v, %v)", removed, err)
	}
	if got := blacklistTestJSON(t, masked); got != `["a","b"]` {
		t.Fatalf("数组应原样返回，实际 %s", got)
	}
}

// TestBlacklist_UnverifiableValueFailsClosed 无法按 JSON 外形比对时返回错误，
// 而不是静默返回「空证据」把不可判定伪装成「没有敏感字段」。
func TestBlacklist_UnverifiableValueFailsClosed(t *testing.T) {
	masked, removed, err := ApplyBlacklist(map[string]any{"ch": make(chan int), "imei": "x"})
	if err == nil {
		t.Fatalf("不可比对的值必须报错（fail-closed），实际 masked=%v removed=%v", masked, removed)
	}
	if masked != nil || removed != nil {
		t.Fatalf("报错时必须不返回数据与证据，实际 (%v, %v)", masked, removed)
	}
}

// TestBlacklist_DoesNotMutateInput 调用方数据零副作用（脱敏不得就地改写入参）。
func TestBlacklist_DoesNotMutateInput(t *testing.T) {
	in := map[string]any{
		"imei": "x",
		"keep": map[string]any{"imei": "y", "n": 1},
	}
	data := map[string]any{
		"imei": "x",
		"keep": map[string]any{"imei": "y", "n": 1},
	}
	if _, _, err := ApplyBlacklist(in); err != nil {
		t.Fatalf("报错：%v", err)
	}
	if !reflect.DeepEqual(in, data) {
		t.Fatalf("入参被就地改写：%v", in)
	}
}

// TestBlacklist_PrecisionKeepsNonSensitiveFields 固化精确性决定：
// 复数 / 统计量 / 无范围词的主机名等**不得**被误伤。
func TestBlacklist_PrecisionKeepsNonSensitiveFields(t *testing.T) {
	in := map[string]any{
		"max_tokens":   100,
		"total_tokens": 200,
		"tokens":       []any{"a"},
		"time_index":   "t",
		"session_id":   "s",
		"trace_id":     "tr",
		"host":         "example.com",
		"domain":       "example.com",
		"ip":           "1.2.3.4",
		"url":          "http://example.com",
		"role":         "admin",
		"keep":         true,
	}
	masked, removed, err := ApplyBlacklist(in)
	if err != nil {
		t.Fatalf("报错：%v", err)
	}
	if len(removed) != 0 {
		t.Fatalf("误伤非敏感字段：removed=%v", removed)
	}
	if got, want := blacklistTestJSON(t, masked), blacklistTestJSON(t, in); got != want {
		t.Fatalf("无命中时输出应与输入等价\n got = %s\nwant = %s", got, want)
	}
}

func TestBlacklist_ConcurrentUse(t *testing.T) {
	const workers = 8
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < 50; n++ {
				_, removed, err := ApplyBlacklist(map[string]any{"imei": "x", "keep": 1}, "custom")
				if err != nil {
					t.Errorf("并发调用报错：%v", err)
					return
				}
				if !reflect.DeepEqual(removed, []string{"imei"}) {
					t.Errorf("并发调用 removed=%v", removed)
					return
				}
			}
		}()
	}
	wg.Wait()
}

// ---------------------------------------------------------------------------
//  守卫：GUARD:TestGuard_BlacklistNotOptional
// 让「不可关闭」成为**代码结构上的事实**（导出面 + AST + 行为单调性三重固化）
// ---------------------------------------------------------------------------

func TestGuard_BlacklistNotOptional(t *testing.T) {
	fset := token.NewFileSet()

	// ---- ① 导出面扫描：本包不得出现任何「开关 / 可选 / 减项」形式的导出符号 ----
	productionOnly := func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}
	pkgs, err := parser.ParseDir(fset, ".", productionOnly, 0)
	if err != nil {
		t.Fatalf("解析包源码失败：%v", err)
	}

	dangerWords := []string{"disable", "disabled", "enable", "enabled", "optional", "bypass", "unset", "off"}
	subtractWords := []string{"set", "replace", "override", "remove", "delete", "clear", "reset", "config", "option", "options", "prune", "drop", "unprotect", "unmask", "without"}

	exported := map[string]struct{}{}
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			for _, decl := range file.Decls {
				switch d := decl.(type) {
				case *ast.FuncDecl:
					if d.Recv == nil && d.Name.IsExported() {
						exported[d.Name.Name] = struct{}{}
					}
				case *ast.GenDecl:
					for _, spec := range d.Specs {
						switch s := spec.(type) {
						case *ast.TypeSpec:
							if s.Name.IsExported() {
								exported[s.Name.Name] = struct{}{}
							}
						case *ast.ValueSpec:
							for _, name := range s.Names {
								if name.IsExported() {
									exported[name.Name] = struct{}{}
								}
							}
						}
					}
				}
			}
		}
	}

	if _, ok := exported["ApplyBlacklist"]; !ok {
		t.Fatalf("缺少唯一出口 ApplyBlacklist；导出面 = %v", exported)
	}

	for name := range exported {
		words := guardCamelWords(name)
		for _, word := range words {
			for _, danger := range dangerWords {
				if word == danger {
					t.Errorf("导出符号 %q 含「可关闭/可选」语义词 %q —— 黑名单不得存在开关入口", name, word)
				}
			}
		}
		if !guardContainsWord(words, "blacklist") {
			continue
		}
		for _, word := range words {
			for _, subtract := range subtractWords {
				if word == subtract {
					t.Errorf("导出符号 %q 含减项/改写语义词 %q —— 配置与 API 只可增项", name, word)
				}
			}
		}
	}

	// ---- ② AST 扫描 blacklist.go：无开关形参 / 无导出包级变量 / 规则表只读前缀 ----
	file, err := parser.ParseFile(fset, "blacklist.go", nil, 0)
	if err != nil {
		t.Fatalf("解析 blacklist.go 失败：%v", err)
	}

	var pkgVars []string
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.VAR {
			continue
		}
		for _, spec := range gen.Specs {
			valueSpec, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, name := range valueSpec.Names {
				if name.IsExported() {
					t.Errorf("blacklist.go 不得导出包级变量（等于给出可改写的开关句柄）：%s", name.Name)
				}
				if !strings.HasPrefix(name.Name, "blacklist") {
					t.Errorf("blacklist.go 的包级变量必须以 blacklist 前缀命名（便于审计/守卫）：%s", name.Name)
				}
				pkgVars = append(pkgVars, name.Name)
				if i < len(valueSpec.Values) {
					if literal, ok := valueSpec.Values[i].(*ast.CompositeLit); ok && len(literal.Elts) == 0 {
						t.Errorf("内建规则表 %s 不得为空集（置空即等于关闭黑名单）", name.Name)
					}
				}
			}
		}
	}
	if len(pkgVars) == 0 {
		t.Error("blacklist.go 未声明任何包级只读规则表（内建集合应在代码内）")
	}

	applyFound := false
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "ApplyBlacklist" {
			continue
		}
		applyFound = true

		var nonVariadic, variadic int
		for _, param := range fn.Type.Params.List {
			switch paramType := param.Type.(type) {
			case *ast.Ident:
				if paramType.Name == "bool" {
					t.Errorf("ApplyBlacklist 不得接受 bool 形参（最自然的「开关」）")
				}
			case *ast.StarExpr:
				t.Errorf("ApplyBlacklist 不得接受指针形参（易被当成配置/选项开关）")
			case *ast.Ellipsis:
				variadic++
				if ident, ok := paramType.Elt.(*ast.Ident); !ok || ident.Name != "string" {
					t.Errorf("ApplyBlacklist 的变参只能是 ...string（增项），实际 %v", paramType.Elt)
				}
			default:
				t.Errorf("ApplyBlacklist 出现预期外的形参类型 %T", paramType)
			}
			if _, ok := param.Type.(*ast.Ellipsis); !ok {
				nonVariadic++
			}
		}
		if nonVariadic != 1 || variadic != 1 {
			t.Errorf("ApplyBlacklist 签名应为 (data any, extraFields ...string)，实际非变参 %d 个 / 变参 %d 个", nonVariadic, variadic)
		}
		if fn.Type.Results == nil || len(fn.Type.Results.List) != 3 {
			t.Errorf("ApplyBlacklist 应返回 (any, []string, error)")
		} else {
			last := fn.Type.Results.List[len(fn.Type.Results.List)-1].Type
			if ident, ok := last.(*ast.Ident); !ok || ident.Name != "error" {
				t.Errorf("ApplyBlacklist 第三出参必须是 error，实际 %v", last)
			}
		}
	}
	if !applyFound {
		t.Fatalf("blacklist.go 未找到 ApplyBlacklist")
	}

	// blacklist.go 内不得出现任何「可关闭」语义的结构体字段（未来的回归护栏）
	ast.Inspect(file, func(node ast.Node) bool {
		typeSpec, ok := node.(*ast.TypeSpec)
		if !ok {
			return true
		}
		structType, ok := typeSpec.Type.(*ast.StructType)
		if !ok {
			return true
		}
		for _, field := range structType.Fields.List {
			for _, name := range field.Names {
				for _, word := range guardCamelWords(name.Name) {
					for _, danger := range dangerWords {
						if word == danger {
							t.Errorf("struct %s 出现「可关闭」语义字段 %s", typeSpec.Name.Name, name.Name)
						}
					}
				}
			}
		}
		return true
	})

	// ---- ③ 行为单调性：内建命中在任何「增项 / 空项 / 开关式入参」下都不消失 ----
	builtinRepresentatives := []string{"imei", "imei_clientid", "clientid", "access_token", "client_secret", "api_key", "password", "internal_host", "private_ip"}
	for _, extras := range [][]string{nil, {}, {""}, {"  "}, {"unrelated"}, {"enabled"}, {"disabled"}, {"blacklist"}, {"disable_blacklist"}} {
		in := map[string]any{"keep": 1}
		for _, field := range builtinRepresentatives {
			in[field] = "sensitive"
		}
		masked, removed, err := ApplyBlacklist(in, extras...)
		if err != nil {
			t.Fatalf("extras=%v 报错：%v", extras, err)
		}
		if len(removed) < len(builtinRepresentatives) {
			t.Fatalf("extras=%v 让内建命中消失：removed=%v", extras, removed)
		}
		keys := map[string]struct{}{}
		blacklistTestCollectKeys(masked, keys)
		for _, field := range builtinRepresentatives {
			if _, still := keys[field]; still {
				t.Fatalf("extras=%v 时永不外发字段 %q 仍能出域", extras, field)
			}
		}
	}
}

// guardCamelWords 把驼峰标识符拆成小写词（用于「可关闭」语义扫描）。
func guardCamelWords(name string) []string {
	var b strings.Builder
	for i, r := range name {
		if unicode.IsUpper(r) && i > 0 {
			b.WriteByte(' ')
		}
		b.WriteRune(unicode.ToLower(r))
	}
	return strings.Fields(b.String())
}

func guardContainsWord(words []string, want string) bool {
	for _, word := range words {
		if word == want {
			return true
		}
	}
	return false
}
