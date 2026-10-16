package im

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// 本文件承载与 `app_id` 相关的单实例语义与会话载体（ / ADR-003 第 6 条）。
//
// 「同一 `app_id` 只允许一个网关实例」（`CHANGE.md:76`）在本包内落两件事：
//  1. **进程内**：ClaimInstance 保证同进程内同 app_id 不可被两个实例占用（网关自身重复启动/自测可验）；
//  2. **跨进程**：进程内守卫**无法**覆盖，属**部署约束**（ADR-003 已把同 app_id 单实例登记为
//     明示的单点 R9），由部署侧与 `--check` 探活兜底 —— 此处如实声明边界，不假装做到了分布式锁。

var (
	// ErrInvalidAppID app_id 为空。
	ErrInvalidAppID = errors.New("im: app_id 为空")
	// ErrInstanceClaimed 同一 app_id 已有实例在运行（拒绝第二个实例）。
	ErrInstanceClaimed = errors.New("im: 同一 app_id 已有实例在运行")
	// ErrNotInstanceOwner 释放者不是该 app_id 的持有实例。
	ErrNotInstanceOwner = errors.New("im: 释放者不是该 app_id 的持有实例")
	// ErrAppIDMismatch 会话键的 app_id 与本 store 绑定的 app_id 不一致。
	ErrAppIDMismatch = errors.New("im: 会话键的 app_id 与本实例不一致")
)

// instanceMu / instanceOwners 是进程内的「app_id → 持有实例」表。
var (
	instanceMu     sync.Mutex
	instanceOwners = map[string]string{}
)

// ClaimInstance 以 owner 身份占用某个 app_id。
//
// 同一 owner 重复占用是**幂等成功**（重启路径先 Claim 一次即可，无需判错）；
// 不同 owner 占用同一 app_id 返回 ErrInstanceClaimed（网关据此拒绝启动）。
func ClaimInstance(appID, owner string) error {
	appID = strings.TrimSpace(appID)
	if appID == "" {
		return ErrInvalidAppID
	}
	instanceMu.Lock()
	defer instanceMu.Unlock()
	if cur, ok := instanceOwners[appID]; ok && cur != owner {
		return fmt.Errorf("%w: app_id=%s 已被实例 %q 占用", ErrInstanceClaimed, appID, cur)
	}
	instanceOwners[appID] = owner
	return nil
}

// ReleaseInstance 释放 owner 占用的 app_id（仅持有者可释放）。
func ReleaseInstance(appID, owner string) error {
	appID = strings.TrimSpace(appID)
	if appID == "" {
		return ErrInvalidAppID
	}
	instanceMu.Lock()
	defer instanceMu.Unlock()
	cur, ok := instanceOwners[appID]
	if !ok || cur != owner {
		return fmt.Errorf("%w: app_id=%s", ErrNotInstanceOwner, appID)
	}
	delete(instanceOwners, appID)
	return nil
}

// InstanceOwner 查询某个 app_id 当前的持有实例；未被占用时 ok=false。
func InstanceOwner(appID string) (string, bool) {
	instanceMu.Lock()
	defer instanceMu.Unlock()
	owner, ok := instanceOwners[strings.TrimSpace(appID)]
	return owner, ok
}

// SessionKey 是一条 IM 会话的稳定键。
//
// 私聊的 chat_id 本身就是发送者账号 ⇒ SenderID 不参与键（同一人换设备仍是同一会话）；
// 群聊/频道把 SenderID 纳入键，使「同群不同人」在网关侧天然分开（是否按发言人拆会话
// 由 `cmd/im-gateway` 决定，本包只提供可表达两种口径的键）。
type SessionKey struct {
	// AppID 是IM 平台机器人应用标识；**同一 app_id 单实例**（ADR-003 第 6 条）。
	AppID string
	// ChatType ∈ {ChatTypeDM, ChatTypeGroup, ChatTypeChannel}。
	ChatType string
	// ChatID 私聊=对方账号；群=group_id；频道=teams_...。
	ChatID string
	// SenderID 发送者账号；私聊下不参与键。
	SenderID string
}

// String 返回确定性键（同一输入恒等，可用于日志与 map 键）。
//
// 会话类型必须参与键：否则 16 位 group_id 与同串数字的账号会撞到同一条会话上。
func (k SessionKey) String() string {
	parts := []string{k.AppID, k.ChatType, k.ChatID}
	if k.ChatType != ChatTypeDM && strings.TrimSpace(k.SenderID) != "" {
		parts = append(parts, k.SenderID)
	}
	return strings.Join(parts, "|")
}

// Valid 校验会话键的结构（fail-closed：结构不全就不建会话）。
func (k SessionKey) Valid() error {
	if strings.TrimSpace(k.AppID) == "" {
		return ErrInvalidAppID
	}
	if strings.TrimSpace(k.ChatID) == "" {
		return fmt.Errorf("%w: 会话键 chat_id 为空", ErrInvalidChatID)
	}
	if !knownChatType(k.ChatType) {
		return fmt.Errorf("%w: %q", ErrInvalidChatType, k.ChatType)
	}
	return nil
}

// Session 是一条IM 平台会话在网关侧的载体。
//
// 引擎本身**无状态**（ADR-002）：记忆/上下文由调用方携带并随响应回显，
// 因此这里存的是「下一次请求要原样带回去的东西」——幂等/回溯键 TraceID 与回显上下文 Context。
//
// Context 刻意是**不透明的原始 JSON**（`json.RawMessage`）：
//   - 记忆句柄的类型唯一归 `本包/kitctx`（MemoryHandle），四类记忆分组结构唯一归
//     `本包/agent`（`L-015` 契约 2）——IM 层**不得**另立记忆结构，否则同一结构两处定义必漂移；
//   - 本包又必须零依赖（NFR-T4：本包 各包均 stdlib-only， 守卫禁止任何
//     `host_后端服务` 前缀的 import）⇒ 「原样存、原样带回、不解释结构」是唯一同时满足两者的落法。
type Session struct {
	// Key 是会话键（含 app_id）。
	Key SessionKey
	// TraceID 是与引擎往返的幂等键 / 回溯键；新会话为空。
	TraceID string
	// Context 是上一次响应回显的 `$.data.context` 原文（不透明，本包不解析其内部结构）；
	// 新会话为 nil。
	Context json.RawMessage
	// UpdatedAt 是最后一次写入时刻。
	UpdatedAt time.Time
}

// SessionStore 是某一 app_id 下全部会话的**进程内**集合（并发安全；不落盘）。
//
// 不落盘是刻意的：持久化/多副本属于 `cmd/im-gateway` 的装配选择，框架层不预设存储介质
// （与 「不预设基础设施」的口径一致）。
type SessionStore struct {
	appID    string
	mu       sync.RWMutex
	sessions map[string]Session
}

// NewSessionStore 创建绑定到某个 app_id 的会话集合；app_id 为空返回 ErrInvalidAppID。
func NewSessionStore(appID string) (*SessionStore, error) {
	appID = strings.TrimSpace(appID)
	if appID == "" {
		return nil, ErrInvalidAppID
	}
	return &SessionStore{appID: appID, sessions: map[string]Session{}}, nil
}

// AppID 返回本 store 绑定的 app_id。
func (s *SessionStore) AppID() string { return s.appID }

// Get 取一条会话的**拷贝**；不存在返回 ok=false。
//
// 返回拷贝是硬要求：调用方（网关）会在会话上改写 TraceID / Memory，
// 若共享底层指针，外部改写会静默污染存储（并发下更会 data race）。
func (s *SessionStore) Get(key SessionKey) (Session, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sess, ok := s.sessions[key.String()]
	if !ok {
		return Session{}, false
	}
	return cloneSession(sess), true
}

// Put 写入或更新一条会话。键非法或跨 app_id 一律拒绝。
func (s *SessionStore) Put(sess Session) error {
	if err := sess.Key.Valid(); err != nil {
		return err
	}
	if sess.Key.AppID != s.appID {
		return fmt.Errorf("%w: 会话 %s，本实例 %s", ErrAppIDMismatch, sess.Key.AppID, s.appID)
	}
	if sess.UpdatedAt.IsZero() {
		sess.UpdatedAt = time.Now()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[sess.Key.String()] = cloneSession(sess)
	return nil
}

// Delete 删除一条会话；返回是否确实删掉了。
func (s *SessionStore) Delete(key SessionKey) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := key.String()
	if _, ok := s.sessions[k]; !ok {
		return false
	}
	delete(s.sessions, k)
	return true
}

// Count 当前会话条数（状态与日志用）。
func (s *SessionStore) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.sessions)
}

// cloneSession 复制会话及其回显上下文（浅拷贝 json.RawMessage 会让外部改写穿透到存储）。
func cloneSession(sess Session) Session {
	out := sess
	if len(sess.Context) > 0 {
		out.Context = append(json.RawMessage(nil), sess.Context...)
	}
	return out
}
