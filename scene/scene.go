// Package scene 是"场景"这张表：可复用的工作模板，内容就是**一组工具名**。
//
// 形状上刻意只有三个字段，两处削减都是被既有纪律逼出来的，不是省事：
//
//   - **不写角色/权限点**。工具能不能被某个角色用，事实随工具自己走（`tools` 的 `RequiredRoles`，
//     P5 之后还可以由权限点生成）。场景里再抄一份角色，就会出现"工具收紧了、场景没跟上"
//     这种两张表互相矛盾的答案（框架内授权谓词只有一处，ADR-011 规则 5）。
//   - **不预留模型配置字段**。`modelcfg`（P8）没落地之前放一个 `ModelConfigID` 就是无人使用的抽象
//     （ADR/`kit/doc.go` 规则 2）；等它真被读到时再加。
//
// 于是本包只剩三件实事：登记模板、按角色裁剪、把"引用了不存在的工具"这件事变得可报告。
package scene

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"agentkit/tools"
)

// 登记与解析阶段的错误。
var (
	// ErrNoScenes 表示构造了一张空表（多半是配置没读到，而不是"这个应用没有场景"）。
	ErrNoScenes = errors.New("scene: 场景表为空")
	// ErrIDRequired 表示场景缺少 ID（跨请求引用它的东西全靠这个键）。
	ErrIDRequired = errors.New("scene: 场景缺少 ID")
	// ErrLabelRequired 表示场景缺少展示名。
	ErrLabelRequired = errors.New("scene: 场景缺少展示名（Label）")
	// ErrNoTools 表示场景没有引用任何工具（空模板会让解析出一个"谁都能用"的假结论）。
	ErrNoTools = errors.New("scene: 场景没有引用任何工具")
	// ErrDuplicateID 表示场景 ID 重复。
	ErrDuplicateID = errors.New("scene: 场景 ID 重复")
	// ErrUnknownScene 表示按 ID 解析时表里没有这个场景。
	ErrUnknownScene = errors.New("scene: 未知场景")
	// ErrRegistryRequired 表示裁剪时没交出工具表。
	ErrRegistryRequired = errors.New("scene: 需要 tools.Registry（授权判定的唯一来源）")
)

// Scene 是一条可复用模板：名字 + 引用的工具名集合。
//
// Tools 的**顺序有含义**：解析结果按它排序输出（不是按字典序），因为下游把工具顺序
// 当作"建议的调用顺序"，打乱了就是一份新的编排。
type Scene struct {
	ID    string   `json:"id"`
	Label string   `json:"label"`
	Tools []string `json:"tools"`
}

// Valid 报告场景自身是否形状完整（不代表工具存在，那要问工具表）。
func (s Scene) Valid() bool {
	return strings.TrimSpace(s.ID) != "" && strings.TrimSpace(s.Label) != "" && len(s.declared()) > 0
}

// declared 返回去空白后的工具名（保留声明顺序）。
func (s Scene) declared() []string {
	out := make([]string, 0, len(s.Tools))
	seen := make(map[string]bool, len(s.Tools))
	for _, t := range s.Tools {
		name := strings.TrimSpace(t)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out
}

// DropReason 是"场景里的一个工具没能成为可用工具"的原因闭集。
type DropReason string

const (
	// DropUnknownTool 表示工具表里根本没有这个名字：配置漂移，必须响亮报告。
	DropUnknownTool DropReason = "unknown_tool"
	// DropNotAuthorized 表示工具存在但当前角色不被授权（含角色不可用）。
	DropNotAuthorized DropReason = "not_authorized"
)

var dropKinds = map[DropReason]bool{DropUnknownTool: true, DropNotAuthorized: true}

// Valid 报告是否为已知原因。
func (d DropReason) Valid() bool { return dropKinds[d] }

// DropKinds 返回原因闭集（字典序）。
func DropKinds() []DropReason {
	out := make([]DropReason, 0, len(dropKinds))
	for k := range dropKinds {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Drop 是一条"这个工具为什么没用上"的留痕。
type Drop struct {
	Tool   string     `json:"tool"`
	Reason DropReason `json:"reason"`
}

// Resolution 是一次解析的完整答案：可用工具 + 被裁掉的事实。
//
// 被裁掉的东西必须报出来，不能只给一个变短的列表：静默少做与直接报错是两回事
// 。
type Resolution struct {
	Scene   Scene    `json:"scene"`
	Tools   []string `json:"tools"`
	Dropped []Drop   `json:"dropped,omitempty"`
}

// Registry 是场景表。写入口只有 Register/NewRegistry 一处，读取面不暴露内部切片。
type Registry struct {
	mu    sync.RWMutex
	byID  map[string]Scene
	order []string
}

// NewRegistry 用一批场景构造表；任何一条畸形都让整次构造失败。
func NewRegistry(seeds ...Scene) (*Registry, error) {
	if len(seeds) == 0 {
		return nil, ErrNoScenes
	}
	r := &Registry{byID: map[string]Scene{}}
	for _, s := range seeds {
		if err := r.Register(s); err != nil {
			return nil, err
		}
	}
	return r, nil
}

// Register 登记一条场景（归一化 ID/Label/工具名，去重工具但保留顺序）。
func (r *Registry) Register(s Scene) error {
	if r == nil {
		return errors.New("scene: 注册表为 nil")
	}
	id := strings.TrimSpace(s.ID)
	label := strings.TrimSpace(s.Label)
	switch {
	case id == "":
		return ErrIDRequired
	case label == "":
		return fmt.Errorf("%w：%s", ErrLabelRequired, id)
	}
	tools := s.declared()
	if len(tools) == 0 {
		return fmt.Errorf("%w：%s", ErrNoTools, id)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.byID[id]; dup {
		return fmt.Errorf("%w：%s", ErrDuplicateID, id)
	}
	r.byID[id] = Scene{ID: id, Label: label, Tools: tools}
	r.order = append(r.order, id)
	return nil
}

// Get 按 ID 取场景（返回副本，改动结果不影响表内数据）。
func (r *Registry) Get(id string) (Scene, bool) {
	if r == nil {
		return Scene{}, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.byID[strings.TrimSpace(id)]
	if !ok {
		return Scene{}, false
	}
	return copyScene(s), true
}

// IDs 返回登记顺序的场景 ID（管理面展示要稳定，不按字典序）。
func (r *Registry) IDs() []string {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]string(nil), r.order...)
}

// Len 报告场景条数。
func (r *Registry) Len() int {
	if r == nil {
		return 0
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.byID)
}

// Missing 返回"被引用但工具表里不存在"的工具名（按场景与声明序）。
//
// 单独提供它是因为**校验时机**只有一个正确落点：装配根 Build 期（此时工具表已定形）。
// 表本身不知道工具表，硬要它在校验期报错就等于假装能看见（然后长出第二份工具清单）。
func (r *Registry) Missing(reg *tools.Registry) []string {
	if r == nil || reg == nil {
		return nil
	}
	r.mu.RLock()
	order := append([]string(nil), r.order...)
	byID := make(map[string]Scene, len(r.byID))
	for id, s := range r.byID {
		byID[id] = s
	}
	r.mu.RUnlock()
	var out []string
	seen := map[string]bool{}
	for _, id := range order {
		for _, name := range byID[id].declared() {
			if seen[name] {
				continue
			}
			if _, ok := reg.Lookup(name); !ok {
				out = append(out, name)
				seen[name] = true
			}
		}
	}
	return out
}

// Resolve 按角色裁出该场景下真正可用的工具集。
//
// 授权判定完全交给 `tools.Registry`（唯一谓词）：本包里没有任何一处比较角色字符串。
// role 为空不特殊处理 —— 空串不是"已判定角色"，工具会整批被裁掉，这是正确结论而不是故障。
func (r *Registry) Resolve(id, role string, reg *tools.Registry) (Resolution, error) {
	if r == nil {
		return Resolution{}, ErrUnknownScene
	}
	if reg == nil {
		return Resolution{}, ErrRegistryRequired
	}
	s, ok := r.Get(id)
	if !ok {
		return Resolution{}, fmt.Errorf("%w：%s", ErrUnknownScene, strings.TrimSpace(id))
	}
	res := Resolution{Scene: s}
	for _, name := range s.Tools {
		_, registered, allowed := reg.LookupAuthorized(name, role)
		switch {
		case !registered:
			res.Dropped = append(res.Dropped, Drop{Tool: name, Reason: DropUnknownTool})
		case !allowed:
			res.Dropped = append(res.Dropped, Drop{Tool: name, Reason: DropNotAuthorized})
		default:
			res.Tools = append(res.Tools, name)
		}
	}
	return res, nil
}

func copyScene(s Scene) Scene {
	out := Scene{ID: s.ID, Label: s.Label}
	if len(s.Tools) > 0 {
		out.Tools = append([]string(nil), s.Tools...)
	}
	return out
}
