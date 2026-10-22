package kitctx

import (
	"context"
	"time"
)

// Injections 承载一次请求的注入点。
//
// 宿主应用在入口处构造：宿主侧在 宿主接入层 内以 **per-request 闭包/包装器**
// 捕获 宿主框架上下文类型 后注入（框架裁决），因此本包与整个 pkg/agentkit 都不依赖宿主类型。
//
// 注入点是活对象，**不随请求序列化**（不参与上下文回显）。
//
// Masker 契约不在本包定义：脱敏证据的类型与调用点由 pkg/agentkit/security 拥有。
// 本包在此只登记其签名约束 —— **只允许使用 stdlib context.Context**，不得出现 宿主框架上下文类型：
//
//	func(ctx context.Context, sign string, data any) (masked any, applied []string, err error)
//
// 同理，HTTPDoer / EntityParser / Transport 等其它注入接口的实现一律不得反向依赖宿主应用。
type Injections struct {
	Clock  Clock  `json:"-"`
	Logger Logger `json:"-"`
}

// Clock 是可注入时钟。
//
// 一切与时间相关的判定（耗时、限流窗口）都经它取值，使行为可在测试中确定性复现。
type Clock interface {
	Now() time.Time
}

// systemClock 是 Clock 的兜底实现：真实系统时钟。
type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

// SystemClock 返回真实系统时钟；未注入时钟时的兜底实现。
func SystemClock() Clock { return systemClock{} }

// Logger 是结构化日志注入点。
//
// 方法名沿用宿主项目既有日志风格（Debug/Info/Warn/Error），字段为 key-value 映射，
// 便于 宿主侧适配到既有 宿主日志库。
type Logger interface {
	Debug(msg string, fields map[string]any)
	Info(msg string, fields map[string]any)
	Warn(msg string, fields map[string]any)
	Error(msg string, fields map[string]any)
}

// nopLogger 丢弃全部日志；用于「未注入」与测试。
type nopLogger struct{}

func (nopLogger) Debug(string, map[string]any) {}
func (nopLogger) Info(string, map[string]any)  {}
func (nopLogger) Warn(string, map[string]any)  {}
func (nopLogger) Error(string, map[string]any) {}

// NopLogger 返回丢弃全部日志的 Logger。
func NopLogger() Logger { return nopLogger{} }

// ClockFrom 取出本请求的注入时钟；未注入或 ctx 为空时返回系统时钟。
//
// **绝不返回 nil**，调用方无需判空。
func ClockFrom(ctx context.Context) Clock {
	if rc, ok := RequestFrom(ctx); ok && rc.Injections.Clock != nil {
		return rc.Injections.Clock
	}
	return systemClock{}
}

// LoggerFrom 取出本请求的注入日志；未注入或 ctx 为空时返回丢弃实现。
//
// **绝不返回 nil**，调用方无需判空。
func LoggerFrom(ctx context.Context) Logger {
	if rc, ok := RequestFrom(ctx); ok && rc.Injections.Logger != nil {
		return rc.Injections.Logger
	}
	return nopLogger{}
}
