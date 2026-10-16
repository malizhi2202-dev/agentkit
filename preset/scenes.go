package preset

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"agentkit/kit"
	"agentkit/kitctx"
	"agentkit/scene"
	"agentkit/tools"
)

// NameScenes 是场景预制插件的默认名。
const NameScenes = "preset.scenes"

var (
	// ErrScenesRequired 表示没交出场景表（零值构造或传了 nil）。
	ErrScenesRequired = errors.New("preset: 未提供场景表")
	// ErrScenesNotAssembled 表示 Build 之前使用场景解析（它需要 Kit 的那张唯一工具表）。
	ErrScenesNotAssembled = errors.New("preset: 场景解析未装配（需先 kit.Build）")
	// ErrScenesStopped 表示装配根已停止 ⇒ 不再按旧的工具表回答新问题。
	ErrScenesStopped = errors.New("preset: 装配根已停止，场景解析不再服务")
	// ErrScenesToolsMissing 表示场景引用了工具表里不存在的工具（配置漂移，Build 期即失败）。
	ErrScenesToolsMissing = errors.New("preset: 场景引用的工具在工具表里不存在")
)

// Scenes 把一张场景表接进装配根，并在 Build 期校验它引用的工具真的存在。
//
// 校验时机只有这一个落点，是刻意的：场景表自己看不见工具表（能看见就得让 `scene` 反向依赖装配根，
// 或者让场景里抄一份工具清单 —— 两条都会造出第二份事实）。而 `Apply` 跑在所有插件的 `Declare` 之后，
// 此时工具表已定形，"引用了不存在的工具"这种配置漂移就能在启动时炸，而不是运行时静默少一个工具。
type Scenes struct {
	name    string
	reg     *scene.Registry
	tools   *tools.Registry
	stopped bool
}

// NewScenes 构造场景插件；表为 nil 时在构造期就拒（不留到 Apply 才说）。
func NewScenes(reg *scene.Registry) (*Scenes, error) {
	if reg == nil {
		return nil, ErrScenesRequired
	}
	return &Scenes{name: NameScenes, reg: reg}, nil
}

// Name 返回插件名。
func (p *Scenes) Name() string {
	if p == nil || p.name == "" {
		return NameScenes
	}
	return p.name
}

// Declare 不做登记（场景不进任何扩展点，它只是另一张被查的表）。
func (p *Scenes) Declare(*kit.Declarer) error { return nil }

// Apply 接上唯一工具表并校验引用；停止后关掉解析能力。
func (p *Scenes) Apply(k *kit.Kit) error {
	if p == nil || p.reg == nil {
		return ErrScenesRequired
	}
	reg := k.Tools()
	if missing := p.reg.Missing(reg); len(missing) > 0 {
		return fmt.Errorf("%w：%v", ErrScenesToolsMissing, missing)
	}
	p.tools = reg
	return k.OnStop(func() { p.stopped = true; p.tools = nil })
}

// Describe 供管理面内省。
func (p *Scenes) Describe() kit.Info {
	n := 0
	ids := ""
	if p != nil && p.reg != nil {
		n = p.reg.Len()
		ids = strings.Join(p.reg.IDs(), ",")
	}
	return kit.Info{Name: p.Name(), Version: "preset", Notes: map[string]string{
		"scenes": fmt.Sprint(n), "ids": ids,
	}}
}

// Resolve 按 ctx 里的角色裁出该场景当前可用的工具集。
//
// 角色只从 `kitctx.RoleFrom` 读：读不到就交空串，由唯一谓词判成"角色不可用"（框架不猜默认角色）。
// 被裁掉工具的原因在 `Resolution.Dropped` 里 —— 那不是错误，静默少做才是问题。
func (p *Scenes) Resolve(ctx context.Context, id string) (scene.Resolution, error) {
	if p == nil || p.reg == nil {
		return scene.Resolution{}, ErrScenesRequired
	}
	if p.stopped {
		return scene.Resolution{}, ErrScenesStopped
	}
	if p.tools == nil {
		return scene.Resolution{}, ErrScenesNotAssembled
	}
	role, _ := kitctx.RoleFrom(ctx)
	return p.reg.Resolve(id, string(role), p.tools)
}

var _ kit.Plugin = (*Scenes)(nil)
