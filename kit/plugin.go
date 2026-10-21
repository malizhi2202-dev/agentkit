package kit

import (
	"errors"
	"fmt"

	"github.com/malizhi2202-dev/agentkit/tools"
	"github.com/malizhi2202-dev/agentkit/web"
)

// Plugin 是能力的最小可插拔单元（ADR-011 决策 1）。
//
// 两阶段是刻意的：
//   - `Declare` **只登记声明**（工具/权限点/路由/场景/…），做的是闭集校验、查重、引用可解析性；
//   - `Apply` **只接线**：此时全部声明已就绪且互相可见，才允许取依赖、构造服务、注册撤销钩子。
//
// 把接线写进 Declare 会同时废掉两阶段的意义（依赖可能尚未声明完 ⇒ 顺序耦合悄悄回到运行期），
// 因此 Declare 相**拿不到 `*Kit`**：`Declarer` 不持有 Kit 指针，类型层面就堵死这条捷径
// （常驻断言见 TestKit_DeclarerCannotReachKit）。
//
// 副作用（goroutine、ticker、外部连接、注册进宿主的东西）必须在 Apply 相用 `(*Kit).OnStop` 登记，
// 否则 `Stop` 撤不干净 —— 而"撤不干净"就等于"不可插拔"。
type Plugin interface {
	// Name 是插件唯一标识：重名直接判失败（两份同名声明会让依赖与内省都不可预期）。
	Name() string
	// Declare 登记本插件贡献的声明。可以返回错误，也可以用 d.Errorf 累积多条 —— 后者
	// 不会中断循环：所有声明期问题会被一次性收集后统一判失败（L-096：一处红不得掩盖其余红）。
	Declare(d *Declarer) error
	// Apply 接线：从 Kit 取服务、构造运行期对象、登记撤销钩子。
	Apply(k *Kit) error
}

// Enableable 是可选的插拔开关（返回 false ⇒ 该插件整体不进入装配，包括它贡献的声明）。
//
// 注意"拔除"的后果：依赖它的插件会在 Build 期以 `ErrMissingDependency` **失败**，
// 而不是静默降级 —— 少一个能力却继续跑，是比启动失败严重得多的事故。
type Enableable interface {
	Enabled() bool
}

// Dependent 是可选的依赖声明：本插件 Apply 之前必须先完成 Apply 的插件名字集合。
//
// 有了它，"谁必须先接线"从**宿主代码的书写顺序**变成**可机械校验的事实**（拓扑排序 + 成环判失败）。
type Dependent interface {
	Deps() []string
}

// Introspectable 是可选的自述接口，供管理面渲染（沿用既有可见性口径：只给 admin 看）。
type Introspectable interface {
	Describe() Info
}

// Info 是插件自述内容。刻意不塞任何活对象：内省结果要能安全序列化给前端。
type Info struct {
	Name    string            `json:"name"`
	Version string            `json:"version,omitempty"`
	Notes   map[string]string `json:"notes,omitempty"`
}

// 注册与装配失败的可识别错误（均以 %w 包装，调用方可 errors.Is 判定）。
var (
	// ErrPluginNameRequired 表示插件没有名字：无法引用、无法被依赖、无法内省定位。
	ErrPluginNameRequired = errors.New("kit: 插件 Name() 不能为空")
	// ErrPluginNil 表示 Use 传进了 nil：多半是装配表拼错，不拒绝会把 nil 解引用推到运行期。
	ErrPluginNil = errors.New("kit: 插件不能为 nil")
	// ErrPluginDuplicate 表示插件名重复。
	ErrPluginDuplicate = errors.New("kit: 插件名重复")
	// ErrMissingDependency 表示 Deps 指向一个未参与装配的插件（不存在，或被拔除）。
	ErrMissingDependency = errors.New("kit: 缺少依赖插件")
	// ErrDependencyCycle 表示依赖成环：不存在确定的 Apply 顺序。
	ErrDependencyCycle = errors.New("kit: 插件依赖成环")
	// ErrNotBuilt 表示在 Build 之前使用运行期能力。
	ErrNotBuilt = errors.New("kit: 尚未 Build（装配期能力不可用）")
	// ErrKitStopped 表示 Kit 已停止：不拒绝会在死掉的对象上继续接线。
	ErrKitStopped = errors.New("kit: 已停止，不可再用")
	// ErrLateDeclare 表示 Declare 相之后仍有插件往扩展点塞东西（破坏"闭集在装配期定死"）。
	ErrLateDeclare = errors.New("kit: Declare 相已结束，不得再登记声明")
)

// Declarer 是 Declare 相唯一可拿到的手柄。
//
// 它**只**提供三件事：我是谁（Owner）、往既有扩展点登记（Tools）、累积声明期问题（Errorf）。
// 没有 `Kit`／`Get`／返回 `*Kit` 的方法 —— 这是 ADR-011 两阶段边界的类型层保证。
type Declarer struct {
	owner    string
	tools    *tools.Registry
	web      web.Registrar
	problems []string
}

// Owner 返回当前正在登记的插件名（登记进扩展点时用于回溯"这条是谁贡献的"）。
func (d *Declarer) Owner() string {
	if d == nil {
		return ""
	}
	return d.owner
}

// Tools 返回框架内**唯一**的工具注册表（既有 `tools.Registry`，非副本、非二次封装）。
//
// 为什么不另造一层：`tools.Registry` 已经实现了空名/重名/`Run==nil` 拒绝、注册顺序即输出顺序、
// 以及"裁剪与执行复用同一谓词"（`allows`/`LookupAuthorized`）。再造一层就是把同一不变量写两遍（L-014）。
func (d *Declarer) Tools() *tools.Registry {
	if d == nil {
		return nil
	}
	return d.tools
}

// Errorf 累积一条声明期问题，不中断装配循环。
//
// 与直接 return error 的区别只有一个但很要紧：一个插件里可能有多条独立声明各自有问题，
// 中途返回会掩盖后面的问题（L-096 实测踩过）。全部收集后由 Build 用 errors.Join 一次性报出。
// Web 返回宿主注入的路由扩展点；未配置 ⇒ nil（调用方必须显式处理，不得"没路由表就只登记工具"）。
//
// 返回的是 `web.Registrar` 而非 `*web.Router`：声明期只该登记/枚举/直调，
// 不该有能力提前封存整张路由表。
func (d *Declarer) Web() web.Registrar {
	if d == nil {
		return nil
	}
	return d.web
}

func (d *Declarer) Errorf(format string, args ...any) {
	if d == nil {
		return
	}
	d.problems = append(d.problems, fmt.Sprintf("kit: 插件 %q 声明期：%s", d.owner, fmt.Sprintf(format, args...)))
}
