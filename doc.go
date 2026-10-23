// Package agentkit 是可跨 Go 程序搬运的「语义理解 + 模型出域 + 工具调用 + IM 接入」框架。
//
// 框架只依赖 Go 标准库（外加既有 `// indirect` 的 golang.org/x/sync/errgroup，见
// README.md 的依赖口径），通过注入式接口（Logger / Masker / Clock / HTTPDoer /
// EntityParser / Transport / Responder / Directory）与宿主程序解耦。
//
// 零业务依赖的口径见 .specs/adr/010-agentkit-internal-imports-and-extraction.md：
// 框架内部各包（kitctx / model / security / semantic / tools / agent / im）可以互相
// import，但**不得**落在 module 前缀之下 `agentkit/` 子树以外的任何位置
// （宿主的业务与接入目录）。
// 该判定式由本目录的 guard_test.go 机械固化：守卫从最近的 go.mod 读 module 路径，
// 并以**框架自身所在目录**推导豁免子树 ⇒ 整目录抽取到别的 module 后自动适配，
// 不需要改守卫代码。抽取方式（整目录复制 + 一次 import 前缀重写）见 README.md。
//
// 本文件只承载包文档，不含任何逻辑：它同时保证 `agentkit` 根包在
// `go build $(go list ./... | grep -v 'stone/example')` 下可构建（该目录下 guard_test.go
// 是唯一的 _test.go 文件，仅测试文件的目录会让 go build 报 “no non-test Go files”）。
package agentkit
