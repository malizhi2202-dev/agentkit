// 本文件是**框架侧的网关装配**（ / 框架裁决 / 既有教训）：
// 事件 → 会话 → （注入的）引擎问答 → 文本/卡片经（注入的）Transport 下发。
//
// 它**不是** `cmd/im-gateway` 进程本体（M4 · ~）：进程、传输实现、配置装载、
// 后端服务 对外入口的 HTTP 调用都在 `cmd/im-gateway` 内，经本文件的两个注入点接进来。
//
// 硬边界（违反即失败）：
//
//  1. **零业务 HTTP**（既有教训 · 契约 4 / ）：本包不 import `net/http`、不拼
//     `/api/v1/...` 路径、不持有任何凭据。真正调 后端服务 对外入口（`后端入口 *`）
//     的实现由 `cmd/im-gateway`以 `Responder` 注入 —— 与 `model.HTTPDoer` /
//     `Transport` 同一种注入风格：框架只定义「往哪里注入」，实现方自带传输。
//
//  2. **身份只能来自配置查表**（既有教训 · 契约 3）：被代表 后端服务 用户一律经 `Directory`
//     查表取得；**禁止**把入站 `IncomingMessage.UserID`（IM 平台账号）当身份头透传，
//     也**禁止**任何兜底。未传账号 / 未登记 / 映射为空 ⇒ `ErrUserNotMapped`，不调引擎、
//     不下发 —— 这是 “（未传 user 或凭据失效不降级为无脱敏兜底）在 IM 侧的落点。
//
//  3. **失败隔离 + 有上限重试**（ / 框架裁决 第 5 条）：引擎不可用返回可识别错误
//     `ErrEngineUnavailable`，重试次数 **≤ MaxAttempts**（≤0 ⇒ 仅 1 次），**不无限重放**；
//     单条事件失败不终止事件流循环（`Run`）。本包不持有任何 后端服务 侧资源（无连接、
//     无 DB、无全局可变状态），因此网关崩溃/重启对 后端服务 无影响。
//
//  4. **dry-run 零出网**（框架裁决 第 8 条）：`DryRun = true` 时**所有**下发短路成内存
//     outbox 记录，`Transport` 一次都不被调用；配合 `SampleEvents` / `SampleDirectory` /
//     `DryRunResponder{}` 即可让内置事件走完整链路而不出网
//     （沿用参考仓库形态：Gateway 在 DryRun 时自己把所有发送短路，出站方向不需要替身）。
//
//  5. **只做读侧**：问答 + 卡片下发，无确认/取消写动作回调（`CHANGE.md:75`）。
package im

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// 本文件哨兵错误。调用方用 errors.Is 判定，**不要**比对错误文本。
var (
	// ErrGatewayConfig 装配不完整或装配不一致（缺 app_id / Directory / Responder，
	// 或调用方缺少该路径所需的注入件）。
	ErrGatewayConfig = errors.New("im: 网关配置不完整")
	// ErrInvalidEvent 事件不可处理（平台事件 ID字段 为空、消息体畸形）。
	// fail-closed：不进处理链，也不静默丢弃。
	ErrInvalidEvent = errors.New("im: 事件不可处理")
	// ErrUserNotMapped 入站账号未登记在身份映射表中（或未传）。
	// **禁止**据此回退为入站账号 / 匿名身份。
	ErrUserNotMapped = errors.New("im: 入站账号未登记在身份映射表中")
	// ErrEngineUnavailable 引擎（后端服务 对外入口）不可用，且已按上限重试完毕。
	ErrEngineUnavailable = errors.New("im: 引擎不可用")
	// ErrEventStream 无法建立入站事件流（配置缺失 / 握手失败），区别于「暂时没消息」。
	ErrEventStream = errors.New("im: 无法建立事件流")
	// ErrDeliveryFailed 出站下发失败（或卡片 fail-closed 校验未通过而放弃下发）。
	ErrDeliveryFailed = errors.New("im: 出站下发失败")
)

// Directory 是「IM 平台账号 → 后端服务 被代表用户」的**配置查表**（框架裁决 第 3 条 / 既有教训 · 契约 3）。
//
// 它是身份的**唯一**来源：网关从不读 `IncomingMessage` 的账号字段做身份，也不做任何兜底。
// 未登记 ⇒ 返回一个 `ErrUserNotMapped` 链上的错误；返回空串同样被 `Handle` 视为未映射。
type Directory interface {
	HMUser(account string) (string, error)
}

// DirectoryFunc 让普通函数满足 Directory（与 http.HandlerFunc 同形）。
type DirectoryFunc func(account string) (string, error)

// HMUser 实现 Directory。
func (f DirectoryFunc) HMUser(account string) (string, error) {
	if f == nil {
		return "", fmt.Errorf("%w: 未配置身份映射表", ErrUserNotMapped)
	}
	return f(account)
}

// DirectoryFromMap 用 `conf/im-gateway.yml` 的 `mapping[]` 构造身份查表。
//
// 空白键或空白值一律不入表（防空值命中）；查询时两侧空白规范化；
// 空账号不命中任何条目 ⇒ 未传账号天然 fail-closed，不需要额外分支。
func DirectoryFromMap(mapping map[string]string) Directory {
	table := make(map[string]string, len(mapping))
	for account, hmUser := range mapping {
		account, hmUser = strings.TrimSpace(account), strings.TrimSpace(hmUser)
		if account == "" || hmUser == "" {
			continue
		}
		table[account] = hmUser
	}
	return DirectoryFunc(func(account string) (string, error) {
		account = strings.TrimSpace(account)
		if account == "" {
			return "", fmt.Errorf("%w: 未传账号", ErrUserNotMapped)
		}
		hmUser, ok := table[account]
		if !ok {
			return "", fmt.Errorf("%w: 账号不在映射表内", ErrUserNotMapped)
		}
		return hmUser, nil
	})
}

// Reply 是实现方（`cmd/im-gateway`）从引擎响应转换出的**待下发内容**。
//
// 它是**框架侧的注入契约**，不是 后端服务 对外接口的 wire 契约：
// wire 契约（`parsed`/`report`/`memory`/`degraded` 等字段）唯一归
// `宿主接入层` + ，本包不重新定义、不解释其结构。
type Reply struct {
	// Texts 是要下发的文本（长文本由网关按 MaxTextBytes 用 SplitText 自动分段）。
	Texts []string
	// Cards 是要下发的读侧卡片（下发前逐张 Validate，不过即放弃并不发空卡片）。
	Cards []Card
	// TraceID 是引擎回显的幂等/回溯键；为空表示本轮回显缺失，保留会话原值。
	TraceID string
	// Context 是引擎回显的上下文原文，**不透明**原样写入会话、下次原样带回。
	Context json.RawMessage
}

// ReplyRequest 是一次问答请求（注入方唯一能看到的东西）。
type ReplyRequest struct {
	// Session 是当前会话（含上一次回显的 TraceID / Context；新会话为零值）。
	Session Session
	// Message 是归一化后的入站消息。
	Message IncomingMessage
	// HMUser 是**经 Directory 查表**得到的被代表 后端服务 用户。
	// 注入方必须原样用作 `后端身份头`，不得改用 Message 里的任何字段。
	HMUser string
}

// Responder 是「问引擎」的注入接口，由 `cmd/im-gateway`实现为
// 「携带既有会话凭据 + 逐请求 后端身份头 调 `后端入口 *`」。
//
// 失败语义：返回错误表示**本次问答失败**（后端服务 不可用、凭据失效等）；
// 网关按上限重试后回可识别错误 `ErrEngineUnavailable`，**不**构造任何降级应答
// （不为「未传 user / 凭据失效」兜底：该裁决属对外入口）。
type Responder interface {
	Respond(ctx context.Context, req ReplyRequest) (Reply, error)
}

// ResponderFunc 让普通函数满足 Responder。
type ResponderFunc func(ctx context.Context, req ReplyRequest) (Reply, error)

// Respond 实现 Responder。
func (f ResponderFunc) Respond(ctx context.Context, req ReplyRequest) (Reply, error) {
	return f(ctx, req)
}

// DryRunResponder 是 `--dry-run` 用的引擎替身：**零 I/O、零出网**，返回一条固定应答。
//
// 它让「内置事件走完整链路」不必依赖任何真实端点（框架裁决 第 8 条），
// 也让 dry-run 的「零出网」不取决于调用方是否记得换掉真实 Responder。
type DryRunResponder struct{}

// Respond 实现 Responder（不读配置、不发请求、不落盘）。
func (DryRunResponder) Respond(_ context.Context, req ReplyRequest) (Reply, error) {
	card := NewCard(
		"干跑",
		fmt.Sprintf("本卡片由 --dry-run 生成：未调用引擎、未出网（入站文本 %d 字节）。", len(req.Message.Text)),
		CardAction{Text: "入口命令详情", Name: "dry-run"},
	)
	return Reply{
		Texts:   []string{"[dry-run] 这条没有真去问引擎，仅用于配置与链路自检。"},
		Cards:   []Card{card},
		TraceID: req.Session.TraceID,
		Context: append(json.RawMessage(nil), req.Session.Context...),
	}, nil
}

// Delivery 是 dry-run 下「本应下发」的一条出站记录（真实下发时不会产生）。
type Delivery struct {
	To   ChatRef
	Text string
	Card *Card
}

// Stats 是网关运行计数（并发安全快照；诊断与 dry-run 自证用）。
type Stats struct {
	// Received 进入 Handle 的事件数（含被跳过与被拒的）。
	Received uint64
	// Skipped 生命周期事件（心跳等）数：不处理、也不算失败。
	Skipped uint64
	// Handled 完整走完链路的条数。
	Handled uint64
	// Failed 返回错误的条数（畸形事件 / 身份未映射 / 引擎不可用 / 下发失败）。
	Failed uint64
	// Sent 真实下发次数（DryRun 下恒为 0）。
	Sent uint64
	// WouldSend dry-run 下记录进 outbox 的下发次数（真实模式恒为 0）。
	WouldSend uint64
}

// GatewayConfig 是网关装配参数。
type GatewayConfig struct {
	// AppID IM 平台应用标识；同时是会话存储的 app_id（同 app_id 单实例语义见 框架裁决 第 6 条，
	// 由 `cmd/im-gateway` 用 ClaimInstance 在启动时保证）。必填。
	AppID string
	// Directory 身份映射查表。必填：缺省即无身份来源，装配直接失败（不兜底）。
	Directory Directory
	// Responder 引擎问答注入点。必填（dry-run 请显式给 DryRunResponder{}，不隐式替换）。
	Responder Responder
	// Transport IM 出站传输注入点。DryRun 下**不会被调用**，可为 nil；
	// 非 dry-run 且为 nil 时 Run 报 ErrGatewayConfig、下发报 ErrDeliveryFailed（不 panic）。
	Transport Transport
	// CardCodec 把语义卡片编码成平台线格式；必填（不注入即拒发卡片，见 sendCard）。
	CardCodec CardCodec
	// Store 会话集合。为 nil 时按 AppID 新建（不落盘；持久化属 cmd/im-gateway 的装配选择）。
	Store *SessionStore
	// DryRun 干跑：所有下发短路成内存 outbox，零出网。
	DryRun bool
	// MaxTextBytes 单条出站文本字节上限；≤0 交给 SplitText 用 DefaultMaxTextBytes
	// （默认值唯一归 protocol.go，本文件不重复一份）。
	MaxTextBytes int
	// MaxAttempts 引擎问答与出站下发的**重试上限**；≤0 ⇒ 1 次（单次，不重放）。
	// 上限是安全属性：宁可失败也不无限重放。
	MaxAttempts int
	// RetryDelay 两次尝试之间的退避；≤0 ⇒ 立即重试。
	RetryDelay time.Duration
}

// Gateway 是消息→会话→问答→下发的装配（对应参考实现 Python 侧 bridge.Bridge 的位置，
// 但只保留框架职责：不解析业务指令、不渲染业务卡片、不落盘）。
type Gateway struct {
	cfg   GatewayConfig
	store *SessionStore

	received  atomic.Uint64
	skipped   atomic.Uint64
	handled   atomic.Uint64
	failed    atomic.Uint64
	sent      atomic.Uint64
	wouldSend atomic.Uint64

	mu     sync.Mutex
	outbox []Delivery
}

// NewGateway 装配网关。缺少任何必需注入件一律 `ErrGatewayConfig`（fail-closed，不补默认实现）。
func NewGateway(cfg GatewayConfig) (*Gateway, error) {
	cfg.AppID = strings.TrimSpace(cfg.AppID)
	if cfg.AppID == "" {
		return nil, fmt.Errorf("%w: 缺少 app_id", ErrGatewayConfig)
	}
	if cfg.Directory == nil {
		return nil, fmt.Errorf("%w: 缺少身份映射表 Directory", ErrGatewayConfig)
	}
	if cfg.Responder == nil {
		return nil, fmt.Errorf("%w: 缺少引擎注入 Responder", ErrGatewayConfig)
	}
	store := cfg.Store
	if store == nil {
		var err error
		store, err = NewSessionStore(cfg.AppID)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrGatewayConfig, err)
		}
	}
	return &Gateway{cfg: cfg, store: store}, nil
}

// Store 返回会话集合（装配时给定的，或按 AppID 新建的）。
func (g *Gateway) Store() *SessionStore { return g.store }

// Stats 返回计数快照。
func (g *Gateway) Stats() Stats {
	return Stats{
		Received:  g.received.Load(),
		Skipped:   g.skipped.Load(),
		Handled:   g.handled.Load(),
		Failed:    g.failed.Load(),
		Sent:      g.sent.Load(),
		WouldSend: g.wouldSend.Load(),
	}
}

// Outbox 返回 dry-run 下「本应下发」的记录拷贝（真实模式下恒为空）。
func (g *Gateway) Outbox() []Delivery {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]Delivery, len(g.outbox))
	copy(out, g.outbox)
	for i := range out {
		if out[i].Card != nil {
			card := *out[i].Card
			out[i].Card = &card
		}
	}
	return out
}

// Run 消费 `Transport.Events` 直到 ctx 结束或事件流关闭。
//
// 失败隔离：单条事件失败**绝不**终止循环（错误计入 Stats.Failed 后继续），
// 因此一次坏消息不会拖垮网关，更不会影响 后端服务（本包不持有其任何资源）。
// 事件流本身建立失败返回 `ErrEventStream`（可识别错误，区别于「暂时没消息」）。
func (g *Gateway) Run(ctx context.Context) error {
	if g.cfg.Transport == nil {
		return fmt.Errorf("%w: 缺少 Transport（dry-run 自检请用 RunSample）", ErrGatewayConfig)
	}
	events, err := g.cfg.Transport.Events(ctx)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrEventStream, err)
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-events:
			if !ok {
				return nil
			}
			_ = g.Handle(ctx, ev) // 单条失败隔离：错误已计入 Stats().Failed
		}
	}
}

// RunSample 把内置事件逐条走完整链路（dry-run 自检入口），返回成功条数与首个错误。
//
// 与 `DryRunResponder{}` + `SampleDirectory` 配套使用即可做到「完整链路、零出网」；
// 非 dry-run 时它会真的下发 —— 调用方负责只在 dry-run 使用（框架不隐式改写调用方意图）。
func (g *Gateway) RunSample(ctx context.Context) (int, error) {
	handled := 0
	var firstErr error
	for _, ev := range SampleEvents() {
		if err := ctx.Err(); err != nil {
			return handled, err
		}
		if err := g.Handle(ctx, ev); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if ev.IsMessage() {
			handled++
		}
	}
	return handled, firstErr
}

// Handle 处理一条入站事件。返回的错误都是可识别错误（errors.Is 可判定）：
// `ErrInvalidEvent` / `ErrUserNotMapped` / `ErrEngineUnavailable` / `ErrDeliveryFailed`。
//
// 全链路为：事件校验 → 身份查表 → 取/建会话 → 问答（有上限重试）→ 分段+卡片下发
// → 成功后才回写会话（空回显不擦除上一代）。
func (g *Gateway) Handle(ctx context.Context, ev Event) error {
	g.received.Add(1)

	msg, hmUser, err := g.admit(ev)
	if err != nil {
		g.failed.Add(1)
		return err
	}
	if msg == nil { // 心跳等生命周期事件：不处理，也不算失败
		g.skipped.Add(1)
		return nil
	}
	if err := g.serve(ctx, msg, hmUser); err != nil {
		g.failed.Add(1)
		return err
	}
	g.handled.Add(1)
	return nil
}

// admit 校验事件并解析被代表用户。返回 (nil, "", nil) 表示该事件无需处理（心跳等）。
//
// 身份：只来自配置查表。未传 / 未登记 / 映射为空一律 fail-closed，
// 绝不回退为入站账号、绝不匿名兜底（既有教训 · 契约 3 / ）。
func (g *Gateway) admit(ev Event) (*IncomingMessage, string, error) {
	if strings.TrimSpace(ev.ID) == "" {
		return nil, "", fmt.Errorf("%w: event_id 为空（无法去重与 ACK，不进入处理链）", ErrInvalidEvent)
	}
	if !ev.IsMessage() {
		if ev.Message != nil {
			// 有消息体但会话类型不可识别 ⇒ 畸形消息，fail-closed（不静默丢弃）。
			return nil, "", fmt.Errorf("%w: 消息的会话类型不可识别", ErrInvalidEvent)
		}
		return nil, "", nil
	}
	if err := ev.Message.Valid(); err != nil {
		return nil, "", fmt.Errorf("%w: %w", ErrInvalidEvent, err)
	}
	hmUser, err := g.cfg.Directory.HMUser(ev.Message.UserID)
	if err != nil {
		if errors.Is(err, ErrUserNotMapped) {
			return nil, "", err
		}
		return nil, "", fmt.Errorf("%w: %v", ErrUserNotMapped, err)
	}
	if strings.TrimSpace(hmUser) == "" {
		return nil, "", fmt.Errorf("%w: 映射结果为空", ErrUserNotMapped)
	}
	return ev.Message, hmUser, nil
}

// serve 取/建会话 → 问答（有上限重试）→ 下发 → 成功才回写会话。
func (g *Gateway) serve(ctx context.Context, msg *IncomingMessage, hmUser string) error {
	key := SessionKey{AppID: g.cfg.AppID, ChatType: msg.ChatType, ChatID: msg.ChatID, SenderID: msg.UserID}
	sess, ok := g.store.Get(key)
	if !ok {
		sess = Session{Key: key}
	}

	reply, err := g.ask(ctx, sess, *msg, hmUser)
	if err != nil {
		return err
	}
	if err := g.deliver(ctx, msg.Chat(), reply); err != nil {
		return err
	}
	return g.commit(sess, reply)
}

// commit 回写会话：只有完整成功的一轮才回写，避免「用户没看到回复、下一轮上下文却已前进」。
// 空回显保留上一代（引擎未回显 ≠ 要求清空）。
func (g *Gateway) commit(sess Session, reply Reply) error {
	if strings.TrimSpace(reply.TraceID) != "" {
		sess.TraceID = reply.TraceID
	}
	if len(reply.Context) > 0 {
		sess.Context = append(json.RawMessage(nil), reply.Context...)
	}
	if err := g.store.Put(sess); err != nil {
		return fmt.Errorf("%w: 会话回写失败: %v", ErrGatewayConfig, err)
	}
	return nil
}

// ask 调注入的 Responder，并按上限重试；失败一律回 ErrEngineUnavailable。
func (g *Gateway) ask(ctx context.Context, sess Session, msg IncomingMessage, hmUser string) (Reply, error) {
	var reply Reply
	err := g.retry(ctx, func() error {
		r, err := g.cfg.Responder.Respond(ctx, ReplyRequest{Session: sess, Message: msg, HMUser: hmUser})
		if err != nil {
			return err
		}
		reply = r
		return nil
	})
	if err != nil {
		return Reply{}, fmt.Errorf("%w: 重试 %d 次后仍失败: %w", ErrEngineUnavailable, g.maxAttempts(), err)
	}
	return reply, nil
}

// retry 以**有上限**的次数重复 fn，直到成功、ctx 结束或耗尽上限。
//
// 上限是安全属性（不无限重放）：`MaxAttempts <= 0` ⇒ 1 次。
func (g *Gateway) retry(ctx context.Context, fn func() error) error {
	attempts := g.maxAttempts()
	var last error
	for attempt := 1; attempt <= attempts; attempt++ {
		if attempt > 1 {
			if err := g.wait(ctx); err != nil {
				return fmt.Errorf("重试等待被中断: %w", err)
			}
		}
		if err := fn(); err != nil {
			last = err
		} else {
			return nil
		}
		if ctx.Err() != nil {
			break
		}
	}
	return last
}

// wait 退避 retry_delay；≤0 时只检查 ctx。
func (g *Gateway) wait(ctx context.Context) error {
	if g.cfg.RetryDelay <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(g.cfg.RetryDelay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (g *Gateway) maxAttempts() int {
	if g.cfg.MaxAttempts <= 0 {
		return 1
	}
	return g.cfg.MaxAttempts
}

// deliver 下发一轮回复：长文本分段、卡片先 Validate（fail-closed：不发空卡片）。
// 返回首个错误（不中断其余段落的下发：用户拿到半截比拿不到好）。
func (g *Gateway) deliver(ctx context.Context, to ChatRef, reply Reply) error {
	if err := to.Valid(); err != nil {
		return fmt.Errorf("%w: 出站目标不可寻址: %w", ErrDeliveryFailed, err)
	}
	var firstErr error
	for _, text := range reply.Texts {
		for _, chunk := range SplitText(text, g.cfg.MaxTextBytes) {
			if err := g.sendText(ctx, to, chunk); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}
	for _, card := range reply.Cards {
		if err := card.Validate(); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("%w: %w", ErrDeliveryFailed, err)
			}
			continue
		}
		if err := g.sendCard(ctx, to, card); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (g *Gateway) sendText(ctx context.Context, to ChatRef, text string) error {
	if g.cfg.DryRun {
		g.record(Delivery{To: to, Text: text})
		g.wouldSend.Add(1)
		return nil
	}
	return g.send(ctx, "文本", func() error {
		_, err := g.cfg.Transport.SendMessage(ctx, to, text)
		return err
	})
}

func (g *Gateway) sendCard(ctx context.Context, to ChatRef, card Card) error {
	if g.cfg.DryRun {
		c := card
		g.record(Delivery{To: to, Card: &c})
		g.wouldSend.Add(1)
		return nil
	}
	return g.send(ctx, "卡片", func() error {
		if g.cfg.CardCodec == nil {
			return fmt.Errorf("%w: 缺少 CardCodec（卡片线格式属接入方，包不做隐式编码）", ErrNoCardCodec)
		}
		payload, err := g.cfg.CardCodec.EncodeCard(card)
		if err != nil {
			return fmt.Errorf("卡片编码失败：%w", err)
		}
		_, err = g.cfg.Transport.SendCard(ctx, to, payload)
		return err
	})
}

// send 是真实下发（非 dry-run）的唯一出口：Transport 缺失即失败（不 panic），
// 失败按上限重试，最终一律回 ErrDeliveryFailed。
func (g *Gateway) send(ctx context.Context, kind string, do func() error) error {
	if g.cfg.Transport == nil {
		return fmt.Errorf("%w: 缺少 Transport", ErrDeliveryFailed)
	}
	if err := g.retry(ctx, do); err != nil {
		return fmt.Errorf("%w: 下发%s失败: %w", ErrDeliveryFailed, kind, err)
	}
	g.sent.Add(1)
	return nil
}

func (g *Gateway) record(d Delivery) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.outbox = append(g.outbox, d)
}

// SampleEvents 是 `--dry-run` 的内置事件（三类会话各一条 + 一条心跳）。
//
// 与 SampleDirectory 配套；账号是固定的，所以身份查表仍然生效 ——
// 干跑不为了跑通而放宽任何身份口径。
func SampleEvents() []Event {
	return []Event{
		{
			ID:   "dry-dm-1",
			Type: EventSingleChat,
			Message: &IncomingMessage{
				ChatID: "tt-alice", ChatType: ChatTypeDM, ChatName: "爱丽丝",
				UserID: "tt-alice", UserName: "爱丽丝", MessageID: "dry-m1", Text: "research:1.2.3.4",
			},
		},
		{
			ID:   "dry-group-1",
			Type: EventGroupChat,
			Message: &IncomingMessage{
				ChatID: "1234567890123456", ChatType: ChatTypeGroup, ChatName: "入口命令值班群",
				UserID: "tt-bob", UserName: "Bob", MessageID: "dry-m2", Text: "@bot 帮我看下这个域名", AtMe: true,
			},
		},
		{
			ID:   "dry-channel-1",
			Type: EventTeamsPostCreate,
			Message: &IncomingMessage{
				ChatID: "teams_1_2_3", ChatType: ChatTypeChannel, ChatName: "安全频道",
				UserID: "tt-carol", UserName: "Carol", MessageID: "dry-m3", Text: "这个 IP 什么情况",
			},
		},
		{ID: "dry-keepalive-1", Type: EventKeepalive},
	}
}

// SampleDirectory 是 SampleEvents 的配套身份查表（**只用于 dry-run / 自测**）。
//
// 它仍然是一张表：干跑也不把入站账号当身份透传（既有教训 · 契约 3）。
func SampleDirectory() Directory {
	return DirectoryFromMap(map[string]string{
		"tt-alice": "hm-alice",
		"tt-bob":   "hm-bob",
		"tt-carol": "hm-carol",
	})
}

// ErrNoCardCodec 表示没注入卡片编码器。它是**必填**而非可选：缺了它就发不出卡片，
// 而不是悄悄按包内标签产出一个可能谁也不认识的形状 —— 失败要响，不要漂。
var ErrNoCardCodec = errors.New("im: 缺少 CardCodec")
