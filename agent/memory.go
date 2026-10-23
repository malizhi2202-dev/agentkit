package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/malizhi2202-dev/agentkit/kitctx"
)

// 本文件是**会话记忆**的唯一归属：
// 四类分组键、来源标注、就地纠正与删除、上下文压缩、冲突标注。
//
// 边界（kitctx 只提供不透明的 MemoryHandle，刻意不复制本结构 —— 既有教训 · 契约 2）：
//   - 记忆由调用方每次显式携带、随响应回显；本包**不落库、不写盘、无服务端会话**（框架裁决）；
//   - 记忆是不可信输入：它由调用方携带，下游出域前必须与用户输入同样过围栏（AD-15 / 既有教训）。
//
// 依赖方向：本包可 import 同属 agentkit 的 kitctx（框架裁决），但不得 import 任何
// 宿主业务包（零依赖红线， 守卫）。

// GroupKey 是会话记忆的四类分组键。
//
// 分组键是**闭集**：只允许 GroupEntity / GroupConclusion / GroupTask / GroupPreference
// 四个常量，其字面值即 $.data.memory.groups 的 key，恰好 4 个。
type GroupKey string

// 四类分组键的字面值。
const (
	GroupEntity     GroupKey = "entity"
	GroupConclusion GroupKey = "conclusion"
	GroupTask       GroupKey = "task"
	GroupPreference GroupKey = "preference"
)

// GroupKeys 按固定顺序返回四类分组键：entity / conclusion / task / preference。
//
// 顺序是契约的一部分：items[] 与派生视图都按该顺序展平，保证同请求重复发送得到同结果。
func GroupKeys() []GroupKey {
	return []GroupKey{GroupEntity, GroupConclusion, GroupTask, GroupPreference}
}

// Valid 判定分组键是否属于闭集。未知分组必须被拒绝，而不是静默落入某个分组。
func (g GroupKey) Valid() bool {
	for _, key := range GroupKeys() {
		if g == key {
			return true
		}
	}
	return false
}

// 来源枚举的两个前缀：round:<n> / tool:<name>。
const (
	SourcePrefixRound = "round:"
	SourcePrefixTool  = "tool:"
)

// RoundSource 生成「第 n 轮对话」的来源标注 round:<n>。轮次从 1 开始计数；
// RoundSource(0) 产生的取值不合法（ValidSource 会拒绝），调用方须传真实轮次。
func RoundSource(n int) string {
	return SourcePrefixRound + strconv.Itoa(n)
}

// ToolSource 生成「某次工具调用」的来源标注 tool:<name>。
func ToolSource(name string) string {
	return SourcePrefixTool + name
}

// ValidSource 判定来源标注是否属于  的闭集 {round:<n>, tool:<name>}。
//
// 来源必须显式携带：来源不明的记忆**不得静默入库**（框架裁决 · 决策 3），
// 因为「这条记忆是从哪来的」是用户复查与纠正它的唯一依据。
func ValidSource(source string) bool {
	switch {
	case strings.HasPrefix(source, SourcePrefixRound):
		n, err := strconv.Atoi(strings.TrimPrefix(source, SourcePrefixRound))
		return err == nil && n > 0
	case strings.HasPrefix(source, SourcePrefixTool):
		return strings.TrimSpace(strings.TrimPrefix(source, SourcePrefixTool)) != ""
	default:
		return false
	}
}

// MemoryItem 是一条记忆条目（$.data.memory.items[]）。
type MemoryItem struct {
	// ID 是条目在本次会话内的稳定标识，由 Memory 分配（Add 忽略调用方传入的 ID）；
	// 就地编辑 / 删除都以它为靶点。
	ID string `json:"id"`
	// Group 是四类分组键之一；取值非法即拒绝入库。
	Group GroupKey `json:"group"`
	// Subject 是该条记忆所述的主体，**冲突归并键**：同一分组内同一 Subject 出现不同 Value
	// 即判为矛盾。事实性结论填被述实体标识；实体条目通常留空。
	Subject string `json:"subject,omitempty"`
	// EntityType 是实体类型（仅 entity 组使用），供记忆面板跳既有实体详情页；
	// 其余分组留空。
	EntityType string `json:"entity_type,omitempty"`
	// Value 是记忆的主文本（实体取值 / 结论 / 任务 / 偏好）。
	Value string `json:"value"`
	// Source 是来源标注，必须 ∈ {round:<n>, tool:<name>}。
	Source string `json:"source"`
}

// Groups 是四类分组的派生视图。它是**视图而非存储**：
// 由 Memory.Items 按分组键派生，因此不可能出现「条目落在四组之外」的漂移。
type Groups struct {
	Entity     []MemoryItem `json:"entity"`
	Conclusion []MemoryItem `json:"conclusion"`
	Task       []MemoryItem `json:"task"`
	Preference []MemoryItem `json:"preference"`
}

// ConflictEntry 是矛盾记录中的一方。
type ConflictEntry struct {
	ID     string `json:"id"`
	Value  string `json:"value"`
	Source string `json:"source"`
}

// Conflict 是同主体矛盾记录：同一分组内同一 Subject 出现了不同 Value。
//
// 冲突由条目**派生**（不单独存储）：因此引擎**永远不会自动择一** —— 两条结论都留在
// groups 里，用户就地编辑 / 删除其中一条后冲突自然消失。
type Conflict struct {
	Group   GroupKey        `json:"group"`
	Subject string          `json:"subject"`
	Items   []ConflictEntry `json:"items"`
}

// CompressionNotice 是会话超模型上下文窗口时的显式告知文案。
const CompressionNotice = "已对早期对话做摘要"

// 记忆的哨兵错误：调用方用 errors.Is 判定，不解析文案。
var (
	// ErrUnknownGroup 表示分组键不在四类闭集内。
	ErrUnknownGroup = errors.New("agent: 未知记忆分组（只允许 entity / conclusion / task / preference）")
	// ErrEmptyValue 表示记忆条目的主文本为空。
	ErrEmptyValue = errors.New("agent: 记忆条目内容为空")
	// ErrInvalidSource 表示来源标注不在闭集 {round:<n>, tool:<name>} 内。
	ErrInvalidSource = errors.New("agent: 记忆来源不合法（只允许 round:<n> / tool:<name>）")
	// ErrItemNotFound 表示要编辑 / 删除的条目不存在。
	ErrItemNotFound = errors.New("agent: 记忆条目不存在")
	// ErrMemoryPayload 表示调用方携带的记忆句柄载荷不可用。
	ErrMemoryPayload = errors.New("agent: 记忆句柄载荷不可用")
)

// memoryWire 固定 $.data.memory 的四个字段：groups / items / conflicts / compressed。
// 自定义序列化以它为准，因此内存形状与线上形状只有一处定义。
type memoryWire struct {
	Groups     Groups       `json:"groups"`
	Items      []MemoryItem `json:"items"`
	Conflicts  []Conflict   `json:"conflicts"`
	Compressed bool         `json:"compressed"`
}

// Memory 是一次会话的记忆载体。
//
// 单一真相源是 Items：groups 与 conflicts 都是**派生视图**（分别在 Groups / Conflicts 与
// MarshalJSON 中产出），因此「四类分组恰 4 个 key」「不自动择一」在结构上无法被破坏。
//
// 记忆由调用方随请求携带、随响应回显，引擎不持有任何会话状态（AD-13 / AD-15）：
// 新会话以空记忆开始，v1 无跨会话长期记忆。Items 只允许经 Add / Edit /
// Delete / Clear 变更，以便来源与分组校验不被绕过。
type Memory struct {
	// Items 是全部记忆条目（items[]）。JSON 外形由 MarshalJSON 固定为  的四字段。
	Items []MemoryItem `json:"items"`
	// Compressed 表示本次会话已以记忆作压缩上下文继续。
	Compressed bool `json:"compressed"`
	// Revision 是回显修订号：只进 kitctx.MemoryHandle，不进  的 memory JSON 形状。
	// 任何一次变更都会递增，调用方据此判断回显是否最新。
	Revision int64 `json:"-"`
}

// NewMemory 返回空记忆：新会话从空记忆开始。
func NewMemory() *Memory {
	return &Memory{Items: []MemoryItem{}}
}

// Add 追加一条记忆并返回落库条目（含由 Memory 分配的稳定 ID）。
//
// 「新一轮提到的实体自动入记忆」即由此落地。调用方传入的 ID 被忽略；
// 分组非法、内容为空、来源不合法（含缺来源）一律拒绝，不静默入库。
func (m *Memory) Add(item MemoryItem) (MemoryItem, error) {
	if err := item.validate(); err != nil {
		return MemoryItem{}, err
	}
	item.ID = m.nextID(item.Group)
	m.Items = append(m.Items, item)
	m.Revision++
	return item, nil
}

// Edit 就地纠正一条记忆的主文本并返回新条目：立即生效，返回体回显新值，
// 下一轮按新记忆解析，无需重开会话。
//
// 只纠正 Value（记错了的是内容）；来源与分组保持原样，避免把「纠正」变成伪造来源。
func (m *Memory) Edit(id, value string) (MemoryItem, error) {
	if strings.TrimSpace(value) == "" {
		return MemoryItem{}, fmt.Errorf("agent: 纠正后的内容不得为空: %w", ErrEmptyValue)
	}
	for i := range m.Items {
		if m.Items[i].ID != id {
			continue
		}
		m.Items[i].Value = value
		m.Revision++
		return m.Items[i], nil
	}
	return MemoryItem{}, fmt.Errorf("agent: 纠正条目 %q: %w", id, ErrItemNotFound)
}

// Delete 删除一条记忆并返回被删条目：立即生效，条目从 items 与所属分组同时消失，
// 由它参与的矛盾也随之消失。
func (m *Memory) Delete(id string) (MemoryItem, error) {
	for i := range m.Items {
		if m.Items[i].ID != id {
			continue
		}
		removed := m.Items[i]
		m.Items = append(m.Items[:i], m.Items[i+1:]...)
		m.Revision++
		return removed, nil
	}
	return MemoryItem{}, fmt.Errorf("agent: 删除条目 %q: %w", id, ErrItemNotFound)
}

// Clear 清空全部记忆：items==[]，并回到「未压缩」的空记忆状态，
// 对话从空记忆继续。
func (m *Memory) Clear() {
	m.Items = nil
	m.Compressed = false
	m.Revision++
}

// Groups 按四类分组键返回记忆的**派生视图**。返回的四个切片恒非 nil
// （空分组序列化为 []，不是 null）。
func (m Memory) Groups() Groups {
	groups := Groups{
		Entity:     []MemoryItem{},
		Conclusion: []MemoryItem{},
		Task:       []MemoryItem{},
		Preference: []MemoryItem{},
	}
	for _, item := range m.Items {
		switch item.Group {
		case GroupEntity:
			groups.Entity = append(groups.Entity, item)
		case GroupConclusion:
			groups.Conclusion = append(groups.Conclusion, item)
		case GroupTask:
			groups.Task = append(groups.Task, item)
		case GroupPreference:
			groups.Preference = append(groups.Preference, item)
		}
	}
	return groups
}

// Conflicts 派生同主体矛盾记录：同一分组内同一非空 Subject 出现 ≥2 个不同
// Value 即构成矛盾。矛盾各方都保留在 Items 中 —— **不自动择一**，交用户裁决。
//
// 输出按分组固定顺序、组内按主体首次出现顺序排列（确定性）。
func (m Memory) Conflicts() []Conflict {
	conflicts := []Conflict{}
	for _, group := range GroupKeys() {
		subjects := []string{}
		entries := map[string][]ConflictEntry{}
		for _, item := range m.Items {
			if item.Group != group || item.Subject == "" {
				continue
			}
			if _, seen := entries[item.Subject]; !seen {
				subjects = append(subjects, item.Subject)
			}
			entries[item.Subject] = append(entries[item.Subject], ConflictEntry{
				ID:     item.ID,
				Value:  item.Value,
				Source: item.Source,
			})
		}
		for _, subject := range subjects {
			if distinctValues(entries[subject]) < 2 {
				continue
			}
			conflicts = append(conflicts, Conflict{Group: group, Subject: subject, Items: entries[subject]})
		}
	}
	return conflicts
}

// Compress 判定并执行上下文压缩。
//
// used / window 是同一计量单位下的「当前会话长度」与「模型上下文窗口上限」，由调用方注入
// （本层不做分词，也不补默认值 —— 既有教训 · 契约 3 / 既有教训 · 契约 1：窗口必须由配置层给出文档化的
// 非零默认值）。超出窗口时：以记忆作压缩上下文继续（**不删除任何条目** ⇒ 关键实体与结论
// 不丢，不静默丢弃），置 Compressed==true，并返回**逐字**告知文案 CompressionNotice。
//
// window <= 0 表示调用方**未声明**窗口：此时框架不猜测、不压缩、不虚报 Compressed==true，
// 返回空串（不得把「未声明」当成「不限」来自我安慰）。
func (m *Memory) Compress(used, window int) string {
	if window <= 0 || used <= window {
		return ""
	}
	if !m.Compressed {
		m.Compressed = true
		m.Revision++
	}
	return CompressionNotice
}

// Handle 把记忆序列化为可随请求携带的句柄（AD-15）：引擎不持有会话状态，
// 记忆由调用方携带、随响应回显。
func (m Memory) Handle() (*kitctx.MemoryHandle, error) {
	payload, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("agent: 记忆序列化失败: %w", err)
	}
	return &kitctx.MemoryHandle{Revision: m.Revision, Payload: payload}, nil
}

// MemoryFromHandle 从调用方携带的句柄恢复记忆：nil 句柄（或空载荷）即新会话 ⇒
// 空记忆，v1 无跨会话长期记忆。载荷不可用时返回 ErrMemoryPayload
// （连同具体的分组 / 来源哨兵错误），不静默降级为空记忆。
func MemoryFromHandle(h *kitctx.MemoryHandle) (*Memory, error) {
	memory := NewMemory()
	if h == nil {
		return memory, nil
	}
	memory.Revision = h.Revision
	if len(h.Payload) == 0 {
		return memory, nil
	}
	if err := json.Unmarshal(h.Payload, memory); err != nil {
		return nil, fmt.Errorf("agent: 记忆句柄载荷不可用: %w: %w", ErrMemoryPayload, err)
	}
	return memory, nil
}

// MarshalJSON 固定 $.data.memory 的形状为  的四字段（groups / items / conflicts / compressed），
// 并保证空集合序列化为 [] 而非 null。条目非法时**报错**而不是产出「落在四组之外」的条目。
func (m Memory) MarshalJSON() ([]byte, error) {
	if err := m.validateItems(); err != nil {
		return nil, err
	}
	items := m.Items
	if items == nil {
		items = []MemoryItem{}
	}
	return json.Marshal(memoryWire{
		Groups:     m.Groups(),
		Items:      items,
		Conflicts:  m.Conflicts(),
		Compressed: m.Compressed,
	})
}

// UnmarshalJSON 以 items 为真相源恢复记忆（groups 是派生视图，缺 items 而 groups 非空即报错，
// 不静默丢条目）。Revision 只随 MemoryHandle 往返，这里保留接收者已有的值。
func (m *Memory) UnmarshalJSON(data []byte) error {
	var wire memoryWire
	if err := json.Unmarshal(data, &wire); err != nil {
		return fmt.Errorf("agent: 记忆载荷解析失败: %w: %w", ErrMemoryPayload, err)
	}
	grouped := len(wire.Groups.Entity) + len(wire.Groups.Conclusion) +
		len(wire.Groups.Task) + len(wire.Groups.Preference)
	if len(wire.Items) == 0 && grouped > 0 {
		return fmt.Errorf("agent: 记忆载荷 groups 非空但 items 缺失（不静默丢条目）: %w", ErrMemoryPayload)
	}

	revision := m.Revision
	decoded := &Memory{Items: make([]MemoryItem, 0, len(wire.Items)), Compressed: wire.Compressed}
	for i, item := range wire.Items {
		if err := item.validate(); err != nil {
			return fmt.Errorf("agent: 记忆载荷 items[%d]: %w: %w", i, ErrMemoryPayload, err)
		}
		decoded.Items = append(decoded.Items, item)
	}
	// 缺 ID 的条目补发稳定 ID（第二遍，确保不与载荷里已有的 ID 撞号）。
	for i := range decoded.Items {
		if decoded.Items[i].ID == "" {
			decoded.Items[i].ID = decoded.nextID(decoded.Items[i].Group)
		}
	}
	decoded.Revision = revision
	*m = *decoded
	return nil
}

// validateItems 校验全部条目，供序列化边界使用。
func (m Memory) validateItems() error {
	for i, item := range m.Items {
		if err := item.validate(); err != nil {
			return fmt.Errorf("agent: items[%d]: %w", i, err)
		}
	}
	return nil
}

// validate 校验单条记忆的分组、内容与来源。
func (item MemoryItem) validate() error {
	if !item.Group.Valid() {
		return fmt.Errorf("agent: 未知记忆分组 %q（只允许 %v）: %w", item.Group, GroupKeys(), ErrUnknownGroup)
	}
	if strings.TrimSpace(item.Value) == "" {
		return fmt.Errorf("agent: 记忆条目内容为空: %w", ErrEmptyValue)
	}
	if !ValidSource(item.Source) {
		return fmt.Errorf("agent: 记忆来源 %q 不合法（只允许 round:<n> / tool:<name>）: %w", item.Source, ErrInvalidSource)
	}
	return nil
}

// nextID 为 group 分配下一个稳定 ID（<group>-<n>，n 取组内已有 ID 的最大序号 +1）。
// ID 由记忆内容派生、不依赖任何进程级计数器 ⇒ 同请求重复发送得到同结果。
func (m *Memory) nextID(group GroupKey) string {
	prefix := string(group) + "-"
	highest := 0
	for _, item := range m.Items {
		if !strings.HasPrefix(item.ID, prefix) {
			continue
		}
		if n, err := strconv.Atoi(strings.TrimPrefix(item.ID, prefix)); err == nil && n > highest {
			highest = n
		}
	}
	return prefix + strconv.Itoa(highest+1)
}

// distinctValues 统计冲突各方出现过的不同取值个数。
func distinctValues(entries []ConflictEntry) int {
	values := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		values[entry.Value] = struct{}{}
	}
	return len(values)
}
