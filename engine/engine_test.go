package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"agentkit/audit"
	"agentkit/authz"
	"agentkit/kit"
	"agentkit/kitctx"
	"agentkit/model"
	"agentkit/preset"
	"agentkit/tools"
)

const endpoint = "http://192.168.16.16:8080/v1/chat/completions"

// ── 假模型网关（零真实出网）──

type scriptedDoer struct {
	steps []string
	err   error
	calls int
}

func (d *scriptedDoer) Do(req *http.Request) (*http.Response, error) {
	if d.err != nil {
		return nil, d.err
	}
	var wire struct {
		Messages []model.ChatMessage `json:"messages"`
	}
	_ = json.NewDecoder(req.Body).Decode(&wire)
	idx := d.calls
	d.calls++
	body := marshalBody(wireResponse{Choices: []wireChoice{{
		Index: 0, Message: wireMessage{Role: "assistant", Content: "没有更多脚本"}, FinishReason: "stop"}}})
	if idx < len(d.steps) {
		body = d.steps[idx]
	}
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)),
		Header: http.Header{"Content-Type": []string{"application/json"}}}, nil
}

type wireMessage struct {
	Role      string           `json:"role"`
	Content   string           `json:"content"`
	ToolCalls []model.ToolCall `json:"tool_calls,omitempty"`
}
type wireChoice struct {
	Index        int         `json:"index"`
	Message      wireMessage `json:"message"`
	FinishReason string      `json:"finish_reason"`
}
type wireResponse struct {
	Choices []wireChoice `json:"choices"`
}

func marshalBody(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func callReply(id, name, args string) string {
	return marshalBody(wireResponse{Choices: []wireChoice{{Index: 0, Message: wireMessage{
		Role: "assistant", ToolCalls: []model.ToolCall{{
			ID: id, Type: "function", Function: model.ToolFunction{Name: name, Arguments: args}}}},
		FinishReason: "tool_calls"}}})
}

func sayReply(text string) string {
	return marshalBody(wireResponse{Choices: []wireChoice{{Index: 0,
		Message: wireMessage{Role: "assistant", Content: text}, FinishReason: "stop"}}})
}

// ── 轨迹记录器 ──

type recLog struct {
	mu    sync.Mutex
	lines []string
}

func (l *recLog) record(level, msg string, fields map[string]any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, level+"|"+msg+"|"+fmt.Sprint(fields["step"], fields["tool"], fields["verdict"]))
}
func (l *recLog) Debug(msg string, f map[string]any) { l.record("debug", msg, f) }
func (l *recLog) Info(msg string, f map[string]any)  { l.record("info", msg, f) }
func (l *recLog) Warn(msg string, f map[string]any)  { l.record("warn", msg, f) }
func (l *recLog) Error(msg string, f map[string]any) { l.record("error", msg, f) }
func (l *recLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.lines...)
}

var _ kitctx.Logger = (*recLog)(nil)

// ── 装配 ──

type kit7 struct {
	deps  Deps
	calls int
	doer  *scriptedDoer
	log   *recLog
}

func build(t *testing.T, steps []string, transportErr error, also ...kit.Plugin) kit7 {
	t.Helper()
	doer := &scriptedDoer{steps: steps, err: transportErr}
	policy := model.NewEndpointPolicy(model.EndpointPolicyConfig{AllowedHosts: []string{"192.168.16.16"}})
	client := model.NewClient(model.ClientConfig{Doer: doer, Policy: policy,
		Timeout: 2 * time.Second, MaxAttempts: 1})
	roles, err := authz.NewRoleSet("support", "auditor", "admin")
	if err != nil {
		t.Fatalf("NewRoleSet：%v", err)
	}
	perms, err := preset.NewPerms(roles, authz.PermissionPoint{
		Key: "user.profile.read", Label: "按 IP 查领域对象", Scope: authz.ScopeProject,
		Enforced: authz.DirectEnforcement(), DefaultRoles: []kitctx.Role{"support", "admin"}})
	if err != nil {
		t.Fatalf("NewPerms：%v", err)
	}
	m, err := preset.NewModel(client, endpoint)
	if err != nil {
		t.Fatalf("NewModel：%v", err)
	}
	decl := preset.NewTools(perms, preset.Direct(tools.Tool{
		Name: "profile.query", Summary: "按 IP 查领域对象", RequiredRoles: []string{"support", "admin"},
		Run: func(context.Context, map[string]any) (any, error) { return "profile-07", nil }}))
	log := &recLog{}
	return kit7{doer: doer, log: log, deps: Deps{
		Model: m, Perms: perms, Tools: decl, Loop: preset.NewLoop(m),
		Config: kit.Config{Logger: log}, Also: also}}
}

func TestEngine_FourInjectionsAreRequired(t *testing.T) {
	k7 := build(t, []string{sayReply("好")}, nil)
	for _, c := range []struct {
		name string
		mut  func(*Deps)
		want error
	}{
		{"少模型", func(d *Deps) { d.Model = nil }, ErrModelRequired},
		{"少权限表", func(d *Deps) { d.Perms = nil }, ErrPermsRequired},
		{"少工具声明", func(d *Deps) { d.Tools = nil }, ErrToolsRequired},
		{"少循环", func(d *Deps) { d.Loop = nil }, ErrLoopRequired},
	} {
		d := k7.deps
		c.mut(&d)
		_, err := New(d)
		if !errors.Is(err, c.want) {
			t.Errorf("%s：应构造期判 %v，实得 %v", c.name, c.want, err)
		}
		if err != nil && !strings.Contains(err.Error(), c.want.Error()) {
			t.Errorf("%s：错误里没点名缺的是哪个注入点：%v", c.name, err)
		}
	}
	// 全缺 ⇒ 一次报全（不是试一个错一个）
	empty := Deps{}
	_, err := New(empty)
	if err == nil {
		t.Fatal("空 Deps 不该被接受")
	}
	for _, want := range []error{ErrModelRequired, ErrPermsRequired, ErrToolsRequired, ErrLoopRequired} {
		if !errors.Is(err, want) {
			t.Errorf("聚合错误缺 %v：%v", want, err)
		}
	}
	e, err := New(k7.deps)
	if err != nil {
		t.Fatalf("完整 Deps 应能装配：%v", err)
	}
	if got := e.Tools(); len(got) != 1 || got[0] != "profile.query" {
		t.Fatalf("工具表快照不对：%v", got)
	}
	if e.Kit() == nil {
		t.Fatal("应能交出装配根")
	}
	var nilE *Engine
	if _, err := nilE.Handle(context.Background(), "x"); !errors.Is(err, ErrKitRequired) {
		t.Fatalf("nil 引擎应判 ErrKitRequired，实得 %v", err)
	}
	if err := nilE.Stop(); !errors.Is(err, ErrKitRequired) {
		t.Fatalf("nil 引擎 Stop 应判 ErrKitRequired，实得 %v", err)
	}
	if nilE.Tools() != nil {
		t.Fatal("nil 引擎 Tools 应返回空")
	}
}

func TestEngine_EndToEndMultiTurnAndTrace(t *testing.T) {
	k7 := build(t, []string{
		callReply("c1", "profile.query", `{"ip":"198.51.100.23"}`),
		sayReply("198.51.100.23 属于 demo-profile-07"),
	}, nil)
	e, err := New(k7.deps)
	if err != nil {
		t.Fatalf("New：%v", err)
	}
	defer func() { _ = e.Stop() }()
	ctx := kitctx.WithRequest(context.Background(), kitctx.RequestContext{
		UserID: "u1", Role: "support", TraceID: "tr1", Limits: kitctx.Limits{MaxSteps: 5}})
	out, err := e.Handle(ctx, "198.51.100.23 是什么领域对象")
	if err != nil {
		t.Fatalf("Handle：%v", err)
	}
	if out.Err != nil || out.Degraded {
		t.Fatalf("正常路径不该带错误/降级：%+v", out)
	}
	if k7.doer.calls != 2 {
		t.Fatalf("应跑两轮模型（工具轮 + 结论轮），实得 %d", k7.doer.calls)
	}
	if len(out.Steps) != 1 || out.Steps[0].Name != "profile.query" || out.Steps[0].Status != kitctx.StepStatusDone {
		t.Fatalf("步骤轨迹不对：%+v", out.Steps)
	}
	if !strings.Contains(out.Verdict, "demo-profile-07") {
		t.Fatalf("结论没回显：%q", out.Verdict)
	}
	// 两件不同的事各自有落点，别混：
	// ① 调用轨迹 = 步骤序列自带 traceid/序号（可按 traceid 回放， 系）；
	if out.Steps[0].TraceID != "tr1" || out.Steps[0].Seq != 1 {
		t.Fatalf("步骤轨迹不完整：{TraceID:%q Seq:%d}", out.Steps[0].TraceID, out.Steps[0].Seq)
	}
	// ② 注入的 Logger 确实被插件用起来（不是装饰品）；它记的是生命周期日志，不承担轨迹。
	if joined := strings.Join(k7.log.all(), "\n"); !strings.Contains(joined, "profile.query") &&
		!strings.Contains(joined, "装配完成") {
		t.Fatalf("注入的 Logger 一条都没被写过：%q", joined)
	}
}

func TestEngine_StepBudgetDegradesVisibly(t *testing.T) {
	k7 := build(t, []string{
		callReply("c1", "profile.query", `{}`),
		callReply("c2", "profile.query", `{}`),
		callReply("c3", "profile.query", `{}`),
		sayReply("收尾"),
	}, nil)
	e, err := New(k7.deps)
	if err != nil {
		t.Fatalf("New：%v", err)
	}
	defer func() { _ = e.Stop() }()
	ctx := kitctx.WithRequest(context.Background(), kitctx.RequestContext{
		UserID: "u1", Role: "support", TraceID: "tr-limit", Limits: kitctx.Limits{MaxSteps: 1}})
	out, err := e.Handle(ctx, "多跑几步")
	if err != nil {
		t.Fatalf("Handle：%v", err)
	}
	if !out.StepsLimited || out.Notice == "" {
		t.Fatalf("撞步数上限必须显式可识别：%+v", out)
	}
	if k7.doer.calls > 2 {
		t.Fatalf("预算生效后还多跑了模型（calls=%d）", k7.doer.calls)
	}
}

func TestEngine_ModelFailureDegradesNotSucceeds(t *testing.T) {
	k7 := build(t, nil, errors.New("connection refused"))
	e, err := New(k7.deps)
	if err != nil {
		t.Fatalf("New：%v", err)
	}
	defer func() { _ = e.Stop() }()
	out, err := e.Handle(kitctx.WithRequest(context.Background(), kitctx.RequestContext{
		UserID: "u1", Role: "support", TraceID: "tr2"}), "问一句")
	if err != nil {
		t.Fatalf("模型不可用不该整体报错（要落成降级）：%v", err)
	}
	if !out.Degraded || out.Notice == "" {
		t.Fatalf("模型不可用必须降级且给冻结文案：%+v", out)
	}
}

func TestEngine_StopRefusesFurtherRequests(t *testing.T) {
	k7 := build(t, []string{sayReply("答复")}, nil)
	e, err := New(k7.deps)
	if err != nil {
		t.Fatalf("New：%v", err)
	}
	// 没有 traceid ⇒ 循环按设计拒绝（轨迹不可回放就不许进循环）。引擎**不**替宿主绕过这道门。
	if _, err := e.Handle(kitctx.WithRequest(context.Background(), kitctx.RequestContext{
		UserID: "u1", Role: "support"}), "无 traceid 的第一问"); err == nil ||
		!strings.Contains(err.Error(), "trace_id") {
		t.Fatalf("缺 traceid 应被循环拒绝且错误可诊断，实得 %v", err)
	}
	if _, err := e.Handle(kitctx.WithRequest(context.Background(), kitctx.RequestContext{
		UserID: "u1", Role: "support", TraceID: "tr-stop"}), "第一问"); err != nil {
		t.Fatalf("带 traceid 时停止前应可服务：%v", err)
	}
	if err := e.Stop(); err != nil {
		t.Fatalf("Stop：%v", err)
	}
	if err := e.Stop(); err != nil {
		t.Fatalf("Stop 应幂等：%v", err)
	}
	if _, err := e.Handle(context.Background(), "第二问"); !errors.Is(err, ErrEngineStopped) {
		t.Fatalf("停止后应拒绝服务，实得 %v", err)
	}
}

// 编译期确认：可选扩展走同一条 Use/Build 路径（审计件即为一例）。
var _ = audit.Sink(nil)
