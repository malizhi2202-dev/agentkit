// Package modelcfg 是"用哪个模型配置"的**唯一**决策点：显式 > 项目 > 平台公共默认。
//
// 为什么单独一层：宿主里"选模型"通常散在三处（请求参数带 model、项目/租户配置、
// 代码里写死的默认值），三处各自读各自的就会互相矛盾 —— 换一次默认值要改三个地方。
// 本包把顺序固定下来，并且**只固定顺序**：它不发 HTTP 请求、不认识任何厂商协议
// （那是 `model` 适配层与宿主 transport 的事），探活通过注入的函数完成（框架裁决 可插拔）。
//
// 一条与安全相关的取舍：**显式指定错了就报错，不静默降级**。
// 调用方点名要 `gpt-x` 而表里没有，"贴心地"换成平台默认去跑，等于把一次本应失败的请求
// 变成一次"用了错模型、看起来成功"的请求 —— 在带权限语义的引擎里这比失败更糟。
// 兜底只发生在"调用方根本没指定"的时候。
package modelcfg

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// 构造与解析阶段的错误。
var (
	// ErrIDRequired 表示配置缺少 ID（兜底链与探活结果都要按它寻址）。
	ErrIDRequired = errors.New("modelcfg: 配置缺少 ID")
	// ErrBaseRequired 表示配置缺少服务地址。
	ErrBaseRequired = errors.New("modelcfg: 配置缺少 Base（服务地址）")
	// ErrModelRequired 表示配置缺少模型名。
	ErrModelRequired = errors.New("modelcfg: 配置缺少 Model")
	// ErrTimeoutRequired 表示超时未设置或不为正 —— 0 值被当成"无限等"是本包不接受的默认。
	ErrTimeoutRequired = errors.New("modelcfg: 配置缺少正的 Timeout（不设超时不是安全默认）")
	// ErrDuplicateID 表示配置 ID 重复。
	ErrDuplicateID = errors.New("modelcfg: 配置 ID 重复")
	// ErrUnknownConfig 表示显式/绑定指定的 ID 不在表里（**不降级**）。
	ErrUnknownConfig = errors.New("modelcfg: 指定的配置不在表里（显式指定不静默降级）")
	// ErrNoDefault 表示既没有可用兜底，调用方也没指定。
	ErrNoDefault = errors.New("modelcfg: 无平台公共默认配置，且调用方未指定")
	// ErrNoHealthyConfig 表示探活后一个可用配置都不剩（每个候选都带 Reason）。
	ErrNoHealthyConfig = errors.New("modelcfg: 没有任何健康的模型配置")
)

// Source 是"这次用的配置是从哪一级来的"闭集。
type Source string

const (
	// SourceExplicit 表示调用方点名指定。
	SourceExplicit Source = "explicit"
	// SourceProject 表示按项目绑定解析。
	SourceProject Source = "project"
	// SourcePlatform 表示落到平台公共默认。
	SourcePlatform Source = "platform"
)

var sourceKinds = map[Source]bool{SourceExplicit: true, SourceProject: true, SourcePlatform: true}

// Valid 报告是否为已知来源。
func (s Source) Valid() bool { return sourceKinds[s] }

// String 便于日志直接打印。
func (s Source) String() string { return string(s) }

// Sources 返回闭集（字典序，稳定输出）。
func Sources() []Source {
	out := make([]Source, 0, len(sourceKinds))
	for s := range sourceKinds {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Config 是一份模型接入配置。
type Config struct {
	// ID 是稳定标识（写在项目配置/请求参数里的就是它，不是 Base）。
	ID string
	// Base 是服务地址（scheme + host，不带路径拼接逻辑）。
	Base string
	// Model 是模型名。
	Model string
	// Timeout 是**单次请求**上限。本包用它给注入的探活函数套 ctx 截止时间，
	// 因此它不是"写了没人读"的字段；宿主的 transport 同样应照着它设。
	Timeout time.Duration
}

// Valid 报告配置形状是否完整（trim 后非空 + 正的 Timeout）。
func (c Config) Valid() error {
	if strings.TrimSpace(c.ID) == "" {
		return ErrIDRequired
	}
	if strings.TrimSpace(c.Base) == "" {
		return fmt.Errorf("%w：%s", ErrBaseRequired, c.ID)
	}
	if strings.TrimSpace(c.Model) == "" {
		return fmt.Errorf("%w：%s", ErrModelRequired, c.ID)
	}
	if c.Timeout <= 0 {
		return fmt.Errorf("%w：%s", ErrTimeoutRequired, c.ID)
	}
	return nil
}

// Resolver 按固定顺序解析配置。构造后表与绑定不再变化。
type Resolver struct {
	byID     map[string]Config
	ids      []string
	project  map[string]string
	platform string
}

// Option 是构造选项（唯一能改构造期行为的两处：平台默认与项目绑定）。
type Option func(*Resolver) error

// PlatformDefault 指定平台公共默认配置 ID（必须存在于本次传入的配置里）。
func PlatformDefault(id string) Option {
	return func(r *Resolver) error {
		r.platform = strings.TrimSpace(id)
		return nil
	}
}

// ProjectBinding 给某个项目绑定一份配置（同样要求存在）。
func ProjectBinding(projectID, configID string) Option {
	return func(r *Resolver) error {
		p, c := strings.TrimSpace(projectID), strings.TrimSpace(configID)
		if p == "" {
			return errors.New("modelcfg: 项目绑定缺少项目 ID")
		}
		if _, ok := r.byID[c]; !ok {
			return fmt.Errorf("%w：项目 %s 绑定的配置 %q", ErrUnknownConfig, p, c)
		}
		if old, dup := r.project[p]; dup && old != r.byID[c].ID {
			return fmt.Errorf("modelcfg: 项目 %s 重复绑定（%s 与 %s）", p, old, c)
		}
		r.project[p] = c
		return nil
	}
}

// New 建解析器：任一配置形状不合法、ID 重复、默认指向不存在的 ID ⇒ 整体不成立（不留半张表）。
func New(configs []Config, opts ...Option) (*Resolver, error) {
	r := &Resolver{byID: make(map[string]Config, len(configs)), project: map[string]string{}}
	for _, c := range configs {
		if err := c.Valid(); err != nil {
			return nil, err
		}
		id := strings.TrimSpace(c.ID)
		if _, dup := r.byID[id]; dup {
			return nil, fmt.Errorf("%w：%s", ErrDuplicateID, id)
		}
		c.ID = id
		c.Base = strings.TrimSpace(c.Base)
		c.Model = strings.TrimSpace(c.Model)
		r.byID[id] = c
		r.ids = append(r.ids, id)
	}
	for _, opt := range opts {
		if err := opt(r); err != nil {
			return nil, err
		}
	}
	if r.platform != "" {
		if _, ok := r.byID[r.platform]; !ok {
			return nil, fmt.Errorf("%w：平台默认 %q", ErrUnknownConfig, r.platform)
		}
	}
	return r, nil
}

// IDs 返回登记顺序的配置 ID 副本。
func (r *Resolver) IDs() []string {
	if r == nil {
		return nil
	}
	return append([]string(nil), r.ids...)
}

// Len 返回配置条数（nil 安全）。
func (r *Resolver) Len() int {
	if r == nil {
		return 0
	}
	return len(r.ids)
}

// Get 按 ID 取一份配置（副本）。
func (r *Resolver) Get(id string) (Config, bool) {
	if r == nil {
		return Config{}, false
	}
	c, ok := r.byID[strings.TrimSpace(id)]
	return c, ok
}

// Resolve 是**唯一**的顺序实现：显式 > 项目绑定 > 平台默认。
//
// 显式非空但查不到 ⇒ 直接报错（不降级）；项目绑定查不到同理。只有"调用方没指定"才往下走。
func (r *Resolver) Resolve(explicitID, projectID string) (Config, Source, error) {
	if r == nil || len(r.byID) == 0 {
		return Config{}, "", ErrNoDefault
	}
	if e := strings.TrimSpace(explicitID); e != "" {
		c, ok := r.byID[e]
		if !ok {
			return Config{}, "", fmt.Errorf("%w：explicit %q", ErrUnknownConfig, e)
		}
		return c, SourceExplicit, nil
	}
	if p := strings.TrimSpace(projectID); p != "" {
		if id, bound := r.project[p]; bound {
			c, ok := r.byID[id]
			if !ok {
				// 构造期已挡住"绑定到不存在的配置"，这里是运行期的双保险（表被外部改坏也要报错而不是降级）。
				return Config{}, "", fmt.Errorf("%w：project %s → %q", ErrUnknownConfig, p, id)
			}
			return c, SourceProject, nil
		}
		// 项目没绑定不是错误：继续往下落平台默认（顺序只在这里定义一次）。
	}
	if r.platform == "" {
		return Config{}, "", ErrNoDefault
	}
	c, ok := r.byID[r.platform]
	if !ok {
		return Config{}, "", fmt.Errorf("%w：platform default %q", ErrUnknownConfig, r.platform)
	}
	return c, SourcePlatform, nil
}

// Outcome 是一个候选的探活结论。
type Outcome struct {
	ID      string `json:"id"`
	Healthy bool   `json:"healthy"`
	Reason  string `json:"reason,omitempty"`
	Latency string `json:"latency,omitempty"`
}

// Prober 由宿主注入：拿一份配置去试一次真实调用（本包不碰网络，也就不知道任何厂商协议）。
type Prober func(ctx context.Context, cfg Config) error

// Probe 逐个探活，返回与登记顺序一致的结果；**全部失败**才判 ErrNoHealthyConfig，
// 并把每个候选各自的失败原因带回去（只说"都不健康"等于没做诊断）。
//
// Timeout 在这里被读取：每个候选用**自己的**超时套 ctx，所以一份慢配置不会拖垮整轮探活。
func (r *Resolver) Probe(ctx context.Context, probe Prober) ([]Outcome, error) {
	if r == nil || len(r.ids) == 0 {
		return nil, ErrNoDefault
	}
	if probe == nil {
		return nil, errors.New("modelcfg: 探活函数未注入（本包不自己发请求）")
	}
	out := make([]Outcome, 0, len(r.ids))
	healthy := 0
	for _, id := range r.ids {
		cfg := r.byID[id]
		single, cancel := context.WithTimeout(ctx, cfg.Timeout)
		started := time.Now()
		err := probe(single, cfg)
		latency := time.Since(started)
		cancel()
		o := Outcome{ID: id, Healthy: err == nil, Latency: latency.Round(time.Microsecond).String()}
		if err != nil {
			o.Reason = err.Error()
		} else {
			healthy++
		}
		out = append(out, o)
	}
	if healthy == 0 {
		return out, fmt.Errorf("%w：%d 个候选全部失败", ErrNoHealthyConfig, len(out))
	}
	return out, nil
}
