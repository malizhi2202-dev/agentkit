// Package im 定义IM 平台（IMPlatform）IM 接入的**协议类型**、**Transport 注入接口**与**卡片/会话结构**。
//
// 边界（ / 框架裁决 /  / ）：
//   - 本包是 pkg/agentkit 的框架侧抽象层，只定义「交换什么形状的数据」与「入站/出站往哪里注入」：
//     不读配置、不拼端点、不发请求、不落盘，**不含任何业务 HTTP 调用**；
//   - 真正的 WebSocket / HTTP 传输实现落在独立进程 `cmd/im-gateway`（M4，/），
//     它实现本包 Transport 并在自己的目录内重写传输，使 后端服务 主进程与IM 平台的长连接、
//     外部凭据、抖动完全解耦（后端服务 既有程序零改动）；
//   - **只做读侧**：卡片下发 + 问答。本包**不定义**确认/取消一类写动作的回调语义
//     （`CHANGE.md:75`）——因此没有 callback 类型、没有按钮回调解析入口；
//   - 零 后端服务 依赖：**只用 Go 标准库**，不 import 任何含 `宿主后端服务` 的路径
//     （含同框架的 `pkg/agentkit/<其它包>`——整个 module 路径都带 `宿主后端服务`，故该规则
//     等价于「本包可整目录复制到另一个 module」，NFR-T4 /  守卫合围）。跨包共享的类型
//     若需在此使用，一律以**不透明**形式（如 `json.RawMessage`）承载，不在本包重新定义。
package im

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// 会话类型：与IM 平台三类入口一一对应（对齐参考实现 `internal/im` 的 chat type 口径）。
const (
	// ChatTypeDM 私聊：chat_id 即对方账号。
	ChatTypeDM = "dm"
	// ChatTypeGroup 群聊：chat_id 为 group_id。
	ChatTypeGroup = "group"
	// ChatTypeChannel 频道帖子：chat_id 形如 teams_{team}_{channel}_{thread}。
	ChatTypeChannel = "channel"
)

// 入站事件类型。仅登记读侧关心的事件：消息三类 + 心跳。
const (
	EventSingleChat      = "single_chat"
	EventGroupChat       = "group_chat"
	EventTeamsPostCreate = "teams_post_create"
	EventTeamsPostModify = "teams_post_modify"
	// EventKeepalive 长连接心跳：无业务语义，只需不当作消息处理。
	EventKeepalive = "keepalive"
)

// DefaultMaxTextBytes 是单条出站文本的默认字节上限（对齐参考实现口径）。
//
// 真实上限由 `cmd/im-gateway` 从配置注入；本包只在调用方传 ≤0 时用它兜底，
// **不读配置**（配置层职责，同 kitctx.Limits 的口径）。
const DefaultMaxTextBytes = 20000

// 本包哨兵错误。调用方用 errors.Is 判定，**不要**比对错误文本。
var (
	// ErrInvalidChatID 会话 ID 为空。
	ErrInvalidChatID = errors.New("im: 会话 ID 为空")
	// ErrInvalidChatType 会话类型不在 dm/group/channel 之内。
	ErrInvalidChatType = errors.New("im: 未知的会话类型")
)

// MessageID 是一条已下发消息的标识（日志/诊断与「这条卡片对应哪次问答」的关联键）。
type MessageID string

// ChatRef 是一次出站的目标会话（ID + 类型 + 展示名）。
//
// 与 IncomingMessage 分开而不是复用它：出站只需要「往哪儿发」，携带正文/发送者
// 等入站字段会让 Transport 实现有机会误用入站数据。
type ChatRef struct {
	// ID 是 chat_id（私聊=对方账号，群=group_id，频道=teams_...）。
	ID string
	// Type ∈ {ChatTypeDM, ChatTypeGroup, ChatTypeChannel}。
	Type string
	// Name 是群名/频道名/昵称，仅用于展示与日志，可为空。
	Name string
}

// Valid 校验出站目标是否可寻址。
func (c ChatRef) Valid() error {
	if strings.TrimSpace(c.ID) == "" {
		return fmt.Errorf("%w: 出站目标 ID 为空", ErrInvalidChatID)
	}
	if !knownChatType(c.Type) {
		return fmt.Errorf("%w: %q", ErrInvalidChatType, c.Type)
	}
	return nil
}

// IncomingMessage 是归一化后的入站消息：文本/图片/语音/文件都折进 Text。
//
// 归一化（把IM 平台各类 msg_type 折成 Text + MediaURLs）属**传输实现**的职责，
// 由 `cmd/im-gateway` 的 Transport 完成：本包只消费「已经归一化好的消息」，
// 因此这里不出现任何原始帧字段（Raw/map[string]any）。
type IncomingMessage struct {
	// ChatID 私聊=发送者账号；群聊=group_id；频道=teams_...（与 ChatRef.ID 同义）。
	ChatID string
	// ChatType ∈ {ChatTypeDM, ChatTypeGroup, ChatTypeChannel}。
	ChatType string
	// ChatName 群名/频道名（私聊是发送者名）。
	ChatName string
	// UserID 发送者账号。
	UserID string
	// UserName 发送者昵称。
	UserName string
	// MessageID 该消息的标识；仅用于日志关联与去重，读侧不做原地更新。
	MessageID string
	// Text 正文；非文本消息是 "[图片] http://..." 之类的占位。
	Text string
	// MediaURLs 图片/语音/文件的可下载 URL。
	MediaURLs []string
	// AtMe 群里是否 @ 了本机器人。
	AtMe bool
	// ReplyTo 被引用消息的正文（由传输实现拼入，便于模型理解"这条指的是什么"）。
	ReplyTo string
}

// Valid 判定这条消息是否可进入处理链（fail-closed：不确定就不处理）。
func (m *IncomingMessage) Valid() error {
	if m == nil {
		return errors.New("im: 入站消息为空")
	}
	if strings.TrimSpace(m.ChatID) == "" {
		return fmt.Errorf("%w: 入站消息 chat_id 为空", ErrInvalidChatID)
	}
	if !knownChatType(m.ChatType) {
		return fmt.Errorf("%w: %q", ErrInvalidChatType, m.ChatType)
	}
	return nil
}

// Chat 取出这条消息对应的会话（出站回复与查会话都用它）。
func (m *IncomingMessage) Chat() ChatRef {
	if m == nil {
		return ChatRef{}
	}
	return ChatRef{ID: m.ChatID, Type: m.ChatType, Name: m.ChatName}
}

// Event 是归一化后的入站事件：消息 + 生命周期（心跳等）。
//
// 事件 ID 是**去重与 ACK 的键**（IM 平台未收到 ACK 会重投）。ACK 与 平台事件 ID字段 去重都属
// 传输实现的职责（它持有长连接）：本包只保证把 ID 带出来，不在这里实现 ACK 协议。
type Event struct {
	// ID 即IM 平台帧里的 平台事件 ID字段；为空的事件不应进入处理链。
	ID string
	// Type 是事件类型（EventSingleChat / EventKeepalive / ...）。
	Type string
	// At 是接收时刻。
	At time.Time
	// Message 仅消息类事件非 nil（心跳等事件为 nil）。
	Message *IncomingMessage
}

// IsMessage 判定这是否是一条可处理的消息事件。
func (e Event) IsMessage() bool { return e.Message != nil && knownChatType(e.Message.ChatType) }

// Transport 是 IM 出站/入站传输的**注入接口**。
//
// 之所以做成接口而不是直接用 websocket/http（与 model.HTTPDoer 同一理由）：
//  1. 框架零依赖 ⇒ 本包只能用 stdlib，真实传输必须由 `cmd/im-gateway` 注入；
//  2. 传输实现要持有长连接、凭据与重连策略，这些**不得**进入框架与会话层；
//  3. 自测可注入 fake（含  的 `--dry-run`：内置事件走完整链路、**零出网**），不打真实网络。
//
// 实现约定：
//   - 实现方负责 ACK 与 平台事件 ID字段 去重（参考实现口径：收到帧先 ACK 再分发，否则服务端重投）；
//   - 实现方负责失败隔离与**有上限**的重试，不得无限重放；
//   - 凭据（app_secret / 会话口令）只出现在请求体或 header，**一律不落日志**（框架裁决 第 4 条）；
//   - 本接口不含分页、限流、重试参数：那些是实现的内部策略，不属于框架契约。
type Transport interface {
	// Name 返回实现标识（如 "implatform-ws"），仅用于日志与诊断；不得包含凭据。
	Name() string
	// Events 返回入站事件流；实现负责长连接与重连，ctx 取消时关闭通道。
	// 返回错误表示**无法建立**事件流（配置缺失/握手失败），而非「暂时没消息」。
	Events(ctx context.Context) (<-chan Event, error)
	// SendMessage 下发一条文本消息（长文本由调用方先用 SplitText 分段）。
	SendMessage(ctx context.Context, to ChatRef, text string) (MessageID, error)
	// SendCard 下发一张交互卡片：payload 由 Gateway 用注入的 CardCodec 编码得到，
	// 实现方只管加上平台信封并发送，不必也不应知道卡片的语义字段）。
	SendCard(ctx context.Context, to ChatRef, payload []byte) (MessageID, error)
}

// knownChatType 判定会话类型是否在受支持集合内。
func knownChatType(t string) bool {
	switch t {
	case ChatTypeDM, ChatTypeGroup, ChatTypeChannel:
		return true
	default:
		return false
	}
}

// GuessChatType 由 chat_id 反推会话类型（出站时若只拿到 chat_id 用）。
//
// 规则沿用参考实现 `guess_chat_type`：`teams_` 前缀=频道，16 位纯数字=群，其余=私聊。
func GuessChatType(chatID string) string {
	switch {
	case strings.HasPrefix(chatID, "teams_"):
		return ChatTypeChannel
	case len(chatID) == 16 && isDigits(chatID):
		return ChatTypeGroup
	default:
		return ChatTypeDM
	}
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// SplitText 把长文本按**字节**上限分段（IM 平台按 UTF-8 字节限制消息长度）。
//
// 断点优先级：段间空行 → 换行 → 硬切；硬切会回退到 UTF-8 字符边界，
// 绝不把多字节字符切成乱码。`maxBytes <= 0` 时用 DefaultMaxTextBytes。
//
// 空文本返回 nil（不发空消息），而非 [""]。
func SplitText(content string, maxBytes int) []string {
	if strings.TrimSpace(content) == "" {
		return nil
	}
	if maxBytes <= 0 {
		maxBytes = DefaultMaxTextBytes
	}
	if len(content) <= maxBytes {
		return []string{content}
	}
	var chunks []string
	rest := content
	for len(rest) > maxBytes {
		cut := safeCut(rest, maxBytes, "\n\n")
		if cut <= 0 {
			cut = safeCut(rest, maxBytes, "\n")
		}
		if cut <= 0 {
			cut = runeSafeCut(rest, maxBytes)
		}
		chunks = append(chunks, rest[:cut])
		rest = strings.TrimLeft(rest[cut:], "\n")
	}
	if strings.TrimSpace(rest) != "" {
		chunks = append(chunks, rest)
	}
	return chunks
}

// safeCut 在 limit 字节内找最后一次出现的 sep，返回其下标；找不到或结果为空返回 -1。
func safeCut(s string, limit int, sep string) int {
	if len(s) < limit {
		limit = len(s)
	}
	cut := strings.LastIndex(s[:limit], sep)
	if cut <= 0 {
		return -1
	}
	return cut
}

// runeSafeCut 把硬切点回退到 UTF-8 字符边界。
//
// cut 可能等于 len(s)（调用方语义为「切到末尾」），此时无需回退。
func runeSafeCut(s string, cut int) int {
	if cut > len(s) {
		cut = len(s)
	}
	for cut > 0 && cut < len(s) && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return cut
}
