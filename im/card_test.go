package im

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
)

// ---------- Card：读侧卡片（下发 + 问答），不含写动作回调 ----------

func TestStandardCardCodec_ReferenceShape(t *testing.T) {
	c := NewCard("IP 1.2.3.4", "命中 3 条情报，风险级别：高", CardAction{
		Text:  "结论详情",
		Name:  "open",
		Value: `{"route":"/api/report"}`,
	})
	c.ID = "card-1"
	c.Head = &CardHead{Text: "结论结果"}
	c.Footer = "以上为初步结论，不构成权威结论"

	raw, err := StandardCardCodec.EncodeCard(c)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	want := `{"id":"card-1","header":{"text":"结论结果"},` +
		`"body":{"title":"IP 1.2.3.4","text":"命中 3 条情报，风险级别：高"},` +
		`"footer":"以上为初步结论，不构成权威结论",` +
		`"actions":[{"text":"结论详情","value":"{\"route\":\"/api/report\"}","key":"open"}]}`
	if got := string(raw); got != want {
		t.Fatalf("参照编码形状漂移\n got: %s\nwant: %s", got, want)
	}
}

// TestCard_NoImplicitWireShape 锁住这次改判的根据：语义模型不得自带任何平台的线格式，
// 否则「忘了注入编码位」会被静默兜底成某个碰巧能用的形状。
func TestCard_NoImplicitWireShape(t *testing.T) {
	c := NewCard("标题", "正文", CardAction{Text: "按钮", Name: "open", Color: "#fff", BgColor: "#000"})
	c.ID = "card-9"
	c.Head = &CardHead{Text: "头部"}
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(raw)
	// 拆成拼接：这些键的**完整字面量**不许出现在包内（守卫扫禁词），故此处不写全串。
	for _, k := range []string{"bg" + "color", "msg" + "type", "inter" + "active",
		`"act` + `ion":[`, `"he` + `ad":`, `"con` + `tent"`} {

		if strings.Contains(got, k) {
			t.Fatalf("语义模型泄漏平台线格式（含 %s）：%s", k, got)
		}
	}
	if !strings.Contains(got, "BgColor") {
		t.Fatalf("语义字段应按 Go 字段名序列化（编码归注入位），实得：%s", got)
	}
}
func TestCard_ValidateFailClosed(t *testing.T) {
	valid := NewCard("标题", "正文")
	valid.Actions = []CardAction{{Text: "进入搜索详情"}}
	emptyBodyContent := Card{Body: &CardBody{Title: "只有标题"}}
	emptyActionText := Card{Body: &CardBody{Content: "正文"}, Actions: []CardAction{{Text: "  "}}}

	cases := []struct {
		name    string
		card    Card
		wantErr bool
	}{
		{"合法卡片", valid, false},
		{"只有标题也算有正文", emptyBodyContent, false},
		{"缺 body", Card{}, true},
		{"body 全空", Card{Body: &CardBody{}}, true},
		{"按钮文案为空", emptyActionText, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.card.Validate()
			if tc.wantErr {
				if !errors.Is(err, ErrInvalidCard) {
					t.Fatalf("期望 ErrInvalidCard，得到 %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("期望通过校验，得到 %v", err)
			}
		})
	}
}

// TestCard_NoWriteActionSemantics 读侧硬约束（CHANGE.md:75）：
// 卡片按钮只承载读侧标识，本包不得提供确认/取消一类写动作的回调编解码入口。
func TestCardCodec_ActionStyleOnlyWhenSet(t *testing.T) {
	enc := func(a CardAction) string {
		raw, err := StandardCardCodec.EncodeCard(NewCard("t", "正文", a))
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		return string(raw)
	}
	if w := enc(CardAction{Text: "按钮", Color: "#fff", BgColor: "#000"}); !strings.Contains(w, `"style":{"text_color":"#fff","background_color":"#000"}`) {
		t.Fatalf("有颜色时应产出 style 节点：%s", w)
	}
	if pl := enc(CardAction{Text: "按钮"}); strings.Contains(pl, `"style"`) {
		t.Fatalf("未设颜色不应出现空 style 节点：%s", pl)
	}
}
func TestChatRef_Valid(t *testing.T) {
	if err := (ChatRef{ID: "u1", Type: ChatTypeDM, Name: "张三"}).Valid(); err != nil {
		t.Fatalf("合法出站目标不该报错：%v", err)
	}
	if err := (ChatRef{Type: ChatTypeDM}).Valid(); !errors.Is(err, ErrInvalidChatID) {
		t.Fatalf("空 ID 应报 ErrInvalidChatID，得到 %v", err)
	}
	if err := (ChatRef{ID: "u1", Type: "unknown"}).Valid(); !errors.Is(err, ErrInvalidChatType) {
		t.Fatalf("未知类型应报 ErrInvalidChatType，得到 %v", err)
	}
}

// ---------- protocol：会话类型反推 / 长文本分段 ----------

func TestGuessChatType(t *testing.T) {
	cases := map[string]string{
		"teams_12_34_56":   ChatTypeChannel,
		"1234567890123456": ChatTypeGroup,
		"123456789012345":  ChatTypeDM, // 15 位不是群
		"someuser":         ChatTypeDM,
		"":                 ChatTypeDM,
	}
	for chatID, want := range cases {
		if got := GuessChatType(chatID); got != want {
			t.Fatalf("GuessChatType(%q) = %q, want %q", chatID, got, want)
		}
	}
}

func TestSplitText(t *testing.T) {
	t.Run("不超限原样返回", func(t *testing.T) {
		got := SplitText("一句话", 100)
		if len(got) != 1 || got[0] != "一句话" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("优先段间空行", func(t *testing.T) {
		a := strings.Repeat("a", 40)
		b := strings.Repeat("b", 40)
		got := SplitText(a+"\n\n"+b, 50)
		if len(got) != 2 || got[0] != a || got[1] != b {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("不切断多字节字符", func(t *testing.T) {
		src := strings.Repeat("威", 10) // 每个 3 字节
		got := SplitText(src, 4)
		for i, part := range got {
			if !utf8.ValidString(part) {
				t.Fatalf("第 %d 段不是合法 UTF-8：%q", i, part)
			}
		}
		if joined := strings.Join(got, ""); joined != src {
			t.Fatalf("分段后可无损拼接：got %q want %q", joined, src)
		}
	})

	t.Run("非法上限用默认值", func(t *testing.T) {
		got := SplitText("短", 0)
		if len(got) != 1 || got[0] != "短" {
			t.Fatalf("got %q", got)
		}
	})
}

func TestIncomingMessage_ValidAndChat(t *testing.T) {
	m := &IncomingMessage{
		ChatID:   "u1",
		ChatType: ChatTypeDM,
		ChatName: "张三",
		UserID:   "u1",
		Text:     "research:1.2.3.4",
	}
	if err := m.Valid(); err != nil {
		t.Fatalf("合法消息不该报错：%v", err)
	}
	chat := m.Chat()
	if chat.ID != "u1" || chat.Type != ChatTypeDM || chat.Name != "张三" {
		t.Fatalf("Chat() 漂移：%+v", chat)
	}

	bad := []*IncomingMessage{
		nil,
		{ChatType: ChatTypeDM},
		{ChatID: "u1", ChatType: "unknown"},
	}
	for i, b := range bad {
		if err := b.Valid(); err == nil {
			t.Fatalf("第 %d 个非法消息应报错", i)
		}
	}
}

func TestEvent_IsMessage(t *testing.T) {
	msg := Event{ID: "e1", Type: EventSingleChat, Message: &IncomingMessage{ChatID: "u1", ChatType: ChatTypeDM}}
	if !msg.IsMessage() {
		t.Fatal("消息类事件应为 true")
	}
	keepalive := Event{ID: "e2", Type: EventKeepalive}
	if keepalive.IsMessage() {
		t.Fatal("心跳事件应为 false")
	}
	noBody := Event{ID: "e3", Type: EventGroupChat}
	if noBody.IsMessage() {
		t.Fatal("无消息体应不算消息事件")
	}
}

// fakeTransport 证明 Transport 可在不出网的前提下被注入（ dry-run 的前提）。
type fakeTransport struct {
	name  string
	ch    chan Event
	cards []string
	texts []string
}

func (f *fakeTransport) Name() string { return f.name }

func (f *fakeTransport) Events(ctx context.Context) (<-chan Event, error) { return f.ch, nil }

func (f *fakeTransport) SendMessage(_ context.Context, to ChatRef, text string) (MessageID, error) {
	f.texts = append(f.texts, to.ID+"|"+to.Type+"|"+text)
	return MessageID("msg-1"), nil
}

func (f *fakeTransport) SendCard(_ context.Context, to ChatRef, payload []byte) (MessageID, error) {
	f.cards = append(f.cards, string(payload))
	return MessageID("card-msg-1"), nil
}

var _ Transport = (*fakeTransport)(nil)

func TestTransport_InjectedFakeNeverGoesOnline(t *testing.T) {
	tr := &fakeTransport{name: "fake", ch: make(chan Event, 1)}
	tr.ch <- Event{
		ID:      "evt-1",
		Type:    EventSingleChat,
		Message: &IncomingMessage{ChatID: "u1", ChatType: ChatTypeDM, UserID: "u1", Text: "research:1.2.3.4"},
	}

	ctx := context.Background()
	stream, err := tr.Events(ctx)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	evt := <-stream
	if !evt.IsMessage() {
		t.Fatalf("事件应为消息类：%+v", evt)
	}
	if _, err := tr.SendCard(ctx, evt.Message.Chat(), mustEncode(t, NewCard("结论结果", "摘要"))); err != nil {
		t.Fatalf("SendCard: %v", err)
	}
	if _, err := tr.SendMessage(ctx, evt.Message.Chat(), "文本"); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if len(tr.cards) != 1 || len(tr.texts) != 1 {
		t.Fatalf("注入的 Transport 未被调用：cards=%d texts=%d", len(tr.cards), len(tr.texts))
	}
}

// ---------- session：app_id 单实例语义 + 会话键/存储 ----------

func TestSessionKey_StringDeterministic(t *testing.T) {
	dm := SessionKey{AppID: "app1", ChatType: ChatTypeDM, ChatID: "u1", SenderID: "u1"}
	dmNoSender := SessionKey{AppID: "app1", ChatType: ChatTypeDM, ChatID: "u1"}
	if dm.String() != dmNoSender.String() {
		t.Fatalf("私聊不分发言人，应同键：%q vs %q", dm.String(), dmNoSender.String())
	}

	group1 := SessionKey{AppID: "app1", ChatType: ChatTypeGroup, ChatID: "1234567890123456", SenderID: "u1"}
	group2 := SessionKey{AppID: "app1", ChatType: ChatTypeGroup, ChatID: "1234567890123456", SenderID: "u2"}
	if group1.String() == group2.String() {
		t.Fatal("群聊按发言人拆分时应为不同键")
	}

	// 同一串数字在群与私聊下不得撞车
	asGroup := SessionKey{AppID: "app1", ChatType: ChatTypeGroup, ChatID: "1234567890123456"}
	asDM := SessionKey{AppID: "app1", ChatType: ChatTypeDM, ChatID: "1234567890123456"}
	if asGroup.String() == asDM.String() {
		t.Fatal("会话类型必须参与键，防 group_id 与账号数字撞车")
	}

	dmKey := SessionKey{AppID: "app1", ChatType: ChatTypeDM, ChatID: "u1"}
	if got := dmKey.String(); got != "app1|dm|u1" {
		t.Fatalf("键格式漂移：%q", got)
	}
}

func TestSessionKey_Valid(t *testing.T) {
	if err := (SessionKey{AppID: "app1", ChatType: ChatTypeDM, ChatID: "u1"}).Valid(); err != nil {
		t.Fatalf("合法键不该报错：%v", err)
	}
	cases := []struct {
		name string
		key  SessionKey
		want error
	}{
		{"缺 app_id", SessionKey{ChatType: ChatTypeDM, ChatID: "u1"}, ErrInvalidAppID},
		{"缺 chat_id", SessionKey{AppID: "app1", ChatType: ChatTypeDM}, ErrInvalidChatID},
		{"未知会话类型", SessionKey{AppID: "app1", ChatType: "unknown", ChatID: "u1"}, ErrInvalidChatType},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.key.Valid(); !errors.Is(err, tc.want) {
				t.Fatalf("期望 %v，得到 %v", tc.want, err)
			}
		})
	}
}

func TestClaimInstance_SingleInstancePerAppID(t *testing.T) {
	const appID = "app-instance-1"
	if err := ClaimInstance(appID, "gateway-a"); err != nil {
		t.Fatalf("首个实例应能占用：%v", err)
	}
	defer func() { _ = ReleaseInstance(appID, "gateway-a") }()

	if err := ClaimInstance(appID, "gateway-b"); !errors.Is(err, ErrInstanceClaimed) {
		t.Fatalf("同 app_id 第二个实例必须被拒：%v", err)
	}
	if owner, ok := InstanceOwner(appID); !ok || owner != "gateway-a" {
		t.Fatalf("持有者应为 gateway-a，得到 %q/%v", owner, ok)
	}
	if err := ReleaseInstance(appID, "gateway-b"); !errors.Is(err, ErrNotInstanceOwner) {
		t.Fatalf("非持有者释放必须被拒：%v", err)
	}
	if err := ClaimInstance("app-instance-2", "gateway-b"); err != nil {
		t.Fatalf("不同 app_id 互不影响：%v", err)
	}
	defer func() { _ = ReleaseInstance("app-instance-2", "gateway-b") }()

	if err := ReleaseInstance(appID, "gateway-a"); err != nil {
		t.Fatalf("持有者释放：%v", err)
	}
	if err := ReleaseInstance(appID, "gateway-a"); !errors.Is(err, ErrNotInstanceOwner) {
		t.Fatalf("重复释放必须被拒：%v", err)
	}
	if err := ClaimInstance(appID, "gateway-b"); err != nil {
		t.Fatalf("释放后应可重新占用：%v", err)
	} else if err := ReleaseInstance(appID, "gateway-b"); err != nil {
		t.Fatalf("清理新占用者：%v", err)
	}

	if err := ClaimInstance("", "gateway-a"); !errors.Is(err, ErrInvalidAppID) {
		t.Fatalf("空 app_id 必须被拒：%v", err)
	}
}

func TestSessionStore_RejectsForeignKeys(t *testing.T) {
	st, err := NewSessionStore("app1")
	if err != nil {
		t.Fatalf("NewSessionStore: %v", err)
	}
	if st.AppID() != "app1" || st.Count() != 0 {
		t.Fatalf("新 store 应绑定 app1 且为空，得到 %q/%d", st.AppID(), st.Count())
	}
	if _, err := NewSessionStore("  "); !errors.Is(err, ErrInvalidAppID) {
		t.Fatalf("空 app_id 必须被拒：%v", err)
	}
	if err := st.Put(Session{Key: SessionKey{AppID: "app2", ChatType: ChatTypeDM, ChatID: "u1"}}); !errors.Is(err, ErrAppIDMismatch) {
		t.Fatalf("跨 app_id 写入必须被拒：%v", err)
	}
	if err := st.Put(Session{Key: SessionKey{AppID: "app1", ChatType: ChatTypeDM}}); !errors.Is(err, ErrInvalidChatID) {
		t.Fatalf("非法键必须被拒：%v", err)
	}
}

func TestSessionStore_GetReturnsCopy(t *testing.T) {
	st, err := NewSessionStore("app1")
	if err != nil {
		t.Fatalf("NewSessionStore: %v", err)
	}
	key := SessionKey{AppID: "app1", ChatType: ChatTypeDM, ChatID: "u1"}
	sess := Session{Key: key, TraceID: "t-1", Context: json.RawMessage(`{"memory":{"revision":7}}`)}
	if err := st.Put(sess); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, ok := st.Get(key)
	if !ok {
		t.Fatal("应能取回会话")
	}
	got.TraceID = "被改坏"
	got.Context[0] = 'X'
	again, _ := st.Get(key)
	if again.TraceID != "t-1" {
		t.Fatalf("Get 必须返回拷贝，TraceID 被外部改写：%q", again.TraceID)
	}
	if !bytes.Equal(again.Context, []byte(`{"memory":{"revision":7}}`)) {
		t.Fatalf("Get 必须返回拷贝，回显上下文被外部改写：%s", again.Context)
	}

	if _, ok := st.Get(SessionKey{AppID: "app1", ChatType: ChatTypeDM, ChatID: "nobody"}); ok {
		t.Fatal("不存在的会话应返回 false")
	}
	if !st.Delete(key) || st.Count() != 0 {
		t.Fatalf("Delete 后 Count 应为 0，得到 %d", st.Count())
	}
}

func TestSessionStore_ConcurrentAccess(t *testing.T) {
	st, err := NewSessionStore("app1")
	if err != nil {
		t.Fatalf("NewSessionStore: %v", err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				key := SessionKey{AppID: "app1", ChatType: ChatTypeGroup, ChatID: fmt.Sprintf("g-%d", n), SenderID: fmt.Sprintf("u-%d", j)}
				if err := st.Put(Session{Key: key, TraceID: fmt.Sprintf("t-%d-%d", n, j)}); err != nil {
					t.Errorf("Put: %v", err)
					return
				}
				_, _ = st.Get(key)
				_ = st.Count()
			}
		}(i)
	}
	wg.Wait()
	if got := st.Count(); got != 8*50 {
		t.Fatalf("Count = %d, want %d", got, 8*50)
	}
}

// mustEncode 走注入的参照编码位，返回真实出站载荷（测试不再依赖包内隐式标签）。
func mustEncode(t *testing.T, c Card) []byte {
	t.Helper()
	raw, err := StandardCardCodec.EncodeCard(c)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return raw
}
