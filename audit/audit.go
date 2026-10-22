// Package audit 是**行为审计的投递面**：把一条事实（谁、对什么、做了什么、结果如何）
// 先脱敏、再多播给若干 sink（日志、库、告警……由宿主实现）。
//
// 与 `既有模型层` 的出域审计（HMAC 证据、可验证链）不是同一件事：那是**出域动作**的
// 专用凭据，本包是**任意行为**的通用投递。二者共用 `security.Masker` 这一唯一脱敏来源。
//
// 三条硬规则（缺一条就会写出"看起来有审计、实际什么都证明不了"的代码）：
//
//  1. **未脱敏的条目禁止入 sink**。投递只有 `Fanout.Emit` 一条路；它给条目盖一个**未导出**
//     凭据（proof），`Entry.Valid` 只认这个凭据。第三方 sink 可以用 `RequireMasked` 包住，
//     这样"绕过 Fanout 直接调 sink"在运行期被拒，而不是靠文档自觉。
//     （为什么不能只靠约定：审计存在的意义就是出事时能自证清白，一条原样入 sink 的载荷
//     就是合规事故本身。）
//  2. **`applied` 只能是"实际被移除或改写的字段路径"**，且只能由 `security.AppliedEvidence`
//     这类输入/输出比对得出（`security/masker.go` 已把这条写死）。因此"数据非空但什么都没改"
//     的空转态必然表现为 `applied` 为空。
//  3. **"已脱敏"是一句需要证据的话，不是一个可以被填写的字段**。`Sanitized` 由
//     proof + `len(applied)>0` 派生，外部无从伪造，也无从"声明自己已脱敏"。
//     注意 proof 与 applied 回答的是两个问题：前者"走过脱敏通道吗"，后者"真的改掉了字段吗"；
//     空 applied 的条目合法（本就无敏感字段），但不得被报告为"已脱敏"。
package audit

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/malizhi2202-dev/agentkit/kitctx"
	"github.com/malizhi2202-dev/agentkit/security"
)

// 稳定错误 token（供宿主 grep 与告警映射，不靠错误文本判定）。
var (
	// ErrMaskerRequired 表示没有注入脱敏函数：没有它，本包唯一的安全属性就不成立。
	ErrMaskerRequired = errors.New("audit: 必须注入 security.Masker（未脱敏不得入 sink）")
	// ErrNoSink 表示 sink 集合为空：投递到 0 个目的地是空转，不该被建成一个能用的对象。
	ErrNoSink = errors.New("audit: 至少需要一个 sink（空 sink 集等于静默丢弃）")
	// ErrSinkNil 表示列表里有 nil sink。
	ErrSinkNil = errors.New("audit: sink 不得为 nil")
	// ErrActionRequired 表示条目没有 action 稳定 token。
	ErrActionRequired = errors.New("audit: 缺少 action 稳定标识")
	// ErrOutcomeUnknown 表示 outcome 不在闭集内（含留空：留空等于"没人决定过结果"）。
	ErrOutcomeUnknown = errors.New("audit: outcome 不在闭集内（ok/denied/error）")
	// ErrNotMasked 表示条目未经 Fanout 脱敏 ⇒ 禁止入 sink。
	ErrNotMasked = errors.New("audit: 条目未经脱敏，禁止入 sink")
	// ErrMaskFailed 表示脱敏本身失败 ⇒ 该条**不投递**（fail-closed，宁可不落也不落原样）。
	ErrMaskFailed = errors.New("audit: 脱敏失败，条目未投递")
)

// Outcome 是行为结果闭集。留空不是一个合法值：没有结果的审计等于没记。
type Outcome string

const (
	OutcomeOK     Outcome = "ok"
	OutcomeDenied Outcome = "denied"
	OutcomeError  Outcome = "error"
)

// Valid 报告结果是否在闭集内。
func (o Outcome) Valid() bool {
	switch o {
	case OutcomeOK, OutcomeDenied, OutcomeError:
		return true
	}
	return false
}

// Entry 是一条行为审计事实。
//
// 载荷一律放 Fields（任何结构都行，Fanout 负责脱敏）；身份/链路字段在 Emit 时若为空，
// 会从 ctx 里取（`kitctx.RequestFrom`），显式值优先 —— 这样调用方可以只填关心的部分。
type Entry struct {
	Time    time.Time      `json:"time"`
	TraceID string         `json:"trace_id,omitempty"`
	UserID  string         `json:"user_id,omitempty"`
	Role    string         `json:"role,omitempty"`
	Sign    string         `json:"sign,omitempty"` // 交给 Masker 的主体标识（选哪套规则）
	Action  string         `json:"action"`         // 稳定 token，供 grep/告警
	Target  string         `json:"target,omitempty"`
	Outcome Outcome        `json:"outcome"`
	Fields  map[string]any `json:"fields,omitempty"`

	// Applied 是脱敏证据（实际被移除/改写的字段路径），由 Fanout 盖章，外部无从填写。
	Applied []string `json:"applied,omitempty"`

	// Err 记录失败原因（仅 Outcome=error 时有意义）。不参与序列化，也不进 sink 载荷。
	Err error `json:"-"`

	// proof 是本包的未导出凭据：只有走过 Fanout 的条目才有 ⇒ 外部无法伪造"已脱敏"。
	proof *seal
}

type seal struct{}

// Sanitized 派生"这条确实被脱掉了字段"的结论：必须同时满足走过脱敏通道与实际改写过。
func (e Entry) Sanitized() bool { return e.proof != nil && len(e.Applied) > 0 }

// Masked 报告"走过脱敏通道"（与 Sanitized 不同：空 applied 的合法条目也是 true）。
func (e Entry) Masked() bool { return e.proof != nil }

// validate 是条目合法性的唯一判据处（既有教训：同一条不变量不写两遍）。
// needProof=false 用于 Fanout.Emit 的入参预检（凭据此刻还没盖上）。
func (e Entry) validate(needProof bool) error {
	if strings.TrimSpace(e.Action) == "" {
		return ErrActionRequired
	}
	if !e.Outcome.Valid() {
		return fmt.Errorf("%w：实得 %q", ErrOutcomeUnknown, string(e.Outcome))
	}
	if needProof && e.proof == nil {
		return ErrNotMasked
	}
	return nil
}

// Valid 判定条目能否入 sink：必须字段齐备**且**走过脱敏通道。
func (e Entry) Valid() error { return e.validate(true) }

// Clone 返回条目的浅拷贝副本（Fields 与 Applied 复制，避免 sink 之间互相看到改写）。
func (e Entry) Clone() Entry {
	out := e
	if e.Fields != nil {
		out.Fields = make(map[string]any, len(e.Fields))
		for k, v := range e.Fields {
			out.Fields[k] = v
		}
	}
	if e.Applied != nil {
		out.Applied = append([]string(nil), e.Applied...)
	}
	return out
}

// ---- Sink ----

// Sink 是审计投递目的地。实现必须无副作用地快速返回；阻塞与重试属宿主自己的缓冲层。
type Sink interface {
	Emit(ctx context.Context, e Entry) error
}

// SinkFunc 让宿主一行接入（`audit.SinkFunc(func(ctx, e) error { ... })`）。
type SinkFunc func(ctx context.Context, e Entry) error

// Emit 实现 Sink。
func (f SinkFunc) Emit(ctx context.Context, e Entry) error { return f(ctx, e) }

// Nop 返回丢弃一切的 sink（测试与"审计开着但暂不落盘"的占位）。
func Nop() Sink { return SinkFunc(func(context.Context, Entry) error { return nil }) }

// Closer 是可选的 sink 收尾接口（`Fanout.Close` 会按注册顺序调用）。
type Closer interface{ Close() error }

// RequireMasked 包住一个第三方 sink，让它在条目缺少本包凭据时拒绝接收。
//
// 用途：宿主自己实现的 sink 在开发期就拒绝"绕过 Fanout 的原样条目"，
// 而不是等到合规检查时才发现某条链路直接调了它。
func RequireMasked(s Sink) Sink { return guarded{inner: s} }

// guarded 在转发的同时保住被包 sink 的 Closer 能力。
//
// 为什么要专门保住：用 SinkFunc 一包，`Fanout.Close` 的类型断言就再也看不到内层 sink，
// 带缓冲的审计 sink（落库、落文件）会**永远不 flush** —— 丢的是事后要拿去举证的事实。
type guarded struct{ inner Sink }

func (g guarded) Emit(ctx context.Context, e Entry) error {
	if err := e.validate(true); err != nil {
		return err
	}
	return g.inner.Emit(ctx, e)
}

func (g guarded) Close() error {
	if c, ok := g.inner.(Closer); ok {
		return c.Close()
	}
	return nil
}

// Recalling 收集条目，供测试与管理面回看。零值可用。
type Recalling struct {
	mu    sync.Mutex
	items []Entry
}

// Emit 实现 Sink（存副本：调用方后续改写自己的 map 不应影响已投递的事实）。
func (r *Recalling) Emit(_ context.Context, e Entry) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.items = append(r.items, e.Clone())
	return nil
}

// Entries 返回已收集条目的副本（切片本身新分配）。
func (r *Recalling) Entries() []Entry {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Entry(nil), r.items...)
}

// Len 返回已收集条数。
func (r *Recalling) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.items)
}

// Reset 清空（只用于测试与显式管理面操作）。
func (r *Recalling) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.items = nil
}

// ---- Fanout：唯一投递路径 ----

// Fanout 把一条事实先脱敏、再按注册顺序多播。
type Fanout struct {
	mask  security.Masker
	clock kitctx.Clock
	sinks []Sink
}

// New 构造投递器。mask 为空拒绝；clock 为空用系统时钟；sinks 为空或含 nil 拒绝。
func New(mask security.Masker, clock kitctx.Clock, sinks ...Sink) (*Fanout, error) {
	if mask == nil {
		return nil, ErrMaskerRequired
	}
	if clock == nil {
		clock = kitctx.SystemClock()
	}
	if len(sinks) == 0 {
		return nil, ErrNoSink
	}
	for i, s := range sinks {
		if s == nil {
			return nil, fmt.Errorf("%w：第 %d 个", ErrSinkNil, i)
		}
	}
	return &Fanout{mask: mask, clock: clock, sinks: append([]Sink(nil), sinks...)}, nil
}

// Emit 脱敏并多播。
//
// 顺序与失败处理：脱敏失败 ⇒ **一个 sink 都不碰**（宁可不落，不落原样数据）；
// 脱敏成功后每个 sink 都被尝试（一个 sink 失败不得让其余 sink 漏记），错误按 sink 顺序聚合。
func (f *Fanout) Emit(ctx context.Context, e Entry) error {
	if err := e.validate(false); err != nil {
		return err
	}
	if rc, ok := kitctx.RequestFrom(ctx); ok {
		if e.TraceID == "" {
			e.TraceID = rc.TraceID
		}
		if e.UserID == "" {
			e.UserID = rc.UserID
		}
		if e.Role == "" {
			e.Role = string(rc.Role)
		}
	}
	if e.Sign == "" {
		e.Sign = e.Action
	}
	masked, applied, err := f.mask(ctx, e.Sign, e.Fields)
	if err != nil {
		return fmt.Errorf("%w：%s：%w", ErrMaskFailed, e.Action, err)
	}
	if e.Fields != nil {
		if m, ok := masked.(map[string]any); ok {
			e.Fields = m
		} else if masked == nil {
			e.Fields = nil
		} else {
			return fmt.Errorf("%w：%s：Masker 返回的载荷不是 map[string]any，无法确认脱敏结果", ErrMaskFailed, e.Action)
		}
	}
	e.Applied = append([]string(nil), applied...)
	sort.Strings(e.Applied)
	if e.Time.IsZero() {
		e.Time = f.clock.Now()
	}
	e.proof = &seal{}

	var problems []error
	for i, s := range f.sinks {
		if err := s.Emit(ctx, e.Clone()); err != nil {
			problems = append(problems, fmt.Errorf("audit: 第 %d 个 sink 投递失败（%s）：%w", i, e.Action, err))
		}
	}
	if len(problems) > 0 {
		return errors.Join(problems...)
	}
	return nil
}

// Sinks 返回 sink 数量（管理面内省用，不交出实例本身）。
func (f *Fanout) Sinks() int { return len(f.sinks) }

// Close 关闭所有实现了 Closer 的 sink（按注册顺序），错误聚合。
// 幂等性由 sink 自己保证；本包不禁止 Close 后再 Emit —— 那是 sink 的语义。
func (f *Fanout) Close() error {
	var problems []error
	for i, s := range f.sinks {
		if c, ok := s.(Closer); ok {
			if err := c.Close(); err != nil {
				problems = append(problems, fmt.Errorf("audit: 第 %d 个 sink 关闭失败：%w", i, err))
			}
		}
	}
	return errors.Join(problems...)
}

// BlacklistMasker 把 `security.ApplyBlacklist` 适配成 Masker（多数程序要的默认脱敏就是它）。
//
// 忽略 sign：黑名单与主体无关。extra 是**追加**字段，不能减少既有黑名单项
// （`security` 侧已保证不可被载荷关闭，本适配器只是搬运参数）。
func BlacklistMasker(extra ...string) security.Masker {
	return func(_ context.Context, _ string, data any) (any, []string, error) {
		return security.ApplyBlacklist(data, extra...)
	}
}

// ---- 编译期自检 ----

var (
	_ Sink            = SinkFunc(nil)
	_ Sink            = (*Recalling)(nil)
	_ Sink            = Nop()
	_ security.Masker = BlacklistMasker()
)
