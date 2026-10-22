package im

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ErrInvalidCard 卡片不可下发（缺正文 / 空按钮文案等）。fail-closed：校验不过就不发。
var ErrInvalidCard = errors.New("im: 卡片不可下发")

// Card 是一张交互卡片的**语义模型**（标题 / 正文 / 页脚 / 按钮），**不含任何平台的线格式**。
//
// 为什么不在这儿定义 JSON 形状：卡片的键名与嵌套层级是**对方平台**的协议，属于接入方的
// 知识。出站前由注入的 CardCodec 编码（见本文件下方编码位），包只负责校验语义与编排下发。
// 参照编码 StandardCardCodec 用的是一组语义中性的键，仅服务测试与样例，不代表任何真实平台。
//
// **只做读侧**（只做读侧）：卡片用于「卡片下发 + 问答」。
// 本包**不定义**确认/取消一类写动作的按钮语义，也不提供任何回调（callback）编解码入口；
// `CardAction.Value` 只承载读侧所需的稳定标识（如"入口命令详情"要跳到哪个路由/报告）。
// 若将来要支持写动作回调，属**新增能力**，需新 task 并重新评审（框架裁决 第 7 条）。
type Card struct {
	// ID 是卡片标识（同一会话内可用于关联后续问答）。
	ID string
	// Head 是卡片头部（一句话结论位，如「入口命令结果」）。
	Head *CardHead
	// Body 是卡片正文；**必填**（一张没有正文的卡片对用户没有任何信息量）。
	Body *CardBody
	// Footer 是脚注（如「以上为初步入口命令，不构成权威结论」，对齐  文案口径）。
	Footer string
	// Actions 是按钮列表（读侧：跳详情 / 追问），wire 字段名沿用IM 平台的 `action`。
	Actions []CardAction
}

// CardHead 卡片头部：一段文本。
type CardHead struct {
	Text string
}

// CardBody 卡片正文：标题 + 内容。
type CardBody struct {
	// Title 可为空（内容足够自明时）。
	Title string
	// Content 是正文主体；与 Title 至少有一个非空。
	Content string
}

// CardAction 卡片按钮。
//
// Text 必填；Value 是回调原样带回的字符串（读侧语义），可为空。
type CardAction struct {
	Text    string
	Name    string
	Value   string
	Color   string
	BgColor string
}

// NewCard 装配一张基础卡片（标题 + 正文 + 若干读侧按钮）。
func NewCard(title, content string, actions ...CardAction) Card {
	return Card{
		Body:    &CardBody{Title: title, Content: content},
		Actions: actions,
	}
}

// Validate 校验卡片是否可下发。
//
// fail-closed：宁可不下发（返回错误，由网关回可识别错误），也不下发一张空卡片——
// 用户看到空白卡片无法判断是「没有数据」还是「链路坏了」。
func (c Card) Validate() error {
	if c.Body == nil {
		return fmt.Errorf("%w: 缺少正文（body）", ErrInvalidCard)
	}
	if strings.TrimSpace(c.Body.Title) == "" && strings.TrimSpace(c.Body.Content) == "" {
		return fmt.Errorf("%w: 正文与标题都为空", ErrInvalidCard)
	}
	for i, a := range c.Actions {
		if strings.TrimSpace(a.Text) == "" {
			return fmt.Errorf("%w: 第 %d 个按钮缺少文案", ErrInvalidCard, i+1)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// 卡片编码（平台字段名归接入方）
// ---------------------------------------------------------------------------

// CardCodec 把语义卡片编码成出站载荷。为什么需要它：卡片的线格式（键名、层级、样式字段）
// 是**对方平台**的协议，属于接入方的知识；包只提供语义模型与下发编排（去重 / ACK / 分段 /
// 失败上抛）。未注入即拒发，绝不退回"用包内标签碰运气"的隐式编码。
type CardCodec interface {
	EncodeCard(Card) ([]byte, error)
}

// CardCodecFunc 让普通函数当 CardCodec 用。
type CardCodecFunc func(Card) ([]byte, error)

// EncodeCard 实现 CardCodec。
func (f CardCodecFunc) EncodeCard(c Card) ([]byte, error) { return f(c) }

// standardWire 是包内的**参照编码**：键名一律语义中立，不代表任何真实平台。
// 它的用途是让单测与样例能跑通"编码→出站"这条链，而不是给某个平台当默认值。
type standardWire struct {
	ID      string       `json:"id,omitempty"`
	Header  *standardTxt `json:"header,omitempty"`
	Body    *standardBod `json:"body,omitempty"`
	Footer  string       `json:"footer,omitempty"`
	Actions []stdAction  `json:"actions,omitempty"`
}
type standardTxt struct {
	Text string `json:"text"`
}
type standardBod struct {
	Title string `json:"title,omitempty"`
	Text  string `json:"text"`
}
type stdAction struct {
	Text  string    `json:"text"`
	Value string    `json:"value,omitempty"`
	Key   string    `json:"key,omitempty"`
	Style *stdStyle `json:"style,omitempty"`
}
type stdStyle struct {
	TextColor  string `json:"text_color,omitempty"`
	Background string `json:"background_color,omitempty"`
}

// StandardCardCodec 是参照编码，仅用于测试与样例；真实接入方必须注入自己的平台编码。
var StandardCardCodec CardCodec = CardCodecFunc(func(c Card) ([]byte, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	w := standardWire{ID: c.ID, Footer: c.Footer}
	if c.Head != nil {
		w.Header = &standardTxt{Text: c.Head.Text}
	}
	if c.Body != nil {
		w.Body = &standardBod{Title: c.Body.Title, Text: c.Body.Content}
	}
	for _, a := range c.Actions {
		sa := stdAction{Text: a.Text, Value: a.Value, Key: a.Name}
		if a.Color != "" || a.BgColor != "" {
			sa.Style = &stdStyle{TextColor: a.Color, Background: a.BgColor}
		}
		w.Actions = append(w.Actions, sa)
	}
	return json.Marshal(w)
})
