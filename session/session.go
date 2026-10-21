// Package session 是会话记忆的**唯一装配点**（目标清单里的"会话管理"层）。
//
// 它不是新造的抽象：语义来自 宿主侧 的 `既有记忆实现`（该包只 import `agent` 与
// `kitctx`、零宿主耦合，因此整块可平移），今天仍留在单仓里；宿主换用本包登记在 宿主任务。
//
// # 口径（ADR-002 /  系）
//
//   - **对象来自请求，引擎不持有会话状态**：记忆由调用方随请求显式携带、装配完再随响应回显。
//     无服务端会话、无进程内缓存、无表、不落盘。本包因此**没有任何包级可变状态**：
//     输入全在 [Request]，输出全在 [Assembly]，[Assemble] 是纯函数 ⇒ 同请求重发、换实例处理都得同结果。
//   - **不重复定义结构**：四类分组键、`MemoryItem`、`Conflict`、压缩文案唯一归 `agent`；
//     携带句柄唯一归 `kitctx.MemoryHandle`（L-015 契约 2）。本包一行结构都不复制 ——
//     这也是"为什么没有独立的 session 数据结构"的答案：再加一份定义只会漂移。
//   - **闭集校验靠继承，不自己重述**：分组与来源的取值域由 `agent.Memory.Add` 收口，
//     本层只保证"任一步失败就不产生半成品"（见测试 `Assemble` 的零值断言）。
//   - **不读配置**：上下文窗口 `Window` 由调用方按请求声明，≤0 = 未声明 ⇒ 不压缩、不虚报。
package session

import (
	"fmt"

	"github.com/malizhi2202-dev/agentkit/agent"
	"github.com/malizhi2202-dev/agentkit/kitctx"
)

// Edit 是一次「就地纠正」：把 ID 指向的条目主文本改为 Value。
//
// 只改主文本，不改分组与来源 —— 来源是"这条记忆从哪来"的唯一依据，
// 纠正内容不得顺带伪造来源（沿用 `Memory.Edit` 的语义）。
type Edit struct {
	// ID 是靶点条目标识（由 `agent.Memory` 分配，随上一次回显携带）。
	ID string
	// Value 是纠正后的主文本；空白串非法（`agent.ErrEmptyValue`）。
	Value string
}

// Request 是一次请求**显式携带**的记忆输入。零值即"新会话、无任何操作、未声明窗口"⇒ 回显空记忆。
//
// 应用顺序是契约的一部分（固定且可复现）：**Clear → Deletes → Edits → NewItems → Compress**。
// 其中 Clear 是"重置记忆"：重置之后按 ID 定位的 Deletes / Edits 必然找不到条目，
// 此时 [Assemble] 显式返回 `agent.ErrItemNotFound`，**不静默吞掉**互相矛盾的指令。
type Request struct {
	// Handle 是调用方携带的会话记忆句柄；nil（或空载荷）= 新会话 ⇒ 空记忆。
	Handle *kitctx.MemoryHandle
	// Clear 为真时先清空记忆；清空后本轮 NewItems 仍会入记忆（对话从空记忆继续）。
	Clear bool
	// Deletes 是要删除的条目 ID；重复删除 / 不存在 ⇒ `agent.ErrItemNotFound`。
	Deletes []string
	// Edits 是要就地纠正的条目；下一轮即按新值解析，无需重开会话。
	Edits []Edit
	// NewItems 是**本轮新增**的条目；分组与来源取值域由 `agent` 收口，非法则整体失败、不留半成品。
	NewItems []agent.MemoryItem
	// Used 是当前会话长度，与 Window 同一计量单位（本层不分词、不猜）。
	Used int
	// Window 是模型上下文窗口上限；≤0 = 调用方未声明 ⇒ 不压缩。
	Window int
}

// Assembly 是一次请求的装配结果。全部字段都是**本次请求的产物**，调用方用完即弃；本包不持有、不缓存任何一项。
type Assembly struct {
	// Memory 是装配后的记忆，直接序列化即为回显给前端的四字段结构（由 `agent.MarshalJSON` 固定）。
	Memory *agent.Memory
	// Handle 是随响应回显、供下一次请求原样携带的句柄（修订号 + 载荷）。
	Handle *kitctx.MemoryHandle
	// Conflicts 是矛盾派生视图；矛盾**不自动择一**，各方都留在条目里交用户裁决。
	Conflicts []agent.Conflict
	// Notice 是压缩告知文案：超窗时为逐字 `agent.CompressionNotice`，未压缩或未声明窗口时为空串。
	Notice string
}

// Assemble 把请求显式携带的记忆装配成可回显的记忆对象与句柄。
//
// 纯函数：不读全局状态、不读时钟、不落库、不做下游 IO。
// 任一步失败都返回错误且不产生"半装配"结果 —— 调用方拿到的不是记忆，而是一个明确失败。
func Assemble(req Request) (Assembly, error) {
	// ① 从请求携带的句柄恢复记忆；nil / 空载荷 = 新会话 ⇒ 空记忆。
	//    载荷不可用时**不静默降级为空记忆**（那会让用户以为记忆被清空，而其实是请求坏了）。
	memory, err := agent.MemoryFromHandle(req.Handle)
	if err != nil {
		return Assembly{}, fmt.Errorf("session: 恢复请求携带的记忆句柄: %w", err)
	}
	// ② 清空。
	if req.Clear {
		memory.Clear()
	}
	// ③ 就地删除与纠正（顺序在新增之前：先纠正旧条目，再让新条目入场）。
	for _, id := range req.Deletes {
		if _, err := memory.Delete(id); err != nil {
			return Assembly{}, fmt.Errorf("session: 删除记忆条目 %q: %w", id, err)
		}
	}
	for _, edit := range req.Edits {
		if _, err := memory.Edit(edit.ID, edit.Value); err != nil {
			return Assembly{}, fmt.Errorf("session: 纠正记忆条目 %q: %w", edit.ID, err)
		}
	}
	// ④ 累积本轮新条目；分组/来源闭集由 `agent` 收口。
	for _, item := range req.NewItems {
		if _, err := memory.Add(item); err != nil {
			return Assembly{}, fmt.Errorf("session: 累积记忆条目（分组 %q）: %w", item.Group, err)
		}
	}
	// ⑤ 压缩：超窗时以记忆作压缩上下文继续（不删任何条目）并显式告知；未声明窗口不压缩、不虚报。
	notice := memory.Compress(req.Used, req.Window)
	// ⑥ 回显：句柄供下一次请求携带，冲突视图供面板标注。
	handle, err := memory.Handle()
	if err != nil {
		return Assembly{}, fmt.Errorf("session: 回显记忆句柄: %w", err)
	}
	return Assembly{Memory: memory, Handle: handle, Conflicts: memory.Conflicts(), Notice: notice}, nil
}
