package agentkit

// guard_test.go —— 框架边界守卫（历史任务）。
//
// 判定式的权威依据是 .specs/adr/010-agentkit-internal-imports-and-extraction.md
// （主会话裁决，纠正既有设计/任务文档的字面过宽口径）：
//
//	1. `pkg/agentkit/**` 的任何 import **不得落在** module 前缀之下，**除非**该路径位于
//	   `.../pkg/agentkit/` **子树内**（框架内部各包互相 import 是允许且必要的）。
//	   ⇒ 禁止 `github.com/malizhi2202-dev/agentkit/...` 中**非 `pkg/agentkit/` 的一切**
//	   （logic/**、controllers/**、pkg/cloudweb/**、config/**、errorx/**、middlewares/** …）。
//	2. 判定式必须同时禁止标准库之外的第三方依赖；白名单只有
//	   `golang.org/x/sync/errgroup`（既有 `// indirect` require，不新增 go.mod 依赖 —— 框架裁决）。
//	3. 「拿走就能用」的单一出口基线：框架内只有 `pkg/agentkit/model/` 出现 `net/http` 的
//	   实现性使用（出域客户端 ABI 的唯一落点 —— 既有教训）。其余包（含 `im/**`）物理无出网能力（既有教训）。
//	4. go.mod 零改动：框架的非 stdlib import 集合 ⊆ 白名单，且白名单模块在**既有** go.mod/go.sum
//	   里已经存在（不是本任务新增依赖）。
//
// ⚠️ 实现上**按路径段判断**（`pkg` + `agentkit` 两段），不是 `HasPrefix` 一把梭：
// `.../pkg/agentkitfoo`、`.../pkg/agentkit_extra`、`.../pkg/agentkitmodel` 这类相似前缀
// **必须**被判违规，由 TestGuard_AgentkitImportClassification 的合成路径表逐条固化
// （实证：把谓词换成朴素 HasPrefix 后，`agentkitfoo` 探针被**误放行**，守卫假通过）。
//
// 本 module 路径**不写死**：守卫自底向上找最近的 go.mod 并读取 `module` 行
// ⇒ 框架被整目录抽取到别的 module（目录层级随之变化）后守卫自动适配，README 的
// 「复制 + 一次 sed 前缀重写 + go build/test」三步即可验收。
//
// 沿革：本守卫最初以 历史任务 的包内守卫（TestTools_ImportsStayInsideAgentkit）为范本，
// 并把它从「单包」升级为「整个 pkg/agentkit/** 子树」，另补三个缺口：
// （a）stdlib 判别改用 `go/build` 的 GOROOT 归属判定，而非「首段不含点号」启发式；
// （b）豁免分支按**路径段**判断而非前缀；（c）新增单一出口基线与 go.mod 契约断言。
// ⚠️ 历史任务 那份包内守卫**已删除**（见框架裁决与既有教训）：它与本守卫是同一判定式的
// 重复实现（R3），且硬编码宿主 layout ⇒ 被抽取到别的 module 后必然失效（实测在 tools 包 FAIL）。
// **本守卫是唯一权威**，且子树由 go.mod + 自身位置推导 ⇒ 抽取后自动适配。
//
// 既有教训：本守卫的**非空转**由 交付纪要 的四个探针实证（临时注入违规 → 观察到 FAIL → 删净 → PASS）。
// 测试内另有**常驻非空转断言**：扫描到的文件/import 数下限、豁免分支确实被走到、
// 单一出口基线集合非空——否则「永远通过」会被静默当成「真的有守卫」。
// （该断言在实现期抓到过一次真 bug：`WalkDir(".")` 的根目录曾被 `strings.HasPrefix(name,".")`
// 误 SkipDir，导致扫描面为 0。）

import (
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// ---------- 单一出口基线：符号方向四档（框架裁决，取代原「路径唯一」判据） ----------

// guardHTTPDir 是一个 net/http 符号的方向归类。
type guardHTTPDir int

const (
	guardHTTPUnknown guardHTTPDir = iota // 未归类 ⇒ 违规（D4：强制显式归类，禁止靠"再放开一个包"消红）
	guardHTTPEgress                      // D1 出域能力（发请求）
	guardHTTPIngress                     // D2 入域能力（对外提供服务）
	guardHTTPNeutral                     // D3 双向数据类型与常量（本身不具备任何方向的能力）
)

func (d guardHTTPDir) String() string {
	switch d {
	case guardHTTPEgress:
		return "出域(D1)"
	case guardHTTPIngress:
		return "入域(D2)"
	case guardHTTPNeutral:
		return "中立(D3)"
	default:
		return "未归类(D4)"
	}
}

var (
	// guardEgressPkgs 是允许出现 D1 出域符号的包目录（相对框架根）；断言取**等号**（比原判据 ⊆ 更严）。
	guardEgressPkgs = []string{"model"}
	// guardIngressPkgs 是允许出现 D2 入域符号的包目录。web 层落地前该判据无实际压力，由合成用例表承担牙齿。
	guardIngressPkgs = []string{"web"}
	// guardNeutralPkgs 是允许出现 D3 中立符号的包目录 —— 中立 ≠ 自由：框架内其它包不需要任何 HTTP 报文类型。
	guardNeutralPkgs = []string{"model", "web"}
)

// guardHTTPEgressSymbols：D1。拿到这些符号就等于能自己发请求 ⇒ 绕开 框架裁决 职责链。
var guardHTTPEgressSymbols = map[string]bool{
	"Client": true, "Transport": true, "NewRequest": true, "NewRequestWithContext": true,
	"Get": true, "Post": true, "PostForm": true, "Head": true,
	"ProxyFromEnvironment": true, "ProxyURL": true, "NewCookieJar": true,
}

// guardHTTPIngressSymbols：D2。
var guardHTTPIngressSymbols = map[string]bool{
	"Server": true, "ListenAndServe": true, "ListenAndServeTLS": true, "Serve": true, "ServeTLS": true,
	"Handler": true, "HandlerFunc": true, "ResponseWriter": true, "ServeMux": true, "NewServeMux": true,
	"DefaultServeMux": true, "FileServer": true, "StripPrefix": true, "NotFound": true, "NotFoundHandler": true,
	"Error": true, "Redirect": true, "TimeoutHandler": true, "MaxBytesReader": true,
	"Pusher": true, "Hijacker": true, "Flusher": true, "ConnState": true, "CloseNotifier": true,
}

// guardHTTPNeutralSymbols：D3（显式名单部分）。
var guardHTTPNeutralSymbols = map[string]bool{
	"Request": true, "Response": true, "Header": true, "Cookie": true, "Cookies": true,
	"NewResponse": true, "CanonicalHeaderKey": true, "Time": true, "Protocol": true, "Protocols": true,
	// MaxBytesError：P4 落地 web 时**显式归类**的补项（请求体超限的哨兵类型）。
	// 名字不以 Status/Method/Protocol/Err/State 起头 ⇒ 前缀表覆盖不到；归中立而非入域：
	// 它只描述"一个请求体太大"，不具备服务端能力面。D3 仍是 ⊆ {model, web}，不放宽等号基线。
	"MaxBytesError": true,
}

// guardHTTPNeutralPrefixes：D3（前缀部分，覆盖状态码 / 方法 / 协议 / 错误常量族）。
var guardHTTPNeutralPrefixes = []string{"Status", "Method", "Protocol", "Err", "State"}

// guardHTTPUtilSymbolsEgress / guardHTTPUtilSymbolsIngress：net/http/httputil 的少量符号。
// ReverseProxy 虽以 Handler 面目出现，但它**确实在向外发请求** ⇒ 归 D1（框架裁决）。
var (
	guardHTTPUtilSymbolsEgress = map[string]bool{
		"ClientConn": true, "NewClientConn": true, "ProxyServer": true, "ReverseProxy": true,
	}
	guardHTTPUtilSymbolsIngress = map[string]bool{
		"Server": true, "ChunkWriter": true, "NewChunkWriter": true, "NewChunkedReader": true, "ServeContent": true,
	}
)

// guardClassifyHTTP 是纯函数分类器（可被合成用例表驱动反证）。pkgPath 必须是 net/http 或 net/http/httputil。
func guardClassifyHTTP(pkgPath, symbol string) guardHTTPDir {
	switch pkgPath {
	case "net/http":
		switch {
		case guardHTTPEgressSymbols[symbol]:
			return guardHTTPEgress
		case guardHTTPIngressSymbols[symbol]:
			return guardHTTPIngress
		case guardHTTPNeutralSymbols[symbol]:
			return guardHTTPNeutral
		}
		for _, p := range guardHTTPNeutralPrefixes {
			if strings.HasPrefix(symbol, p) {
				return guardHTTPNeutral
			}
		}
		return guardHTTPUnknown
	case "net/http/httputil":
		switch {
		case guardHTTPUtilSymbolsEgress[symbol]:
			return guardHTTPEgress
		case guardHTTPUtilSymbolsIngress[symbol]:
			return guardHTTPIngress
		}
		return guardHTTPUnknown
	default:
		return guardHTTPUnknown
	}
}

const (
	// guardEgressPkg 保留为**唯一出域包名**的字符串形态，供报错措辞与既有文档引用（框架裁决 第 4 条）。
	guardEgressPkg = "model"
)

// guardSanctionedThirdParty 是框架级第三方白名单（框架裁决 第 3 条 / 框架裁决 决策 3 / D13）：
// **package 路径 → 所属 module 路径**。
// 只有 errgroup：它所属的 module 已在既有 go.mod 里以 `// indirect` 存在，**不新增**任何 module 依赖。
// （package ≠ module：go.mod 的 require 记的是 module，故两者都要写清，不能拿 package 去比对 require。）
var guardSanctionedThirdParty = map[string]string{
	"golang.org/x/sync/errgroup": "golang.org/x/sync",
}

// guardSanctioned 判 package 路径是否在第三方白名单内。
func guardSanctioned(pkg string) bool {
	_, ok := guardSanctionedThirdParty[pkg]
	return ok
}

// guardEnv 是本次守卫运行所处的 module 环境（抽取到别的 module 后自动改变）。
type guardEnv struct {
	ModulePath string // 宿主 module 路径（读最近 go.mod 的 module 行）
	GoMod      string // 最近的 go.mod 绝对路径
	GoSum      string // 同目录的 go.sum
	Root       string // 框架根目录（= 本守卫所在包目录，即 cwd）
	Subtree    string // 框架根包的 import 前缀（唯一豁免子树：模块内兄弟包）
}

func (e guardEnv) describe() string {
	return "module=" + e.ModulePath + "、框架根=" + e.Subtree
}

// guardFindGoMod 自底向上找最近的 go.mod：不得写死相对路径
// —— 框架在仓库里是 `pkg/agentkit`（../../go.mod），被抽取后通常是 `<module>/agentkit`（../go.mod）。
func guardFindGoMod(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("获取包目录失败：%v", err)
	}
	for {
		candidate := filepath.Join(dir, "go.mod")
		if _, statErr := os.Stat(candidate); statErr == nil {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("自 %s 向上未找到 go.mod", dir)
		}
		dir = parent
	}
}

// guardModulePath 读取 go.mod 的 module 路径。
func guardModulePath(t *testing.T, goMod string) string {
	t.Helper()
	raw, err := os.ReadFile(goMod)
	if err != nil {
		t.Fatalf("读取 %s 失败：%v", goMod, err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "module ") {
			continue
		}
		mp := strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, "module ")), `"`)
		if mp != "" {
			return mp
		}
	}
	t.Fatalf("%s 中未找到有效的 module 行", goMod)
	return ""
}

// guardEnvOf 组装本次守卫的 module 环境。
//
// 唯一豁免子树**不写死** `pkg/agentkit`：它是「框架根包自身的 import 前缀」= module 路径 + cwd
// 相对 module 根的路径。仓库里 cwd = `<module>/pkg/agentkit` ⇒ 子树 = `<module>/pkg/agentkit`；
// 被抽取成 `<module>/agentkit` 后 ⇒ 子树 = `<module>/agentkit`。两者都按 框架裁决 的语义工作：
// 「宿主 module 前缀之下，只许框架自己的子树」。
func guardEnvOf(t *testing.T) guardEnv {
	t.Helper()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("获取包目录失败：%v", err)
	}
	goMod := guardFindGoMod(t)
	mp := guardModulePath(t, goMod)
	moduleRoot := filepath.Dir(goMod)
	rel, err := filepath.Rel(moduleRoot, cwd)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		t.Fatalf("框架根目录 %s 不在 module 根 %s 之内（rel=%q, err=%v）⇒ 守卫无法确定豁免子树", cwd, moduleRoot, rel, err)
	}
	subtree := mp
	if rel != "." {
		subtree = mp + "/" + filepath.ToSlash(rel)
	}
	return guardEnv{
		ModulePath: mp,
		GoMod:      goMod,
		GoSum:      filepath.Join(moduleRoot, "go.sum"),
		Root:       cwd,
		Subtree:    subtree,
	}
}

// guardFile 是一个被解析到的框架源文件。
type guardFile struct {
	Path string // 相对包目录的路径（斜杠分隔）
	Ast  *ast.File
}

// guardImport 是一次 import 的落点证据（文件:行 + 路径）。
type guardImport struct {
	File string
	Line int
	Path string
}

func (g guardImport) String() string {
	return g.File + ":" + strconv.Itoa(g.Line) + " import " + strconv.Quote(g.Path)
}

// guardParseGoFiles 以 ImportsOnly 模式解析 `pkg/agentkit/**` 的全部 .go 文件（含 _test.go）：
// 只看 import 列表的判据用它，成本最低。
func guardParseGoFiles(t *testing.T) (*token.FileSet, []guardFile) {
	t.Helper()
	return guardWalkGoFiles(t, parser.ImportsOnly)
}

// guardWalkGoFiles 用 go/parser 真解析框架内**全部** .go 文件（含 _test.go），按给定模式取 AST。
//
// 方向判据（框架裁决）必须看到**函数/接口签名与选择器表达式**，故传入 `parser.SkipObjectResolution`
// （完整解析但不做对象解析：足够用 ast.Inspect，且避免旧式对象解析的开销与坑）。
func guardWalkGoFiles(t *testing.T, mode parser.Mode) (*token.FileSet, []guardFile) {
	t.Helper()

	fset := token.NewFileSet()
	var files []guardFile
	err := filepath.WalkDir(".", func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			// 注意：根目录 "." 的 Name() 也是 "."，不能按「以点开头」跳过（否则扫描面为 0）。
			if path != "." && strings.HasPrefix(entry.Name(), ".") {
				return filepath.SkipDir
			}
			if entry.Name() == "vendor" || entry.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(entry.Name(), ".go") {
			return nil
		}
		file, perr := parser.ParseFile(fset, path, nil, mode)
		if perr != nil {
			return perr
		}
		files = append(files, guardFile{Path: filepath.ToSlash(path), Ast: file})
		return nil
	})
	if err != nil {
		t.Fatalf("扫描 pkg/agentkit/** 失败：%v", err)
	}
	return fset, files
}

// guardCollectImports 返回逐条 import 证据（按文件、行号排序）与扫描到的文件数。
func guardCollectImports(t *testing.T) ([]guardImport, int) {
	t.Helper()

	fset, files := guardParseGoFiles(t)
	var imports []guardImport
	for _, f := range files {
		for _, imp := range f.Ast.Imports {
			p, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				t.Fatalf("解析 %s 的 import 字面量失败：%v", f.Path, err)
			}
			imports = append(imports, guardImport{
				File: f.Path,
				Line: fset.Position(imp.Pos()).Line,
				Path: p,
			})
		}
	}
	sort.Slice(imports, func(i, j int) bool {
		if imports[i].File != imports[j].File {
			return imports[i].File < imports[j].File
		}
		return imports[i].Line < imports[j].Line
	})
	return imports, len(files)
}

// guardUnderModulePrefix 判 import 路径是否落在本 module 前缀之下（按**路径段**：
// module 自身，或 module + "/" 开头的路径）。
func guardUnderModulePrefix(modulePath, path string) bool {
	return path == modulePath || strings.HasPrefix(path, modulePath+"/")
}

// guardInsideFramework 判 import 路径是否位于框架**自己的子树**（subtree 或其子孙）之内。
// 必须按路径段判断：`.../pkg/agentkitfoo`、`.../pkg/agentkit_extra`、`.../agentkitmodel`
// **不算**子树内（朴素 HasPrefix 会误放行 —— 见 TestGuard_AgentkitImportClassification）。
// 非法路径段（空段 / "." / ".."）一律 fail-closed 判为「不在子树内」。
func guardInsideFramework(subtree, path string) bool {
	if path == subtree {
		return true
	}
	rest, ok := strings.CutPrefix(path, subtree)
	if !ok || !strings.HasPrefix(rest, "/") {
		return false
	}
	for _, seg := range strings.Split(strings.TrimPrefix(rest, "/"), "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

// guardIsStandardLibrary 用 go/build 的 GOROOT 归属判定标准库，
// **不用**「首段不含点号」这类启发式（会把 dotless 的第三方漏判为 stdlib）。
// guardIsConsumerWiring：该文件是否位于**消费方接线区**（`cmd/**` 下的 package main）。
// 独立布局下框架根 == 仓库根，取走实验放进去的样例 main 就落在 <root>/cmd/…；按样例自身
// 的头注释，它是「接入方」代码 —— 唯一允许自己实现出域构造点的位置，不受框架中立性管辖。
// 宿主布局下它压根不在框架子树内，所以这条判据此前从未被触发。
// ⚠️ 调用方必须同时断言「cmd/ 下只许出现 main 包」，否则这条豁免就是藏框架代码的洞。
func guardIsConsumerWiring(path string, file *ast.File) bool {
	return strings.HasPrefix(filepath.ToSlash(path), "cmd/") && file != nil && file.Name.Name == "main"
}

func guardIsStandardLibrary(t *testing.T, path string) bool {
	t.Helper()
	if build.Default.GOROOT == "" && runtime.GOROOT() == "" {
		t.Fatalf("无法确定 GOROOT，标准库判别器不成立（禁止退化为启发式）")
	}
	pkg, err := build.Default.Import(path, "", build.FindOnly)
	return err == nil && pkg.Goroot
}

// guardAssertStdlibDetectorWorks 是判别器的自检：判别器一旦失效（GOROOT 丢失、被换成启发式），
// 守卫必须**响亮失败**而不是静默放行。
func guardAssertStdlibDetectorWorks(t *testing.T, env guardEnv) {
	t.Helper()
	for _, p := range []string{"net/http", "net/http/httptest", "context", "go/ast", "path/filepath", "strings"} {
		if !guardIsStandardLibrary(t, p) {
			t.Fatalf("标准库判别器失效：%q 未被判为标准库", p)
		}
	}
	for _, p := range []string{"golang.org/x/sync/errgroup", env.Subtree + "/model"} {
		if guardIsStandardLibrary(t, p) {
			t.Fatalf("标准库判别器失效：非标准库 %q 被判为标准库（会漏判第三方依赖）", p)
		}
	}
}

// ---------- 1. 零业务依赖（框架裁决 判定式：module 前缀之下只许 pkg/agentkit 子树） ----------

func TestGuard_AgentkitNoHostModuleImport(t *testing.T) {
	env := guardEnvOf(t)
	imports, files := guardCollectImports(t)

	// 常驻非空转断言：扫描面必须真的覆盖整个框架，且「module 前缀」分支确实被走到。
	if files < 20 {
		t.Fatalf("只扫描到 %d 个 .go 文件，扫描面可疑（<20）", files)
	}
	if len(imports) < 20 {
		t.Fatalf("只解析到 %d 条 import，扫描面可疑（<20）", len(imports))
	}
	prefixHits, exemptHits := 0, 0
	for _, imp := range imports {
		if guardUnderModulePrefix(env.ModulePath, imp.Path) {
			prefixHits++
		}
		if guardInsideFramework(env.Subtree, imp.Path) {
			exemptHits++
		}
	}
	if prefixHits == 0 || exemptHits == 0 {
		t.Fatalf("豁免分支从未被走到（module 前缀 import=%d、pkg/agentkit 子树 import=%d）⇒ 守卫空转", prefixHits, exemptHits)
	}

	var violations []guardImport
	var internal []string
	for _, imp := range imports {
		if !guardUnderModulePrefix(env.ModulePath, imp.Path) {
			continue
		}
		if guardInsideFramework(env.Subtree, imp.Path) {
			internal = append(internal, imp.Path)
			continue
		}
		violations = append(violations, imp)
	}

	for _, v := range violations {
		t.Errorf("违规 import：%s —— 框架不得依赖宿主业务实现（框架裁决 第 2 条；module 前缀之下只豁免 %s/ 子树）", v, env.Subtree)
	}
	if len(violations) > 0 {
		t.Fatalf("零业务依赖守卫失败：%d 条违规（见上）", len(violations))
	}

	t.Logf("%s；扫描 %d 个 .go 文件 / %d 条 import；module 前缀命中 %d 条，全部位于豁免子树 %s 之内", env.describe(), files, len(imports), prefixHits, env.Subtree)
	for _, p := range guardUniqSorted(internal) {
		t.Logf("  合法框架内部 import：%s", p)
	}
}

// ---------- 2. 零第三方依赖（白名单只有 errgroup） ----------

func TestGuard_AgentkitNoThirdPartyDeps(t *testing.T) {
	env := guardEnvOf(t)
	guardAssertStdlibDetectorWorks(t, env)
	imports, files := guardCollectImports(t)
	if files == 0 {
		t.Fatal("未扫描到任何 .go 文件 ⇒ 守卫空转")
	}

	var violations []guardImport
	thirdParty := map[string]bool{}
	for _, imp := range imports {
		if guardUnderModulePrefix(env.ModulePath, imp.Path) {
			continue // module 前缀依赖由 TestGuard_AgentkitNoHostModuleImport 负责
		}
		if guardIsStandardLibrary(t, imp.Path) {
			continue
		}
		thirdParty[imp.Path] = true
		if !guardSanctioned(imp.Path) {
			violations = append(violations, imp)
		}
	}

	for _, v := range violations {
		t.Errorf("违规 import：%s —— 框架只允许标准库 + 已登记第三方（白名单：%v；框架裁决 第 3 条）", v, guardWhiteList())
	}
	if len(violations) > 0 {
		t.Fatalf("零第三方依赖守卫失败：%d 条违规（见上）", len(violations))
	}

	t.Logf("标准库之外的 import 集合 = %v（白名单 %v）", guardUniqSorted(guardKeys(thirdParty)), guardWhiteList())
}

// ---------- 3. 单一出口基线（按符号方向判定 —— 框架裁决 取代原「按路径判定」） ----------

// guardHTTPDefaultName 是「无别名 import 时的包名」：httputil 的包名不是 net/http 截出来的。
var guardHTTPDefaultName = map[string]string{
	"net/http":          "http",
	"net/http/httputil": "httputil",
}

// guardHTTPAliases 解析一个文件里绑定到 net/http（及 net/http/httputil）的**局部名字** → 导入路径。
//
// 必须按别名解析：`import h "net/http"` 会让所有选择器前缀变成 `h.`，按字面 "http" 匹配将**全部漏检**。
// 点导入（`.`）使方向不可判（裸标识符无前缀），空白导入（`_`）在 net/http 上没有任何合法用途
// （该包无 init 副作用可依赖）⇒ 两者都以特殊键回报给调用方判红。
func guardHTTPAliases(f guardFile) map[string]string {
	out := map[string]string{}
	for _, imp := range f.Ast.Imports {
		p, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		def, tracked := guardHTTPDefaultName[p]
		if !tracked {
			continue
		}
		if imp.Name == nil {
			out[def] = p
			continue
		}
		switch imp.Name.Name {
		case "_":
			out["_"] = p
		case ".":
			out["."] = p
		default:
			out[imp.Name.Name] = p
		}
	}
	return out
}

// guardResultsHoldHTTPResponse 判函数/方法的结果里是否出现 `*http.Response`。
//
// 这是注入式 Doer 旁路的**形状特征**：服务端处理器永远不会返回 `*http.Response`，
// 而「拿一个注入进来的 Doer 自己发请求」必然返回它 —— 且该动作可以完全不出现任何 D1 符号，
// 旧判据（只看 import 路径）对此完全无感（框架裁决 第 3 条）。
func guardResultsHoldHTTPResponse(results *ast.FieldList) bool {
	if results == nil {
		return false
	}
	found := false
	for _, fl := range results.List {
		ast.Inspect(fl.Type, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if id, ok := sel.X.(*ast.Ident); ok && guardHTTPDefaultName["net/http"] == id.Name && sel.Sel.Name == "Response" {
				found = true
			}
			return true
		})
	}
	return found
}

// TestGuard_NoDirectLLMCall 是框架侧唯一出口基线（框架裁决 决策 1 的机械判据）。
//
// 判据已由「路径唯一」升级为「符号方向四档」（框架裁决）：
//
//	D1 出域符号（Client/NewRequest/Transport/Get/Post/…）所在包集合 **== {model}**（等号，比原 ⊆ 更严）
//	D2 入域符号（Server/Handler/ResponseWriter/…）所在包集合 **⊆ {web}**
//	D3 中立符号（Request/Response/Header/Status*/Method*/…）所在包集合 **⊆ {model, web}**（中立 ≠ 自由）
//	D4 未归类符号 ⇒ 逐条违规（强制显式归类，禁止靠「再放开一个包」消红）
//
//	外加旁路封堵：非 model 包引用 HTTPDoer，或函数/方法结果出现 *http.Response ⇒ 违规。
//
// 三条断言各自带反空转：D1 集合必须非空；web 目录已存在时 D2 集合必须非空（否则判红，既有教训）。
func TestGuard_NoDirectLLMCall(t *testing.T) {
	fset, files := guardWalkGoFiles(t, parser.SkipObjectResolution)

	type usage struct {
		dir  guardHTTPDir
		pkg  string
		file string
		line int
		sym  string
	}
	var (
		usages   []usage
		total    int
		imported map[string]bool = map[string]bool{} // 非测试文件中 import net/http 的包（旧口径，仍要报出来做对照）
	)
	for _, f := range files {
		if strings.HasSuffix(f.Path, "_test.go") {
			continue
		}
		// 消费方接线区豁免（布局差异的必然结果，不是后门）：独立布局下框架根 == 仓库根，
		// 样例 main 就落在 <root>/cmd/agentkit-demo —— 它是**接入方**代码（样例头注释写明它
		// 是唯一允许自己实现模型请求构造点的位置），不在框架中立性的管辖范围内；
		// 宿主布局下它压根不在框架子树内，所以这条判据此前从没被触发过。
		// 豁免带牙齿：cmd/ 下出现非 package main 的文件 ⇒ 判违规，防止把框架代码藏进消费方目录躲守卫。
		if strings.HasPrefix(filepath.ToSlash(f.Path), "cmd/") {
			if f.Ast.Name.Name != "main" {
				t.Errorf("违规（豁免被滥用）：%s 位于 cmd/ 却不是 package main ⇒ 框架代码不得藏进消费方接线区", f.Path)
			}
			continue
		}
		total++
		pkg := filepath.ToSlash(filepath.Dir(f.Path))
		aliases := guardHTTPAliases(f)
		if _, bad := aliases["."]; bad {
			t.Errorf("违规：%s 点导入 net/http（或 httputil）⇒ 选择器失去前缀、方向不可判；框架内禁止点导入", f.Path)
		}
		if _, bad := aliases["_"]; bad {
			t.Errorf("违规：%s 空白导入 net/http（或 httputil）⇒ 该包无 init 副作用可用，属夹带", f.Path)
		}
		if len(aliases) == 0 {
			continue
		}
		imported[pkg] = true
		ast.Inspect(f.Ast, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			id, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			importPath, bound := aliases[id.Name]
			if !bound {
				return true
			}
			usages = append(usages, usage{
				dir:  guardClassifyHTTP(importPath, sel.Sel.Name),
				pkg:  pkg,
				file: f.Path,
				line: fset.Position(sel.Pos()).Line,
				sym:  id.Name + "." + sel.Sel.Name,
			})
			return true
		})
	}
	if total == 0 {
		t.Fatal("未扫描到任何非测试 .go 文件 ⇒ 守卫空转")
	}

	byDir := map[guardHTTPDir]map[string][]usage{}
	for _, u := range usages {
		if byDir[u.dir] == nil {
			byDir[u.dir] = map[string][]usage{}
		}
		byDir[u.dir][u.pkg] = append(byDir[u.dir][u.pkg], u)
	}
	pkgsOf := func(d guardHTTPDir) []string {
		out := make([]string, 0, len(byDir[d]))
		for p := range byDir[d] {
			out = append(out, p)
		}
		sort.Strings(out)
		return out
	}

	// 全部判据一次跑完再统一报红：中途 t.Fatal 会**掩盖**后面几档的违规——
	// 实测踩过：一处 D1 出域违规的红，吞掉了 D2/D3/旁路三档的全部诊断（反证重跑时才暴露）。
	var problems []string

	// ---- D4：未归类即违规（逐条给文件:行与符号名，便于直接定位）----
	for _, u := range usages {
		if u.dir == guardHTTPUnknown {
			problems = append(problems, fmt.Sprintf("违规（D4 未归类）：%s:%d 使用 %s —— 既不在出域表也不在入域表也不在中立表，必须显式归类（框架裁决）",
				u.file, u.line, u.sym))
		}
	}

	// ---- D1：出域集合必须等于 {model}（等号）----
	egress := pkgsOf(guardHTTPEgress)
	for _, p := range egress {
		if !guardIn(p, guardEgressPkgs) {
			problems = append(problems, fmt.Sprintf("违规（D1 出域）：框架根相对目录 %q 具备自行发请求的能力 —— 框架内唯一出域实现是 %q（既有教训/既有教训/框架裁决）", p, guardEgressPkg))
		}
	}
	for _, want := range guardEgressPkgs {
		if !guardIn(want, egress) {
			problems = append(problems, fmt.Sprintf("违规（D1 等号强度）：允许出域的包 %q 实际未出现任何出域符号 ⇒ 出口基线正在退化或被搬走", want))
		}
	}
	if len(egress) == 0 {
		problems = append(problems, "违规（空转）：net/http 出域符号所在包集合为空 ⇒ 断言退化为空集包含，守卫空转")
	}

	// ---- D2：入域集合 ⊆ {web}；web 已存在时不得为空 ----
	ingress := pkgsOf(guardHTTPIngress)
	for _, p := range ingress {
		if !guardIn(p, guardIngressPkgs) {
			problems = append(problems, fmt.Sprintf("违规（D2 入域）：框架根相对目录 %q 出现服务端符号 —— 入域能力只允许在 %v 之内（框架裁决）", p, guardIngressPkgs))
		}
	}
	if _, statErr := os.Stat("web"); statErr == nil {
		if len(ingress) == 0 {
			problems = append(problems, "违规（空转）：web/ 目录已存在却没有任何入域符号 ⇒ 入域判据空转（既有教训）")
		}
	} else {
		t.Log("入域判据当前无上界压力：web/ 尚未落地（其牙齿由 TestGuard_HTTPDirectionClassification 的合成用例表承担）")
	}

	// ---- D3：中立集合 ⊆ {model, web} ----
	for _, p := range pkgsOf(guardHTTPNeutral) {
		if !guardIn(p, guardNeutralPkgs) {
			problems = append(problems, fmt.Sprintf("违规（D3 中立）：框架根相对目录 %q 使用 HTTP 报文类型/常量 —— 中立不等于自由，只允许 %v（框架裁决）", p, guardNeutralPkgs))
		}
	}

	// ---- 旁路封堵：非 model 包不得引用 HTTPDoer，也不得让函数/方法返回 *http.Response ----
	for _, f := range files {
		if strings.HasSuffix(f.Path, "_test.go") {
			continue
		}
		if guardIsConsumerWiring(f.Path, f.Ast) { // 消费方接线区：见 helper 注释的牙齿条款
			continue
		}
		pkg := filepath.ToSlash(filepath.Dir(f.Path))
		if guardIn(pkg, guardEgressPkgs) {
			continue
		}
		ast.Inspect(f.Ast, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.Ident:
				if x.Name == "HTTPDoer" {
					problems = append(problems, fmt.Sprintf("违规（旁路）：%s:%d 在非出域包 %q 引用出域传输类型 HTTPDoer ⇒ 可绕开唯一出口自行发请求（框架裁决第 3 条）",
						f.Path, fset.Position(x.Pos()).Line, pkg))
				}
			case *ast.FuncDecl:
				if guardResultsHoldHTTPResponse(x.Type.Results) {
					problems = append(problems, fmt.Sprintf("违规（旁路）：%s:%d 函数 %s 在包 %q 返回 *http.Response ⇒ 具备出域调用面，唯一出口在 %q",
						f.Path, fset.Position(x.Pos()).Line, x.Name.Name, pkg, guardEgressPkg))
				}
			case *ast.InterfaceType:
				for _, m := range x.Methods.List {
					if ft, ok := m.Type.(*ast.FuncType); ok && guardResultsHoldHTTPResponse(ft.Results) {
						name := "(匿名接口方法)"
						if len(m.Names) > 0 {
							name = m.Names[0].Name
						}
						problems = append(problems, fmt.Sprintf("违规（旁路）：%s:%d 接口 %s 在包 %q 声明返回 *http.Response 的方法 ⇒ 这是第二个出域 ABI",
							f.Path, fset.Position(m.Pos()).Line, name, pkg))
					}
				}
			}
			return true
		})
	}

	// ---- 统一报红：所有档位的违规一次看全 ----
	for _, p := range problems {
		t.Error(p)
	}
	if len(problems) > 0 {
		t.Fatalf("单一出口/方向基线守卫失败：%d 条违规（全部已列出，见上）", len(problems))
	}

	t.Logf("方向证据（非测试文件，框架根相对路径）：D1 出域=%v（等号基线 %v）D2 入域=%v D3 中立=%v D4 未归类=%d 条",
		egress, guardEgressPkgs, ingress, pkgsOf(guardHTTPNeutral), len(byDir[guardHTTPUnknown]))
	t.Logf("旧口径对照（仅供人工比对，不再是判据）：import net/http 的包 = %v；扫描非测试文件 %d 个", guardUniqSorted(guardKeys(imported)), total)
}

// TestGuard_HTTPDirectionClassification 是分类器的表驱动反证：不依赖仓库现状，
// 直接把四类方向与「未归类即拒」钉死（框架裁决 的 Consequences 反证 1~6 中，凡是需要改仓库文件的
// 那条都在 P0-2 的手脚实证里做，其余都在这里常驻）。
func TestGuard_HTTPDirectionClassification(t *testing.T) {
	cases := []struct {
		pkgPath string
		symbol  string
		want    guardHTTPDir
		why     string
	}{
		{"net/http", "Client", guardHTTPEgress, "能自行发请求"},
		{"net/http", "NewRequestWithContext", guardHTTPEgress, "能自行构造出域请求"},
		{"net/http", "Transport", guardHTTPEgress, "可绕过端点白名单"},
		{"net/http", "Get", guardHTTPEgress, "包级快捷出域"},
		{"net/http/httputil", "ReverseProxy", guardHTTPEgress, "以 Handler 面目出现，但确实在向外发请求"},
		{"net/http", "HandlerFunc", guardHTTPIngress, "纯服务端量"},
		{"net/http", "ResponseWriter", guardHTTPIngress, "纯服务端量"},
		{"net/http", "ListenAndServe", guardHTTPIngress, "纯服务端量"},
		{"net/http", "Request", guardHTTPNeutral, "双向数据类型：model 的出域签名也要它"},
		{"net/http", "Response", guardHTTPNeutral, "同上（旁路另有专门判据堵）"},
		{"net/http", "StatusTooManyRequests", guardHTTPNeutral, "常量族"},
		{"net/http", "MethodPost", guardHTTPNeutral, "常量族"},
		{"net/http", "NoSuchThing", guardHTTPUnknown, "未登记符号必须判违规，不得静默放行"},
		{"net/http/httputil", "ChunkWriter", guardHTTPIngress, "读入站分块体"},
		{"net/http/httputil", "Whatever", guardHTTPUnknown, "httputil 同样不放开未知符号"},
		{"net/http2", "Client", guardHTTPUnknown, "非跟踪路径不参与方向判定"},
	}
	for _, c := range cases {
		if got := guardClassifyHTTP(c.pkgPath, c.symbol); got != c.want {
			t.Errorf("分类错误：%s.%s ⇒ 得 %s，期望 %s（%s）", c.pkgPath, c.symbol, got, c.want, c.why)
		}
	}
	// 反空转：三档都必须有成员，否则「集合为空」会让上界断言退化为永真。
	if len(guardHTTPEgressSymbols) == 0 || len(guardHTTPIngressSymbols) == 0 || len(guardHTTPNeutralSymbols) == 0 {
		t.Fatal("方向符号表存在空档 ⇒ 判据退化为空集包含，守卫空转")
	}
	// 互斥性：同一符号不得同时属于两档（否则可用「换个档」绕过等号基线）。
	for s := range guardHTTPEgressSymbols {
		if guardHTTPIngressSymbols[s] || guardHTTPNeutralSymbols[s] || guardHTTPUtilSymbolsEgress[s] && false {
			t.Errorf("符号 %q 同时登记在出域与入域/中立表 ⇒ 方向判定可被规避", s)
		}
	}
}

// ---------- 4. go.mod 零改动基线 ----------

func TestGuard_AgentkitGoModContract(t *testing.T) {
	env := guardEnvOf(t)
	guardAssertStdlibDetectorWorks(t, env)

	raw, err := os.ReadFile(env.GoMod)
	if err != nil {
		t.Fatalf("读取 %s 失败：%v", env.GoMod, err)
	}
	goMod := string(raw)
	if !strings.Contains(goMod, "module "+env.ModulePath) {
		t.Fatalf("%s 中缺少 %q —— 框架所在 module 与守卫口径不一致", env.GoMod, "module "+env.ModulePath)
	}

	// 白名单里的每个模块都必须已是**既有** require（不是本任务新增依赖）。
	for _, mod := range guardSanctionedThirdParty {
		if !guardGoModRequires(goMod, mod) {
			t.Fatalf("白名单模块 %q 不在 %s 的 require 中 ⇒ 白名单等于新增依赖，违反 框架裁决", mod, env.GoMod)
		}
	}
	sum, err := os.ReadFile(env.GoSum)
	if err != nil {
		t.Fatalf("读取 %s 失败：%v", env.GoSum, err)
	}
	for _, mod := range guardSanctionedThirdParty {
		if !strings.Contains(string(sum), mod+" v") {
			t.Fatalf("白名单模块 %q 不在 %s 中（模块内容不可用）", mod, env.GoSum)
		}
	}

	// 框架实际用到的非 stdlib / 非 module 前缀 import 集合必须 ⊆ 白名单。
	imports, _ := guardCollectImports(t)
	used := map[string]bool{}
	for _, imp := range imports {
		if guardUnderModulePrefix(env.ModulePath, imp.Path) || guardIsStandardLibrary(t, imp.Path) {
			continue
		}
		used[imp.Path] = true
	}
	for p := range used {
		if !guardSanctioned(p) {
			t.Fatalf("框架引入了未登记依赖 %q ⇒ go.mod 零改动基线被破坏（白名单 %v）", p, guardWhiteList())
		}
	}
	t.Logf("go.mod 零改动基线：module=%s；框架非 stdlib 依赖 = %v ⊆ 白名单 %v；白名单模块均已在既有 go.mod/go.sum 中",
		env.ModulePath, guardUniqSorted(guardKeys(used)), guardWhiteList())
}

// guardGoModRequires 判 go.mod 文本里是否已有 mod 的 require（支持单行与 require 块）。
func guardGoModRequires(goMod, mod string) bool {
	for _, line := range strings.Split(goMod, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "//") || strings.HasPrefix(line, "require (") || line == ")" {
			continue
		}
		fields := strings.Fields(strings.TrimPrefix(line, "require "))
		if len(fields) >= 1 && fields[0] == mod {
			return true
		}
	}
	return false
}

// ---------- 5. 路径段判定（HasPrefix 误判的常驻反例） ----------

// TestGuard_AgentkitImportClassification 固化「按路径段判断」本身：
//   - 在**仓库布局**（框架位于 <module>/pkg/agentkit）与**抽取布局**（<module>/agentkit）下
//     逐条判对；相似前缀的合成路径（agentkitfoo / agentkit_extra / agentkitmodel）必须被判违规
//     —— 朴素 HasPrefix 会误放行（实证见 交付纪要 探针 B 的反事实对照）。
func TestGuard_AgentkitImportClassification(t *testing.T) {
	const host = "example.com/hostmod"
	const repoSubtree = host + "/pkg/agentkit"
	const extractedSubtree = host + "/agentkit"

	cases := []struct {
		subtree string
		path    string
		inside  bool
		reason  string
	}{
		// 仓库布局
		{repoSubtree, repoSubtree, true, "框架根包（子树根）"},
		{repoSubtree, repoSubtree + "/model", true, "框架内部包"},
		{repoSubtree, repoSubtree + "/agent", true, "框架内部包（深层）"},
		{repoSubtree, host + "/pkg/agentkitfoo", false, "相似前缀：不是 pkg/agentkit 子树"},
		{repoSubtree, host + "/pkg/agentkitfoo/service", false, "相似前缀：agentkitfoo ≠ agentkit"},
		{repoSubtree, host + "/pkg/agentkit_extra", false, "相似前缀：agentkit_extra ≠ agentkit"},
		{repoSubtree, host + "/pkg/agentkitmodel", false, "相似前缀：agentkitmodel ≠ agentkit"},
		{repoSubtree, host + "/pkg/agentkit/", false, "尾斜杠空段：fail-closed"},
		{repoSubtree, host + "/pkg/agentkit/../logic", false, "带 .. 的段：fail-closed"},
		{repoSubtree, host + "/pkg/cloudweb/magweb/defined", false, "宿主非框架包（业务实现）"},
		{repoSubtree, host + "/logic/engine/llm", false, "宿主非框架包（业务实现）"},
		{repoSubtree, host + "/controllers/engine", false, "宿主非框架包（业务实现）"},
		{repoSubtree, host + "/config", false, "宿主非框架包"},
		{repoSubtree, host + "/errorx", false, "宿主非框架包"},
		{repoSubtree, host + "/pkg", false, "pkg 段下但不是 agentkit"},
		{repoSubtree, host, false, "module 自身不是框架子树"},
		{repoSubtree, "golang.org/x/sync/errgroup", false, "第三方：不在 module 前缀之下"},
		// 抽取布局（框架位于 <module>/agentkit）
		{extractedSubtree, extractedSubtree, true, "抽取后：框架根包"},
		{extractedSubtree, extractedSubtree + "/model", true, "抽取后：框架内部包"},
		{extractedSubtree, host + "/agentkitfoo", false, "抽取后：agentkitfoo ≠ agentkit"},
		{extractedSubtree, host + "/agentkit_extra", false, "抽取后：agentkit_extra ≠ agentkit"},
		{extractedSubtree, host + "/logic", false, "抽取后：宿主非框架包"},
		{extractedSubtree, host + "/pkg/agentkit/model", false, "抽取后：仓库布局路径已不再是框架子树"},
	}
	for _, tc := range cases {
		t.Run(tc.subtree+"|"+tc.path, func(t *testing.T) {
			if got := guardInsideFramework(tc.subtree, tc.path); got != tc.inside {
				t.Fatalf("guardInsideFramework(%q, %q) = %v，want %v（%s）", tc.subtree, tc.path, got, tc.inside, tc.reason)
			}
		})
	}

	// module 前缀判定（规则 1 的第一道闸）单独固化。
	for _, tc := range []struct {
		path   string
		expect bool
	}{
		{host, true},
		{host + "/pkg/agentkit", true},
		{host + "/anything", true},
		{host + "x", false},
		{"golang.org/x/sync/errgroup", false},
		{"net/http", false},
	} {
		if got := guardUnderModulePrefix(host, tc.path); got != tc.expect {
			t.Fatalf("guardUnderModulePrefix(%q, %q) = %v，want %v", host, tc.path, got, tc.expect)
		}
	}
}

// ---------- 小工具 ----------

// guardIn 判 s 是否在 list 中（方向基线的允许集合都很小，线性扫描即可，不必建 map 副本）。
func guardIn(s string, list []string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func guardKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func guardWhiteList() []string {
	out := make([]string, 0, len(guardSanctionedThirdParty))
	for k := range guardSanctionedThirdParty {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func guardUniqSorted(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
