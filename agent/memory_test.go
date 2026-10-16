package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	"agentkit/kitctx"
)

//  · RED/GREEN 验收测试：
//   -   四类分组键恰为 entity / conclusion / task / preference（$.data.memory.groups 恰这 4 个 key）
//   -   每条带来源标注 source ∈ {round:<n>, tool:<name>}（ADR-002：来源不明不得静默）
//   -   就地编辑/删除立即生效，下一轮按新值解析（无需重开会话）
//   -   新一轮提到新实体自动入记忆
//   -   超模型上下文窗口 ⇒ 以记忆作压缩上下文继续，compressed==true + 逐字文案「已对早期对话做摘要」
//   -   同实体矛盾结论 ⇒ conflicts[] 非空且不自动择一
//   -   清空 ⇒ items==[]
//   -  新会话从空记忆开始、v1 无跨会话长期记忆
//   - AD-15/ 记忆对象由调用方携带（kitctx.MemoryHandle 为唯一载体）、引擎不落库

// compressionNoticeLiteral 是  的逐字文案（不得改写）。
const compressionNoticeLiteral = "已对早期对话做摘要"

func jsonKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func addMemory(t *testing.T, mem *Memory, item MemoryItem) MemoryItem {
	t.Helper()
	got, err := mem.Add(item)
	if err != nil {
		t.Fatalf("Add(%+v) 应成功: %v", item, err)
	}
	return got
}

func memoryJSON(t *testing.T, mem *Memory) map[string]any {
	t.Helper()
	return mustJSONMap(t, mem)
}

func memoryGroupsOf(t *testing.T, mem *Memory) map[string]any {
	t.Helper()
	raw := memoryJSON(t, mem)
	groups, ok := raw["groups"].(map[string]any)
	if !ok {
		t.Fatalf("$.data.memory.groups 期望对象，得到 %#v", raw["groups"])
	}
	return groups
}

func memoryItemsOf(t *testing.T, mem *Memory) []any {
	t.Helper()
	raw := memoryJSON(t, mem)
	items, ok := raw["items"].([]any)
	if !ok {
		t.Fatalf("$.data.memory.items 期望数组，得到 %#v", raw["items"])
	}
	return items
}

func hasConflictEntry(entries []ConflictEntry, id string) bool {
	for _, e := range entries {
		if e.ID == id {
			return true
		}
	}
	return false
}

// TestMemory_GroupsExactlyFourKeys 是  的硬口径：四类分组键恰为
// entity / conclusion / task / preference，且未知分组无法进入记忆。
func TestMemory_GroupsExactlyFourKeys(t *testing.T) {
	want := []GroupKey{"entity", "conclusion", "task", "preference"}
	if got := GroupKeys(); !reflect.DeepEqual(got, want) {
		t.Fatalf("四类分组键必须恰为 %v，得到 %v", want, got)
	}
	for _, k := range want {
		if !k.Valid() {
			t.Errorf("分组键 %q 应合法", k)
		}
	}
	for _, bad := range []GroupKey{"memory", "Entity", "", "items", "group"} {
		if bad.Valid() {
			t.Errorf("分组键 %q 不得合法（分组键是闭集）", bad)
		}
	}

	mem := NewMemory()
	addMemory(t, mem, MemoryItem{Group: GroupEntity, EntityType: "ip", Value: "1.2.3.4", Source: RoundSource(1)})
	addMemory(t, mem, MemoryItem{Group: GroupConclusion, Subject: "1.2.3.4", Value: "高危", Source: RoundSource(1)})
	addMemory(t, mem, MemoryItem{Group: GroupTask, Value: "出初步报告", Source: RoundSource(2)})
	addMemory(t, mem, MemoryItem{Group: GroupPreference, Value: "只看近 7 天", Source: ToolSource("statistics")})

	groups := memoryGroupsOf(t, mem)
	if len(groups) != 4 {
		t.Fatalf("$.data.memory.groups 必须恰为 4 个 key（契约点），得到 %d 个: %v", len(groups), jsonKeys(groups))
	}
	if got := jsonKeys(groups); !reflect.DeepEqual(got, []string{"conclusion", "entity", "preference", "task"}) {
		t.Fatalf("分组 key 期望 entity/conclusion/task/preference，得到 %v", got)
	}
	for _, name := range []string{"entity", "conclusion", "task", "preference"} {
		arr, ok := groups[name].([]any)
		if !ok || len(arr) != 1 {
			t.Errorf("分组 %q 期望恰好 1 条，得到 %#v", name, groups[name])
		}
	}

	// 未知分组无法进入记忆（Add 拒绝）
	if _, err := mem.Add(MemoryItem{Group: GroupKey("memory"), Value: "x", Source: RoundSource(1)}); !errors.Is(err, ErrUnknownGroup) {
		t.Fatalf("未知分组应被拒绝（ErrUnknownGroup），得到 %v", err)
	}
	// 分组键是闭集：畸形载荷不得静默接受
	var decoded Memory
	bad := []byte(`{"items":[{"id":"entity-1","group":"memory","value":"x","source":"round:1"}]}`)
	if err := json.Unmarshal(bad, &decoded); !errors.Is(err, ErrUnknownGroup) {
		t.Fatalf("畸形记忆载荷的未知分组应被拒绝（ErrUnknownGroup），得到 %v", err)
	}
}

// TestMemory_ItemHasSource 覆盖 ：来源枚举恰为 round:<n> / tool:<name>，且缺来源的记忆被拒。
func TestMemory_ItemHasSource(t *testing.T) {
	if got := RoundSource(3); got != "round:3" {
		t.Fatalf("RoundSource(3) 期望 round:3，得到 %q", got)
	}
	if got := ToolSource("web_search"); got != "tool:web_search" {
		t.Fatalf("ToolSource 期望 tool:web_search，得到 %q", got)
	}
	for _, ok := range []string{"round:1", "round:12", "tool:web_search", "tool:query_log"} {
		if !ValidSource(ok) {
			t.Errorf("来源 %q 应合法（契约点）", ok)
		}
	}
	for _, bad := range []string{"", "round:", "round:0", "round:-1", "round:abc", "tool:", "tool:   ", "round", "tool", "user", "manual"} {
		if ValidSource(bad) {
			t.Errorf("来源 %q 不得合法（枚举是闭集：round:<n> / tool:<name>）", bad)
		}
	}

	mem := NewMemory()
	addMemory(t, mem, MemoryItem{Group: GroupEntity, EntityType: "ip", Value: "1.2.3.4", Source: RoundSource(7)})
	addMemory(t, mem, MemoryItem{Group: GroupConclusion, Subject: "1.2.3.4", Value: "高危", Source: ToolSource("ip_detail")})

	for _, raw := range memoryItemsOf(t, mem) {
		item, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("items[] 期望对象，得到 %#v", raw)
		}
		source, _ := item["source"].(string)
		if !ValidSource(source) {
			t.Fatalf("items[].source 必须 ∈ {round:<n>, tool:<name>}（契约点），得到 %q", source)
		}
		if kind := strings.SplitN(source, ":", 2)[0]; kind != "round" && kind != "tool" {
			t.Fatalf("来源前缀只允许 round / tool，得到 %q", kind)
		}
	}

	if _, err := mem.Add(MemoryItem{Group: GroupEntity, Value: "5.6.7.8"}); !errors.Is(err, ErrInvalidSource) {
		t.Fatalf("缺来源的记忆必须被拒（来源不明不得静默，ADR-002 决策 3），得到 %v", err)
	}
	if _, err := mem.Add(MemoryItem{Group: GroupEntity, Value: "   ", Source: RoundSource(1)}); !errors.Is(err, ErrEmptyValue) {
		t.Fatalf("空内容必须被拒（ErrEmptyValue），得到 %v", err)
	}
}

// TestMemory_EditTakesEffectNextTurn 覆盖 ：就地编辑/删除立即生效，下一轮按新值解析。
func TestMemory_EditTakesEffectNextTurn(t *testing.T) {
	mem := NewMemory()
	item := addMemory(t, mem, MemoryItem{Group: GroupEntity, EntityType: "ip", Value: "1.2.3.4", Source: RoundSource(1)})

	first, err := mem.Handle()
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}

	edited, err := mem.Edit(item.ID, "5.6.7.8")
	if err != nil {
		t.Fatalf("Edit 应立即生效: %v", err)
	}
	if edited.Value != "5.6.7.8" {
		t.Fatalf("编辑后条目值期望 5.6.7.8，得到 %q", edited.Value)
	}
	if edited.ID != item.ID {
		t.Fatalf("编辑不得改变条目 ID：%q → %q", item.ID, edited.ID)
	}

	// 返回体立即回显新值
	groups := memoryGroupsOf(t, mem)
	arr, _ := groups["entity"].([]any)
	if len(arr) != 1 {
		t.Fatalf("编辑后 entity 分组期望 1 条，得到 %#v", groups["entity"])
	}
	if got := arr[0].(map[string]any)["value"]; got != "5.6.7.8" {
		t.Fatalf("返回体必须回显新值（契约点），得到 %#v", got)
	}

	// 下一轮：调用方携带新句柄 ⇒ 按新记忆解析（无需重开会话）
	next, err := mem.Handle()
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if next.Revision <= first.Revision {
		t.Fatalf("修改后 revision 应递增：%d → %d", first.Revision, next.Revision)
	}
	round2, err := MemoryFromHandle(next)
	if err != nil {
		t.Fatalf("MemoryFromHandle: %v", err)
	}
	if got := round2.Groups().Entity; len(got) != 1 || got[0].Value != "5.6.7.8" {
		t.Fatalf("下一轮必须按新值解析，得到 %#v", got)
	}

	// 删除立即生效：条目从 items 与 groups 同时消失
	if _, err := mem.Delete(item.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := mem.Delete(item.ID); !errors.Is(err, ErrItemNotFound) {
		t.Fatalf("重复删除应报 ErrItemNotFound，得到 %v", err)
	}
	if len(mem.Groups().Entity) != 0 || len(memoryItemsOf(t, mem)) != 0 {
		t.Fatalf("删除后条目必须从 items 与 groups 同时消失，得到 %#v / %#v", mem.Items, mem.Groups().Entity)
	}
}

// TestMemory_AutoAccumulate 覆盖 ：新一轮提到的新实体自动入记忆。
func TestMemory_AutoAccumulate(t *testing.T) {
	mem := NewMemory()
	before := len(memoryItemsOf(t, mem))
	addMemory(t, mem, MemoryItem{Group: GroupEntity, EntityType: "domain", Value: "evil.example.com", Source: RoundSource(2)})
	addMemory(t, mem, MemoryItem{Group: GroupEntity, EntityType: "ip", Value: "9.9.9.9", Source: ToolSource("domain_detail")})

	items := memoryItemsOf(t, mem)
	if len(items)-before != 2 {
		t.Fatalf("新一轮提到的新实体必须自动入记忆（契约点）：期望新增 2 条，得到 %d", len(items)-before)
	}
	got := mem.Groups().Entity
	if len(got) != 2 {
		t.Fatalf("两个新实体都应落在 entity 分组，得到 %#v", got)
	}
	values := map[string]bool{got[0].Value: true, got[1].Value: true}
	for _, want := range []string{"evil.example.com", "9.9.9.9"} {
		if !values[want] {
			t.Fatalf("实体 %q 未自动入记忆，得到 %#v", want, got)
		}
	}
	for _, it := range got {
		if !ValidSource(it.Source) {
			t.Fatalf("自动累积的条目同样必须带合法来源（契约点），得到 %q", it.Source)
		}
	}
}

// TestMemory_CompressionNotice 覆盖 ：超窗 ⇒ 压缩上下文继续 + compressed==true + 逐字告知。
func TestMemory_CompressionNotice(t *testing.T) {
	if CompressionNotice != compressionNoticeLiteral {
		t.Fatalf("压缩告知文案必须逐字为 %q，得到 %q", compressionNoticeLiteral, CompressionNotice)
	}

	mem := NewMemory()
	entity := addMemory(t, mem, MemoryItem{Group: GroupEntity, EntityType: "ip", Value: "1.2.3.4", Source: RoundSource(1)})
	conclusion := addMemory(t, mem, MemoryItem{Group: GroupConclusion, Subject: "1.2.3.4", Value: "高危", Source: RoundSource(1)})

	// 未超窗 ⇒ 不压缩、不给告知
	if notice := mem.Compress(1000, 8192); notice != "" {
		t.Fatalf("未超上下文窗口不得压缩，得到告知 %q", notice)
	}
	// 未设窗口上限 ⇒ 不压缩
	if notice := mem.Compress(9000, 0); notice != "" {
		t.Fatalf("未设窗口上限不得压缩，得到告知 %q", notice)
	}
	if raw := memoryJSON(t, mem); raw["compressed"] != false {
		t.Fatalf("未超窗 compressed 应为 false，得到 %#v", raw["compressed"])
	}

	// 超窗 ⇒ 以记忆作压缩上下文继续 + 显式告知（不静默丢弃）
	notice := mem.Compress(9000, 8192)
	if notice != compressionNoticeLiteral {
		t.Fatalf("超窗必须逐字告知 %q，得到 %q", compressionNoticeLiteral, notice)
	}
	if raw := memoryJSON(t, mem); raw["compressed"] != true {
		t.Fatalf("超窗后 compressed 必须为 true（契约点），得到 %#v", raw["compressed"])
	}

	// 关键实体与结论不丢
	g := mem.Groups()
	if len(g.Entity) != 1 || g.Entity[0].ID != entity.ID {
		t.Fatalf("压缩后关键实体不得丢失: %#v", g.Entity)
	}
	if len(g.Conclusion) != 1 || g.Conclusion[0].ID != conclusion.ID {
		t.Fatalf("压缩后结论不得丢失: %#v", g.Conclusion)
	}
	if len(mem.Items) != 2 {
		t.Fatalf("压缩不得删除任何条目，得到 %d 条", len(mem.Items))
	}

	// 压缩状态与条目随句柄往返
	h, err := mem.Handle()
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	next, err := MemoryFromHandle(h)
	if err != nil {
		t.Fatalf("MemoryFromHandle: %v", err)
	}
	if !next.Compressed || len(next.Items) != 2 {
		t.Fatalf("压缩状态与条目必须随句柄往返，得到 %+v", next)
	}
}

// TestMemory_ConflictFlagged 覆盖 ：同实体矛盾结论标出且不自动择一。
func TestMemory_ConflictFlagged(t *testing.T) {
	mem := NewMemory()
	first := addMemory(t, mem, MemoryItem{Group: GroupConclusion, Subject: "1.2.3.4", Value: "高危", Source: RoundSource(2)})
	second := addMemory(t, mem, MemoryItem{Group: GroupConclusion, Subject: "1.2.3.4", Value: "低危（历史情报）", Source: RoundSource(5)})

	conflicts := mem.Conflicts()
	if len(conflicts) != 1 {
		t.Fatalf("同实体矛盾结论必须标出（契约点），conflicts 期望 1 条，得到 %d: %+v", len(conflicts), conflicts)
	}
	c := conflicts[0]
	if c.Group != GroupConclusion || c.Subject != "1.2.3.4" {
		t.Fatalf("矛盾记录的分组/主体不符: %+v", c)
	}
	if len(c.Items) != 2 || !hasConflictEntry(c.Items, first.ID) || !hasConflictEntry(c.Items, second.ID) {
		t.Fatalf("矛盾各方都必须出现在 conflicts[].items（不自动择一）: %+v", c.Items)
	}
	// 冲突各方携带来源 ⇒ 面板可渲染「与第 N 轮结论矛盾，待你裁决」
	sources := []string{c.Items[0].Source, c.Items[1].Source}
	if !reflect.DeepEqual(sources, []string{"round:2", "round:5"}) {
		t.Fatalf("冲突各方必须带来源（供裁决提示定位轮次），得到 %v", sources)
	}

	// 返回体里 conflicts[] 非空，且两条结论都还在（不自动择一）
	raw := memoryJSON(t, mem)
	arr, ok := raw["conflicts"].([]any)
	if !ok || len(arr) != 1 {
		t.Fatalf("$.data.memory.conflicts[] 必须非空（契约点），得到 %#v", raw["conflicts"])
	}
	if len(mem.Items) != 2 {
		t.Fatalf("不自动择一：两条结论都必须保留，得到 %d 条", len(mem.Items))
	}

	// 同主体同取值不是矛盾
	same := NewMemory()
	addMemory(t, same, MemoryItem{Group: GroupConclusion, Subject: "8.8.8.8", Value: "高危", Source: RoundSource(1)})
	addMemory(t, same, MemoryItem{Group: GroupConclusion, Subject: "8.8.8.8", Value: "高危", Source: RoundSource(3)})
	if got := same.Conflicts(); len(got) != 0 {
		t.Fatalf("同主体同取值不构成矛盾，得到 %+v", got)
	}

	// 不同主体不是矛盾
	other := NewMemory()
	addMemory(t, other, MemoryItem{Group: GroupConclusion, Subject: "1.1.1.1", Value: "高危", Source: RoundSource(1)})
	addMemory(t, other, MemoryItem{Group: GroupConclusion, Subject: "2.2.2.2", Value: "低危", Source: RoundSource(2)})
	if got := other.Conflicts(); len(got) != 0 {
		t.Fatalf("不同主体不构成矛盾，得到 %+v", got)
	}

	// 用户裁决（删除其一）后矛盾立即消失
	if _, err := mem.Delete(second.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if got := mem.Conflicts(); len(got) != 0 {
		t.Fatalf("用户裁决（删除其一）后矛盾必须消失（不自动择一），得到 %+v", got)
	}
}

// TestMemory_Clear 覆盖 ：清空 ⇒ items==[]，从空记忆继续。
func TestMemory_Clear(t *testing.T) {
	mem := NewMemory()
	addMemory(t, mem, MemoryItem{Group: GroupEntity, EntityType: "ip", Value: "1.2.3.4", Source: RoundSource(1)})
	addMemory(t, mem, MemoryItem{Group: GroupConclusion, Subject: "1.2.3.4", Value: "高危", Source: RoundSource(1)})
	if notice := mem.Compress(9000, 8192); notice == "" {
		t.Fatal("前置条件：此刻应已压缩")
	}

	mem.Clear()

	if len(mem.Items) != 0 {
		t.Fatalf("清空后 items 必须为空，得到 %#v", mem.Items)
	}
	raw := memoryJSON(t, mem)
	items, ok := raw["items"].([]any)
	if !ok {
		t.Fatalf("清空后 items 必须是数组（不得为 null），得到 %#v", raw["items"])
	}
	if len(items) != 0 {
		t.Fatalf("清空后 $.data.memory.items 必须为 []（契约点），得到 %v", items)
	}
	for name, v := range memoryGroupsOf(t, mem) {
		arr, ok := v.([]any)
		if !ok || len(arr) != 0 {
			t.Errorf("清空后分组 %q 必须为空数组，得到 %#v", name, v)
		}
	}
	if got := mem.Conflicts(); len(got) != 0 {
		t.Fatalf("清空后不得残留冲突，得到 %+v", got)
	}
	if raw["compressed"] != false {
		t.Fatalf("清空后从空记忆继续，compressed 应为 false，得到 %#v", raw["compressed"])
	}

	// 清空后仍可继续累积（对话从空记忆继续）
	addMemory(t, mem, MemoryItem{Group: GroupEntity, EntityType: "ip", Value: "9.9.9.9", Source: RoundSource(9)})
	if len(mem.Items) != 1 {
		t.Fatalf("清空后应能继续累积，得到 %#v", mem.Items)
	}
}

// TestMemory_NewSessionEmpty 覆盖 ：新会话从空记忆开始、v1 无跨会话长期记忆。
func TestMemory_NewSessionEmpty(t *testing.T) {
	fresh, err := MemoryFromHandle(nil)
	if err != nil {
		t.Fatalf("MemoryFromHandle(nil): %v", err)
	}
	if len(fresh.Items) != 0 || len(fresh.Groups().Entity) != 0 {
		t.Fatalf("新会话必须从空记忆开始（契约点），得到 %#v", fresh.Items)
	}

	// 旧会话的记忆不得泄漏到新会话
	old := NewMemory()
	addMemory(t, old, MemoryItem{Group: GroupEntity, EntityType: "ip", Value: "1.2.3.4", Source: RoundSource(1)})
	another, err := MemoryFromHandle(nil)
	if err != nil {
		t.Fatalf("MemoryFromHandle(nil): %v", err)
	}
	if len(another.Items) != 0 {
		t.Fatalf("v1 无跨会话长期记忆：新会话不得继承旧记忆，得到 %#v", another.Items)
	}

	// 引擎不持有会话状态：两个独立记忆并发累积互不影响（-race 下同样成立）
	const n = 8
	a, b := NewMemory(), NewMemory()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < n; i++ {
			if _, err := a.Add(MemoryItem{Group: GroupEntity, Value: fmt.Sprintf("a-%d", i), Source: RoundSource(i + 1)}); err != nil {
				t.Errorf("记忆 A Add: %v", err)
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < n; i++ {
			if _, err := b.Add(MemoryItem{Group: GroupEntity, Value: fmt.Sprintf("b-%d", i), Source: RoundSource(i + 1)}); err != nil {
				t.Errorf("记忆 B Add: %v", err)
			}
		}
	}()
	wg.Wait()

	if len(a.Items) != n || len(b.Items) != n {
		t.Fatalf("两个会话各自累积应互不影响，得到 %d / %d", len(a.Items), len(b.Items))
	}
	for _, it := range a.Items {
		if !strings.HasPrefix(it.Value, "a-") {
			t.Fatalf("记忆 A 混入了他会话的条目 %q（引擎不得持有跨会话存储）", it.Value)
		}
	}
	for _, it := range b.Items {
		if !strings.HasPrefix(it.Value, "b-") {
			t.Fatalf("记忆 B 混入了他会话的条目 %q（引擎不得持有跨会话存储）", it.Value)
		}
	}
}

// TestMemory_WireShape 固化  的 memory 外形与「groups 是 items 派生视图」的不变量。
func TestMemory_WireShape(t *testing.T) {
	want := []string{"compressed", "conflicts", "groups", "items"}
	if got := jsonKeys(memoryJSON(t, NewMemory())); !reflect.DeepEqual(got, want) {
		t.Fatalf("$.data.memory 必须恰为 节 的四字段 %v，得到 %v", want, got)
	}

	empty := memoryJSON(t, NewMemory())
	if items, ok := empty["items"].([]any); !ok || len(items) != 0 {
		t.Fatalf("空记忆 items 应为 []（不得为 null），得到 %#v", empty["items"])
	}
	if conflicts, ok := empty["conflicts"].([]any); !ok || len(conflicts) != 0 {
		t.Fatalf("空记忆 conflicts 应为 []（不得为 null），得到 %#v", empty["conflicts"])
	}
	if compressed, ok := empty["compressed"].(bool); !ok || compressed {
		t.Fatalf("空记忆 compressed 应为布尔 false，得到 %#v", empty["compressed"])
	}
	for name, v := range empty["groups"].(map[string]any) {
		if arr, ok := v.([]any); !ok || len(arr) != 0 {
			t.Fatalf("空记忆分组 %q 应为 []（不得为 null），得到 %#v", name, v)
		}
	}

	// groups 恰为 items 的分组视图：合计条数必须相等（不存在落在 4 组之外的条目）
	mem := NewMemory()
	addMemory(t, mem, MemoryItem{Group: GroupTask, Value: "出初步报告", Source: RoundSource(1)})
	addMemory(t, mem, MemoryItem{Group: GroupPreference, Value: "只看近 7 天", Source: ToolSource("statistics")})
	addMemory(t, mem, MemoryItem{Group: GroupEntity, EntityType: "ip", Value: "1.2.3.4", Source: RoundSource(3)})

	items := memoryItemsOf(t, mem)
	grouped := 0
	for _, v := range memoryGroupsOf(t, mem) {
		grouped += len(v.([]any))
	}
	if grouped != len(items) {
		t.Fatalf("groups 必须恰为 items 的分组视图：items=%d，分组合计=%d", len(items), grouped)
	}

	// 幂等：自产 JSON 可原样解析回来（groups 派生、items 为真相源）
	payload, err := json.Marshal(mem)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	var back Memory
	if err := json.Unmarshal(payload, &back); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if !reflect.DeepEqual(back.Items, mem.Items) {
		t.Fatalf("自产 JSON 往返丢条目：\n%#v\n%#v", back.Items, mem.Items)
	}

	// groups 非空而 items 缺失的畸形载荷必须报错（不静默丢条目）
	broken := []byte(`{"groups":{"entity":[{"id":"entity-1","group":"entity","value":"1.2.3.4","source":"round:1"}]},"items":[],"compressed":false}`)
	var decoded Memory
	if err := json.Unmarshal(broken, &decoded); !errors.Is(err, ErrMemoryPayload) {
		t.Fatalf("groups 非空但 items 缺失应报错（不得静默丢条目），得到 %v", err)
	}
}

// TestMemory_EchoRoundTrip 覆盖 AD-15 / ：记忆由调用方携带、随请求往返，引擎不落库。
func TestMemory_EchoRoundTrip(t *testing.T) {
	mem := NewMemory()
	addMemory(t, mem, MemoryItem{Group: GroupEntity, EntityType: "ip", Value: "1.2.3.4", Source: RoundSource(1)})
	addMemory(t, mem, MemoryItem{Group: GroupConclusion, Subject: "1.2.3.4", Value: "高危", Source: RoundSource(1)})
	if notice := mem.Compress(9000, 8192); notice == "" {
		t.Fatal("前置条件：此刻应已压缩")
	}

	h, err := mem.Handle()
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if h.Revision != mem.Revision || h.Revision == 0 {
		t.Fatalf("句柄必须携带修订号，得到 %+v（mem.Revision=%d）", h, mem.Revision)
	}
	if len(h.Payload) == 0 {
		t.Fatal("句柄必须携带记忆载荷")
	}

	back, err := MemoryFromHandle(h)
	if err != nil {
		t.Fatalf("MemoryFromHandle: %v", err)
	}
	if !reflect.DeepEqual(back.Items, mem.Items) {
		t.Fatalf("句柄往返丢条目：\n%#v\n%#v", back.Items, mem.Items)
	}
	if back.Compressed != mem.Compressed || back.Revision != mem.Revision {
		t.Fatalf("句柄往返丢状态：%+v vs compressed=%v revision=%d", back, mem.Compressed, mem.Revision)
	}

	// 同请求重复发送 ⇒ 同结果（确定性；引擎不持有会话状态）
	h1, err := mem.Handle()
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	h2, err := mem.Handle()
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !bytes.Equal(h1.Payload, h2.Payload) {
		t.Fatalf("同一记忆重复序列化必须逐字节一致（确定性）:\n%s\n%s", h1.Payload, h2.Payload)
	}

	// 空载荷视为空记忆，但保留调用方的修订号
	empty, err := MemoryFromHandle(&kitctx.MemoryHandle{Revision: 3})
	if err != nil {
		t.Fatalf("MemoryFromHandle(空载荷): %v", err)
	}
	if len(empty.Items) != 0 || empty.Revision != 3 {
		t.Fatalf("空载荷应得到空记忆且保留修订号，得到 %+v", empty)
	}
	var nullish Memory
	if err := json.Unmarshal([]byte("null"), &nullish); err != nil {
		t.Fatalf("null 载荷应解析为空记忆: %v", err)
	}
}
