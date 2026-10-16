package modelcfg

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func cfgs() []Config {
	return []Config{
		{ID: " platform-fast ", Base: " https://a.example ", Model: " m-a ", Timeout: time.Second},
		{ID: "proj-own", Base: "https://b.example", Model: "m-b", Timeout: 2 * time.Second},
		{ID: "slow", Base: "https://c.example", Model: "m-c", Timeout: 30 * time.Millisecond},
	}
}

func resolver(t *testing.T, opts ...Option) *Resolver {
	t.Helper()
	r, err := New(cfgs(), opts...)
	if err != nil {
		t.Fatalf("New：%v", err)
	}
	return r
}

func TestModelCfg_ShapeAndDupRejected(t *testing.T) {
	base := Config{ID: "x", Base: "https://a", Model: "m", Timeout: time.Second}
	for _, c := range []struct {
		name string
		mut  func(*Config)
		want error
	}{
		{"缺 ID", func(c *Config) { c.ID = "  " }, ErrIDRequired},
		{"缺 Base", func(c *Config) { c.Base = "" }, ErrBaseRequired},
		{"缺 Model", func(c *Config) { c.Model = "" }, ErrModelRequired},
		{"超时为 0", func(c *Config) { c.Timeout = 0 }, ErrTimeoutRequired},
		{"超时为负", func(c *Config) { c.Timeout = -time.Second }, ErrTimeoutRequired},
	} {
		bad := base
		c.mut(&bad)
		if _, err := New([]Config{bad}); !errors.Is(err, c.want) {
			t.Errorf("%s：应判 %v，实得 %v", c.name, c.want, err)
		}
	}
	if _, err := New([]Config{base, {ID: " x ", Base: "https://b", Model: "m", Timeout: time.Second}}); !errors.Is(err, ErrDuplicateID) {
		t.Fatalf("重复 ID（含空格差异）应被拒，实得 %v", err)
	}
	// 默认/绑定指向不存在的 ID ⇒ 整体不成立（不是"运行时才发现没配"）
	if _, err := New(cfgs(), PlatformDefault("nope")); !errors.Is(err, ErrUnknownConfig) {
		t.Fatalf("平台默认指向不存在的配置应整体失败，实得 %v", err)
	}
	if _, err := New(cfgs(), ProjectBinding("p1", "nope")); !errors.Is(err, ErrUnknownConfig) {
		t.Fatalf("项目绑定指向不存在的配置应整体失败，实得 %v", err)
	}
	if _, err := New(cfgs(), ProjectBinding("  ", "slow")); err == nil {
		t.Fatal("空项目 ID 的绑定应被拒")
	}
	// 同项目重复绑到同一份配置允许，绑到不同配置要拒
	r, err := New(cfgs(), ProjectBinding("p1", "slow"), ProjectBinding("p1", "slow"))
	if err != nil {
		t.Fatalf("同目标重复绑定应允许：%v", err)
	}
	if _, err := New(cfgs(), ProjectBinding("p1", "slow"), ProjectBinding("p1", "proj-own")); err == nil {
		t.Fatal("同项目绑两份不同配置必须报错（顺序之外的第二个事实源）")
	}
	if got := r.IDs(); !reflect.DeepEqual(got, []string{"platform-fast", "proj-own", "slow"}) {
		t.Fatalf("IDs 应为登记序且已归一化：%v", got)
	}
	if got := r.IDs(); len(got) > 0 {
		got[0] = "hijacked"
		if r.IDs()[0] == "hijacked" {
			t.Fatal("IDs 交出了内部切片而非副本")
		}
	}
	if c, ok := r.Get(" platform-fast "); !ok || c.Base != "https://a.example" || c.Model != "m-a" {
		t.Fatalf("Get 未归一化/未清理：%+v ok=%v", c, ok)
	}
	var nilR *Resolver
	if _, ok := nilR.Get("x"); ok || nilR.Len() != 0 || nilR.IDs() != nil {
		t.Fatal("nil 解析器读取面不该 panic")
	}
}

func TestModelCfg_PrecedenceIsExplicitThenProjectThenPlatform(t *testing.T) {
	r := resolver(t, PlatformDefault("platform-fast"), ProjectBinding("p1", "proj-own"))
	// ① 显式最优先（即使项目和默认都有值）
	c, src, err := r.Resolve("slow", "p1")
	if err != nil || src != SourceExplicit || c.ID != "slow" {
		t.Fatalf("显式优先破了：%+v %s %v", c, src, err)
	}
	// ② 没指定时走项目绑定
	c, src, err = r.Resolve("", "p1")
	if err != nil || src != SourceProject || c.ID != "proj-own" {
		t.Fatalf("项目绑定优先破了：%+v %s %v", c, src, err)
	}
	// ③ 项目没绑定才落平台默认
	c, src, err = r.Resolve("", "p-none")
	if err != nil || src != SourcePlatform || c.ID != "platform-fast" {
		t.Fatalf("平台默认兜底破了：%+v %s %v", c, src, err)
	}
	// ④ 显式指定了不存在的配置 ⇒ **报错，不降级**（静默换模型等于用错模型还看起来成功）
	if _, _, err := r.Resolve("typo-model", "p1"); !errors.Is(err, ErrUnknownConfig) {
		t.Fatalf("显式指定错名字必须报错，实得 %v", err)
	}
	// ⑤ 没有任何默认且调用方没指定 ⇒ 也报错（不能返回零值配置去发请求）
	noDefault := resolver(t, ProjectBinding("p1", "proj-own"))
	if _, _, err := noDefault.Resolve("", "p-none"); !errors.Is(err, ErrNoDefault) {
		t.Fatalf("无默认且未指定应判 ErrNoDefault，实得 %v", err)
	}
	if _, _, err := noDefault.Resolve("", "p1"); err != nil {
		t.Fatalf("有项目绑定时应可解析：%v", err)
	}
	// 空表 / nil 解析器：一律拒绝，不交零值
	empty, err := New(nil)
	if err != nil {
		t.Fatalf("New(nil)：%v", err)
	}
	if _, _, err := empty.Resolve("x", "p"); !errors.Is(err, ErrNoDefault) {
		t.Fatalf("空表应判 ErrNoDefault，实得 %v", err)
	}
	var nilR *Resolver
	if _, _, err := nilR.Resolve("", ""); !errors.Is(err, ErrNoDefault) {
		t.Fatalf("nil 解析器应判 ErrNoDefault，实得 %v", err)
	}
}

func TestModelCfg_ProbeReadsTimeoutAndReportsEveryReason(t *testing.T) {
	// 全部失败 ⇒ ErrNoHealthyConfig，且每个候选都带自己的原因
	r := resolver(t, PlatformDefault("platform-fast"))
	seenTimeout := map[string]time.Duration{}
	out, err := r.Probe(context.Background(), func(ctx context.Context, c Config) error {
		d, _ := ctx.Deadline()
		seenTimeout[c.ID] = time.Until(d)
		return errors.New(c.ID + " 拒了")
	})
	if !errors.Is(err, ErrNoHealthyConfig) {
		t.Fatalf("全失败应判 ErrNoHealthyConfig，实得 %v", err)
	}
	if len(out) != 3 {
		t.Fatalf("结果数应与候选数一致：%d", len(out))
	}
	for i, o := range range3(out) {
		if o.Healthy || !strings.Contains(o.Reason, o.ID+" 拒了") {
			t.Fatalf("候选 %d 的失败原因不完整：%+v", i, o)
		}
	}
	// 每份配置用自己的超时（一份慢配置不该拖住整轮）
	if seenTimeout["slow"] > 200*time.Millisecond {
		t.Fatalf("slow 的超时没生效：%v", seenTimeout["slow"])
	}
	if seenTimeout["platform-fast"] < 500*time.Millisecond {
		t.Fatalf("platform-fast 的超时被套成了别人的：%v", seenTimeout["platform-fast"])
	}
	// 部分失败 ⇒ 可用（不报错），失败原因仍在结果里
	partial, err := r.Probe(context.Background(), func(_ context.Context, c Config) error {
		if c.ID == "slow" {
			return errors.New("超时")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("部分失败不该整体报错：%v", err)
	}
	if n := healthyCount(partial); n != 2 {
		t.Fatalf("健康数应为 2：%+v", partial)
	}
	if partial[2].Healthy || !strings.Contains(partial[2].Reason, "超时") {
		t.Fatalf("失败候选被抹平了：%+v", partial[2])
	}
	// 超时要真的生效：探活函数不看 ctx 也会被自己的 Timeout 掐断
	_, err = r.Probe(context.Background(), func(ctx context.Context, _ Config) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		<-ctx.Done()
		return ctx.Err()
	})
	if !errors.Is(err, ErrNoHealthyConfig) {
		t.Fatalf("每份配置都被自己超时掐断时应全失败，实得 %v", err)
	}
	if _, err := r.Probe(context.Background(), nil); err == nil {
		t.Fatal("没注入探活函数必须报错（本包不自己发请求）")
	}
	var nilR *Resolver
	if _, err := nilR.Probe(context.Background(), func(context.Context, Config) error { return nil }); !errors.Is(err, ErrNoDefault) {
		t.Fatalf("nil 解析器探活应判 ErrNoDefault，实得 %v", err)
	}
}

func range3(in []Outcome) []Outcome { return in[:3] }

func healthyCount(in []Outcome) int {
	n := 0
	for _, o := range in {
		if o.Healthy {
			n++
		}
	}
	return n
}

func TestModelCfg_SourceClosedSet(t *testing.T) {
	// 闭集按字典序输出（稳定展示用），**它不是优先级顺序** —— 优先级只写在 Resolve 一处，
	// 若这里按优先级排，日后有人照 Sources 的顺序做兜底就有了第二个事实源。
	if got := Sources(); !reflect.DeepEqual(got, []Source{SourceExplicit, SourcePlatform, SourceProject}) {
		t.Fatalf("闭集应为字典序三项：%v", got)
	}
	for _, s := range Sources() {
		if !s.Valid() || s.String() == "" {
			t.Errorf("%s 读取面不对", s)
		}
	}
	for _, s := range []Source{"", "default", "EXPLICIT", "user"} {
		if s.Valid() {
			t.Errorf("未知来源 %q 被当作合法", s)
		}
	}
}
