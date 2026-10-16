package im

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// ---------- 测试替身（Transport 另用 card_test.go 的 fakeTransport） ----------

// countingTransport 记录真实下发次数：dry-run 的「零出网」证据就是它的 0。
type countingTransport struct {
	name string
	ch   chan Event

	mu    sync.Mutex
	texts []string
	cards []string
}

func newCountingTransport(buf int) *countingTransport {
	return &countingTransport{name: "counting", ch: make(chan Event, buf)}
}

func (c *countingTransport) Name() string { return c.name }

func (c *countingTransport) Events(_ context.Context) (<-chan Event, error) { return c.ch, nil }

func (c *countingTransport) SendMessage(_ context.Context, to ChatRef, text string) (MessageID, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.texts = append(c.texts, to.ID+"|"+text)
	return MessageID("t-1"), nil
}

func (c *countingTransport) SendCard(_ context.Context, to ChatRef, payload []byte) (MessageID, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cards = append(c.cards, string(payload))
	return MessageID("c-1"), nil
}

func (c *countingTransport) sent() (texts, cards int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.texts), len(c.cards)
}

var _ Transport = (*countingTransport)(nil)

// recordingResponder 是被注入的「引擎」替身，并作为身份口径的探针：
// 它记录自己看到的 HMUser / 会话回显，用来证明身份只来自配置查表。
type recordingResponder struct {
	mu        sync.Mutex
	calls     int
	hmUsers   []string
	traceIDs  []string
	contexts  [][]byte
	reply     Reply
	alwaysErr error
}

func (r *recordingResponder) Respond(_ context.Context, req ReplyRequest) (Reply, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	r.hmUsers = append(r.hmUsers, req.HMUser)
	r.traceIDs = append(r.traceIDs, req.Session.TraceID)
	r.contexts = append(r.contexts, append([]byte(nil), req.Session.Context...))
	if r.alwaysErr != nil {
		return Reply{}, r.alwaysErr
	}
	return r.reply, nil
}

func (r *recordingResponder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

func (r *recordingResponder) seenHMUsers() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.hmUsers...)
}

func (r *recordingResponder) lastTraceID() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.traceIDs) == 0 {
		return ""
	}
	return r.traceIDs[len(r.traceIDs)-1]
}

func (r *recordingResponder) lastContext() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.contexts) == 0 {
		return nil
	}
	return append([]byte(nil), r.contexts[len(r.contexts)-1]...)
}

var (
	_ Responder = (*recordingResponder)(nil)
	_ Responder = ResponderFunc(nil)
	_ Directory = DirectoryFunc(nil)
)

// ---------- 测试夹具 ----------

func mustGateway(t *testing.T, cfg GatewayConfig) *Gateway {
	if cfg.CardCodec == nil {
		cfg.CardCodec = StandardCardCodec // 测试用参照编码；平台编码归接入方，见 codec 测试
	}
	t.Helper()
	g, err := NewGateway(cfg)
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}
	return g
}

func dmEvent(id, account, text string) Event {
	return dmEventAs(id, account, account, text)
}

// dmEventAs 让「会话 ID」与「发送者账号」分离（测「未传 user」时 chat_id 仍合法）。
func dmEventAs(id, chatID, account, text string) Event {
	return Event{
		ID:   id,
		Type: EventSingleChat,
		Message: &IncomingMessage{
			ChatID:    chatID,
			ChatType:  ChatTypeDM,
			ChatName:  chatID,
			UserID:    account,
			MessageID: "m-" + id,
			Text:      text,
		},
	}
}

func aliceDir() Directory {
	return DirectoryFromMap(map[string]string{"tt-alice": "hm-alice"})
}

// ---------- 装配与配置校验 ----------

func TestNewGateway_ConfigValidation(t *testing.T) {
	dir, resp := aliceDir(), &recordingResponder{}

	if _, err := NewGateway(GatewayConfig{}); !errors.Is(err, ErrGatewayConfig) {
		t.Fatalf("空配置必须被拒（ErrGatewayConfig），得到 %v", err)
	}
	if _, err := NewGateway(GatewayConfig{AppID: "app1"}); !errors.Is(err, ErrGatewayConfig) {
		t.Fatalf("缺 Directory / Responder 必须被拒，得到 %v", err)
	}
	if _, err := NewGateway(GatewayConfig{AppID: "app1", Directory: dir}); !errors.Is(err, ErrGatewayConfig) {
		t.Fatalf("缺 Responder 必须被拒，得到 %v", err)
	}

	g := mustGateway(t, GatewayConfig{AppID: "  app1  ", Directory: dir, Responder: resp})
	if g.Store() == nil || g.Store().AppID() != "app1" {
		t.Fatalf("Store 应按 app_id 装配，得到 %v", g.Store())
	}
	if st := g.Stats(); st != (Stats{}) {
		t.Fatalf("新网关计数应全零：%+v", st)
	}
}

// ---------- 身份口径（L-022 契约 3 /  的 IM 侧落点） ----------

// TestGateway_IdentityOnlyFromConfigTable 身份**只能**来自配置查表；
// **禁止**把入站 user_account 当身份头透传。
func TestGateway_IdentityOnlyFromConfigTable(t *testing.T) {
	resp := &recordingResponder{reply: Reply{Texts: []string{"ok"}}}
	tr := newCountingTransport(0)
	g := mustGateway(t, GatewayConfig{AppID: "app1", Directory: aliceDir(), Responder: resp, Transport: tr})

	if err := g.Handle(context.Background(), dmEvent("e1", "tt-alice", "research:1.2.3.4")); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	got := resp.seenHMUsers()
	if len(got) != 1 || got[0] != "hm-alice" {
		t.Fatalf("被代表用户必须来自配置查表 hm-alice，得到 %v", got)
	}
	if got[0] == "tt-alice" {
		t.Fatal("禁止把入站账号透传为身份")
	}
}

// TestGateway_NoFallbackOnUnmappedOrMissingUser 未传 / 未映射 ⇒ 可识别错误，
// 不调引擎、不下发、**不兜底**（：不降级为无脱敏/匿名兜底）。
func TestGateway_NoFallbackOnUnmappedOrMissingUser(t *testing.T) {
	cases := []struct {
		name    string
		chatID  string
		account string
	}{
		{"未映射账号", "tt-eve", "tt-eve"},
		{"未传 user", "tt-alice", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := &recordingResponder{reply: Reply{Texts: []string{"ok"}}}
			tr := newCountingTransport(0)
			g := mustGateway(t, GatewayConfig{AppID: "app1", Directory: aliceDir(), Responder: resp, Transport: tr})

			err := g.Handle(context.Background(), dmEventAs("e1", tc.chatID, tc.account, "research:1.2.3.4"))
			if !errors.Is(err, ErrUserNotMapped) {
				t.Fatalf("期望 ErrUserNotMapped，得到 %v", err)
			}
			if resp.count() != 0 {
				t.Fatalf("身份不可解析时不得调用引擎（兜底），calls=%d", resp.count())
			}
			if texts, cards := tr.sent(); texts != 0 || cards != 0 {
				t.Fatalf("身份不可解析时不得下发任何内容：texts=%d cards=%d", texts, cards)
			}
			if len(g.Outbox()) != 0 {
				t.Fatalf("dry-run outbox 也不得有内容：%+v", g.Outbox())
			}
			if st := g.Stats(); st.Failed != 1 || st.Handled != 0 {
				t.Fatalf("计数漂移：%+v", st)
			}
		})
	}
}

// TestGateway_EngineUnavailableBoundedRetry 后端服务 不可用 ⇒ 可识别错误 +
// **有上限**重试，**不无限重放**（ / ADR-003 第 5 条）。
func TestGateway_EngineUnavailableBoundedRetry(t *testing.T) {
	t.Run("按上限重试", func(t *testing.T) {
		resp := &recordingResponder{alwaysErr: errors.New("宿主侧 连不上")}
		tr := newCountingTransport(0)
		g := mustGateway(t, GatewayConfig{
			AppID: "app1", Directory: aliceDir(), Responder: resp, Transport: tr, MaxAttempts: 3,
		})

		err := g.Handle(context.Background(), dmEvent("e1", "tt-alice", "research:1.2.3.4"))
		if !errors.Is(err, ErrEngineUnavailable) {
			t.Fatalf("期望可识别错误 ErrEngineUnavailable，得到 %v", err)
		}
		if got := resp.count(); got != 3 {
			t.Fatalf("重试必须恰好等于上限 3（不无限重放），得到 %d", got)
		}
		if texts, cards := tr.sent(); texts != 0 || cards != 0 {
			t.Fatalf("引擎不可用不得下发任何内容：texts=%d cards=%d", texts, cards)
		}
		if len(g.Outbox()) != 0 {
			t.Fatal("引擎不可用时 dry-run outbox 也不得有内容")
		}
	})

	t.Run("上限缺省为单次不重放", func(t *testing.T) {
		resp := &recordingResponder{alwaysErr: errors.New("宿主侧 连不上")}
		g := mustGateway(t, GatewayConfig{AppID: "app1", Directory: aliceDir(), Responder: resp})

		if err := g.Handle(context.Background(), dmEvent("e1", "tt-alice", "hi")); !errors.Is(err, ErrEngineUnavailable) {
			t.Fatalf("期望 ErrEngineUnavailable，得到 %v", err)
		}
		if got := resp.count(); got != 1 {
			t.Fatalf("缺省必须有上限（1 次），得到 %d", got)
		}
	})
}

// ---------- 事件 → 会话 → 问答 → 下发 ----------

func TestGateway_HappyPathSplitAndCardDelivered(t *testing.T) {
	tr := &fakeTransport{name: "fake"}
	long := strings.Repeat("a", 40) + "\n\n" + strings.Repeat("b", 40)
	resp := &recordingResponder{reply: Reply{
		Texts:   []string{long},
		Cards:   []Card{NewCard("结论结果", "摘要", CardAction{Text: "结论详情"})},
		TraceID: "t-1",
		Context: json.RawMessage(`{"memory":{"revision":1}}`),
	}}
	g := mustGateway(t, GatewayConfig{
		AppID: "app1", Directory: aliceDir(), Responder: resp, Transport: tr, MaxTextBytes: 50,
	})

	if err := g.Handle(context.Background(), dmEvent("e1", "tt-alice", "research:1.2.3.4")); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(tr.texts) != 2 {
		t.Fatalf("长文本应按 50 字节分段下发 2 段，得到 %d：%v", len(tr.texts), tr.texts)
	}
	if len(tr.cards) != 1 {
		t.Fatalf("卡片应下发 1 张，得到 %d", len(tr.cards))
	}
	if st := g.Stats(); st.Handled != 1 || st.Sent != 3 || st.Failed != 0 {
		t.Fatalf("计数漂移：%+v", st)
	}
	key := SessionKey{AppID: "app1", ChatType: ChatTypeDM, ChatID: "tt-alice"}
	sess, ok := g.Store().Get(key)
	if !ok || sess.TraceID != "t-1" {
		t.Fatalf("成功一轮后应回写 TraceID：%+v / ok=%v", sess, ok)
	}
}

// TestGateway_InvalidCardFailClosed 空卡片宁可不下发（fail-closed），
// 而不是让用户看到一张无从判断的空白卡片。
func TestGateway_InvalidCardFailClosed(t *testing.T) {
	tr := newCountingTransport(0)
	resp := &recordingResponder{reply: Reply{Cards: []Card{{}}}} // 无 body
	g := mustGateway(t, GatewayConfig{AppID: "app1", Directory: aliceDir(), Responder: resp, Transport: tr})

	err := g.Handle(context.Background(), dmEvent("e1", "tt-alice", "hi"))
	if !errors.Is(err, ErrDeliveryFailed) {
		t.Fatalf("期望 ErrDeliveryFailed，得到 %v", err)
	}
	if texts, cards := tr.sent(); texts != 0 || cards != 0 {
		t.Fatalf("空卡片不得下发：texts=%d cards=%d", texts, cards)
	}
}

// TestGateway_SessionRoundTripEchoesContext 会话只存「下次要原样带回的东西」，
// 且空回显不擦除上一代。
func TestGateway_SessionRoundTripEchoesContext(t *testing.T) {
	resp := &recordingResponder{reply: Reply{
		Texts:   []string{"第一轮"},
		TraceID: "t-1",
		Context: json.RawMessage(`{"memory":{"revision":1}}`),
	}}
	g := mustGateway(t, GatewayConfig{AppID: "app1", Directory: aliceDir(), Responder: resp, Transport: newCountingTransport(0)})

	if err := g.Handle(context.Background(), dmEvent("e1", "tt-alice", "第一句")); err != nil {
		t.Fatalf("第一轮: %v", err)
	}
	resp.mu.Lock()
	resp.reply = Reply{Texts: []string{"第二轮"}} // 引擎未回显 ⇒ 保留上一代
	resp.mu.Unlock()

	if err := g.Handle(context.Background(), dmEvent("e2", "tt-alice", "第二句")); err != nil {
		t.Fatalf("第二轮: %v", err)
	}
	if got := resp.lastTraceID(); got != "t-1" {
		t.Fatalf("第二轮应带回上一轮 TraceID，得到 %q", got)
	}
	if got := string(resp.lastContext()); got != `{"memory":{"revision":1}}` {
		t.Fatalf("第二轮应带回上一轮回显上下文，得到 %s", got)
	}
	key := SessionKey{AppID: "app1", ChatType: ChatTypeDM, ChatID: "tt-alice"}
	sess, _ := g.Store().Get(key)
	if sess.TraceID != "t-1" {
		t.Fatalf("空回显不得擦除上一代：%+v", sess)
	}
	if string(sess.Context) != `{"memory":{"revision":1}}` {
		t.Fatalf("空回显不得擦除上下文：%s", sess.Context)
	}
}

// ---------- 事件流装配与失败隔离 ----------

func TestGateway_RunIsolatesPerEventFailure(t *testing.T) {
	tr := newCountingTransport(4)
	tr.ch <- dmEvent("e1", "tt-nobody", "hi")              // 未映射 ⇒ 单条失败
	tr.ch <- dmEvent("e2", "tt-alice", "research:1.2.3.4") // 正常
	close(tr.ch)

	resp := &recordingResponder{reply: Reply{Texts: []string{"ok"}}}
	g := mustGateway(t, GatewayConfig{AppID: "app1", Directory: aliceDir(), Responder: resp, Transport: tr})

	if err := g.Run(context.Background()); err != nil {
		t.Fatalf("单条事件失败不得终止循环（失败隔离）：%v", err)
	}
	if texts, cards := tr.sent(); texts != 1 || cards != 0 {
		t.Fatalf("只有合法事件应下发：texts=%d cards=%d", texts, cards)
	}
	if st := g.Stats(); st.Handled != 1 || st.Failed != 1 || st.Skipped != 0 {
		t.Fatalf("计数漂移：%+v", st)
	}
}

func TestGateway_RunRequiresTransport(t *testing.T) {
	g := mustGateway(t, GatewayConfig{AppID: "app1", Directory: aliceDir(), Responder: &recordingResponder{}})
	if err := g.Run(context.Background()); !errors.Is(err, ErrGatewayConfig) {
		t.Fatalf("无 Transport 的 Run 应回 ErrGatewayConfig，得到 %v", err)
	}
}

func TestGateway_KeepaliveAndMalformedEvents(t *testing.T) {
	g := mustGateway(t, GatewayConfig{AppID: "app1", Directory: aliceDir(), Responder: &recordingResponder{}})

	if err := g.Handle(context.Background(), Event{ID: "k1", Type: EventKeepalive}); err != nil {
		t.Fatalf("心跳应被跳过而非报错：%v", err)
	}
	if err := g.Handle(context.Background(), dmEvent("", "tt-alice", "hi")); !errors.Is(err, ErrInvalidEvent) {
		t.Fatalf("空 event_id 不得进入处理链：%v", err)
	}
	bad := dmEvent("e3", "tt-alice", "hi")
	bad.Message.ChatType = "unknown"
	if err := g.Handle(context.Background(), bad); !errors.Is(err, ErrInvalidEvent) {
		t.Fatalf("畸形消息必须 fail-closed：%v", err)
	}

	st := g.Stats()
	if st.Skipped != 1 || st.Failed != 2 || st.Handled != 0 || st.Received != 3 {
		t.Fatalf("计数漂移：%+v", st)
	}
}

func TestGateway_ConcurrentHandleRace(t *testing.T) {
	dir := DirectoryFunc(func(account string) (string, error) {
		if strings.TrimSpace(account) == "" {
			return "", ErrUserNotMapped
		}
		return "hm-" + account, nil
	})
	g := mustGateway(t, GatewayConfig{
		AppID: "app1", Directory: dir, Responder: DryRunResponder{}, DryRun: true,
	})

	var wg sync.WaitGroup
	for n := 0; n < 8; n++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 25; j++ {
				ev := dmEvent(fmt.Sprintf("e-%d-%d", n, j), fmt.Sprintf("tt-%d-%d", n, j), "hi")
				if err := g.Handle(context.Background(), ev); err != nil {
					t.Errorf("并发 Handle 出错：%v", err)
					return
				}
			}
		}(n)
	}
	wg.Wait()

	if st := g.Stats(); st.Handled != 200 || st.Failed != 0 || st.Sent != 0 {
		t.Fatalf("并发计数漂移：%+v", st)
	}
}

// ---------- dry-run：内置事件走完整链路、零出网 ----------

// TestGateway_DryRunNoEgress 是  的核心证据：
// DryRun=true 时**所有**下发都被短路成内存 outbox，Transport 一次都不被调用。
func TestGateway_DryRunNoEgress(t *testing.T) {
	t.Run("内置事件走完整链路且零下发（Transport 为空）", func(t *testing.T) {
		g := mustGateway(t, GatewayConfig{
			AppID: "app1", Directory: SampleDirectory(), Responder: DryRunResponder{}, DryRun: true,
		})
		handled, err := g.RunSample(context.Background())
		if err != nil {
			t.Fatalf("内置事件干跑不该失败：%v", err)
		}
		if handled != 3 {
			t.Fatalf("内置消息事件应全部走完链路（3 条），得到 %d", handled)
		}
		st := g.Stats()
		if st.Sent != 0 {
			t.Fatalf("dry-run 不得发起任何真实下发，Sent=%d", st.Sent)
		}
		if st.WouldSend == 0 {
			t.Fatal("dry-run 应记录「本应下发」的内容")
		}
		out := g.Outbox()
		if len(out) == 0 {
			t.Fatal("dry-run outbox 应有记录（否则链路没跑完）")
		}
		for _, d := range out {
			if err := d.To.Valid(); err != nil {
				t.Fatalf("outbox 目标必须可寻址：%v", err)
			}
		}
	})

	t.Run("短路所有 Transport 调用（即使注入了真实传输）", func(t *testing.T) {
		tr := newCountingTransport(0)
		g := mustGateway(t, GatewayConfig{
			AppID: "app1", Directory: SampleDirectory(), Responder: DryRunResponder{}, Transport: tr, DryRun: true,
		})
		if err := g.Handle(context.Background(), dmEvent("e1", "tt-alice", "hi")); err != nil {
			t.Fatalf("Handle: %v", err)
		}
		if texts, cards := tr.sent(); texts != 0 || cards != 0 {
			t.Fatalf("dry-run 不得触碰 Transport：texts=%d cards=%d", texts, cards)
		}
		if st := g.Stats(); st.Sent != 0 || st.WouldSend == 0 {
			t.Fatalf("计数漂移：%+v", st)
		}
	})

	t.Run("探针实证：同一传输在非 dry-run 下确实被调用", func(t *testing.T) {
		tr := newCountingTransport(0)
		g := mustGateway(t, GatewayConfig{
			AppID: "app1", Directory: SampleDirectory(), Responder: DryRunResponder{}, Transport: tr,
		})
		if err := g.Handle(context.Background(), dmEvent("e1", "tt-alice", "hi")); err != nil {
			t.Fatalf("Handle: %v", err)
		}
		texts, cards := tr.sent()
		if texts == 0 || cards == 0 {
			t.Fatalf("计数传输未被调用 ⇒ 上面的 0 可能是空转：texts=%d cards=%d", texts, cards)
		}
		if st := g.Stats(); st.Sent == 0 {
			t.Fatalf("非 dry-run 应计真实下发：%+v", st)
		}
	})
}

// TestSampleEvents_CoverThreeChatTypesAndAreMapped 内置样本事件必须覆盖三类会话，
// 且与 SampleDirectory 配套（否则干跑会以「未映射」失败）。
func TestSampleEvents_CoverThreeChatTypesAndAreMapped(t *testing.T) {
	seen := map[string]bool{}
	keepalive := 0
	for _, ev := range SampleEvents() {
		if !ev.IsMessage() {
			keepalive++
			continue
		}
		seen[ev.Message.ChatType] = true
		if _, err := SampleDirectory().HMUser(ev.Message.UserID); err != nil {
			t.Fatalf("内置事件账号必须与 SampleDirectory 配套：%v", err)
		}
	}
	for _, want := range []string{ChatTypeDM, ChatTypeGroup, ChatTypeChannel} {
		if !seen[want] {
			t.Fatalf("内置事件缺少会话类型 %q：%v", want, seen)
		}
	}
	if keepalive == 0 {
		t.Fatal("内置事件应含心跳（证明生命周期事件被跳过）")
	}
}

func TestDirectoryFromMap_FailClosed(t *testing.T) {
	dir := DirectoryFromMap(map[string]string{
		"tt-alice": " hm-alice ", // 两侧空白应被规范化
		"   ":      "hm-x",       // 空键不得命中
		"tt-bob":   "   ",        // 空值不得命中
	})
	if got, err := dir.HMUser("tt-alice"); err != nil || got != "hm-alice" {
		t.Fatalf("normalize: got %q err %v", got, err)
	}
	for _, bad := range []string{"tt-bob", "", "  ", "tt-unknown"} {
		if _, err := dir.HMUser(bad); !errors.Is(err, ErrUserNotMapped) {
			t.Fatalf("账号 %q 必须 fail-closed，得到 %v", bad, err)
		}
	}
}

// TestGateway_SendCardFailsWithoutCodec 锁住 fail-closed：没注入编码位时卡片**发不出去**，
// 而不是退回包内标签碰运气 —— 那种静默兜底会让"谁在定义线格式"重新变得含糊。
func TestGateway_SendCardFailsWithoutCodec(t *testing.T) {
	g := &Gateway{cfg: GatewayConfig{Transport: &countingTransport{}, MaxAttempts: 1}}
	err := g.sendCard(context.Background(), ChatRef{ID: "u1", Type: ChatTypeDM}, NewCard("标题", "正文"))
	if err == nil || !strings.Contains(err.Error(), "CardCodec") {
		t.Fatalf("缺编码位应失败并指名 CardCodec，实得 %v", err)
	}
}
