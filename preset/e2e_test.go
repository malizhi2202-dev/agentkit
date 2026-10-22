package preset

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/malizhi2202-dev/agentkit/audit"
	"github.com/malizhi2202-dev/agentkit/authz"
	"github.com/malizhi2202-dev/agentkit/kit"
	"github.com/malizhi2202-dev/agentkit/kitctx"
	"github.com/malizhi2202-dev/agentkit/model"
	"github.com/malizhi2202-dev/agentkit/tools"
	"github.com/malizhi2202-dev/agentkit/web"
)

// 本文件是「一个程序用 agentkit 装起来并真跑一轮」的端到端判据。
//
// 为什么放在测试里而不是 `cmd/` 下的样例程序里（这点是**被守卫逼出来的**，不是偷懒）：
// `既有模型层/guard_test.go` 全仓 AST 扫描规定，`model.NewClient` / `model.ClientConfig`
// 这类模型请求构造点只允许出现在 `{既有模型层/**, pkg/agentkit/model/**}`。
// 一个 cmd/ 下的样例程序自己构造客户端就是绕出口 —— 与其给守卫加豁免（等于放宽不变量），
// 不如把可运行的样例做成 `pkg/agentkit/samples/agentkit-demo.go.txt` 模板，
// 在**陌生 module** 里 `go run`（`.go` 之外守卫看不见模板，宿主构建也不含它）；
// 仓内的等价断言就放在这里（两个守卫都豁免 `_test.go`）。

type scriptDoer struct {
	steps []string
	calls int
	seen  [][]string
}

func (d *scriptDoer) Do(req *http.Request) (*http.Response, error) {
	var wire struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.NewDecoder(req.Body).Decode(&wire); err != nil {
		return nil, fmt.Errorf("解析出域请求失败：%w", err)
	}
	visible := make([]string, 0, len(wire.Tools))
	for _, t := range wire.Tools {
		visible = append(visible, t.Name)
	}
	idx := d.calls
	d.calls++
	d.seen = append(d.seen, visible)
	body := `{"choices":[{"index":0,"message":{"role":"assistant","content":"脚本用尽"}}],"finish_reason":"stop"}`
	if idx < len(d.steps) {
		body = d.steps[idx]
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
	}, nil
}

// wire* 是网关回复的最小形状（用 Marshal 造，避免手拼 JSON/嵌套 map 的花括号错误）。
type wireResponse struct {
	Choices []wireChoice `json:"choices"`
}

type wireChoice struct {
	Index        int         `json:"index"`
	Message      wireMessage `json:"message"`
	FinishReason string      `json:"finish_reason"`
}

type wireMessage struct {
	Role      string           `json:"role"`
	Content   string           `json:"content"`
	ToolCalls []model.ToolCall `json:"tool_calls,omitempty"`
}

func marshalBody(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func assistantToolCall(id, name, args string) string {
	return marshalBody(wireResponse{Choices: []wireChoice{{
		Index: 0,
		Message: wireMessage{Role: "assistant", ToolCalls: []model.ToolCall{{
			ID: id, Type: "function", Function: model.ToolFunction{Name: name, Arguments: args},
		}}},
		FinishReason: "tool_calls",
	}}})
}

func assistantText(content string) string {
	return marshalBody(wireResponse{Choices: []wireChoice{{
		Index:        0,
		Message:      wireMessage{Role: "assistant", Content: content},
		FinishReason: "stop",
	}}})
}

func profileTool() tools.Tool {
	return tools.Tool{
		Name:          "user.profile",
		Summary:       "查询 IP 的归属与档案（样例假数据）",
		Params:        map[string]any{"type": "object", "properties": map[string]any{"ip": map[string]any{"type": "string"}}},
		RequiredRoles: nil, // 由权限点生成 ⇒ 此处必须留空
		Run: func(_ context.Context, args map[string]any) (any, error) {
			ip, _ := args["ip"].(string)
			if ip == "" {
				return nil, errors.New("缺少 ip 参数")
			}
			return map[string]any{"ip": ip, "profile": "demo-profile-01", "zone": "机房A"}, nil
		},
	}
}

func selfTool() tools.Tool {
	return tools.Tool{
		Name:    "whoami",
		Summary: "回显当前身份（样例）",
		Run:     func(context.Context, map[string]any) (any, error) { return "demo", nil },
	}
}

// closingSink 代表"带缓冲"的 sink：Close 不被调用就会丢事后要举证的事实。
type closingSink struct {
	rec    audit.Recalling
	closed int
}

func (c *closingSink) Emit(ctx context.Context, e audit.Entry) error { return c.rec.Emit(ctx, e) }
func (c *closingSink) Close() error                                  { c.closed++; return nil }

// askResult 是一次端到端装配的产物（把 kit 也交出来，才能断言 Stop 的撤销语义）。
type askResult struct {
	visible []string
	verdict string
	steps   int
	order   []string
	k       *kit.Kit
	aud     *Audit // nil ⇒ 本次未接审计
}

// assembleAndAsk 就是"五步装配"的全部代码：程序侧真正需要写的只有这些。
func assembleAndAsk(t *testing.T, doer model.HTTPDoer, role kitctx.Role, question string, auditSink audit.Sink) askResult {
	t.Helper()
	roles, err := authz.NewRoleSet("support", "auditor", "admin")
	if err != nil {
		t.Fatalf("NewRoleSet：%v", err)
	}
	perms, err := NewPerms(roles,
		authz.PermissionPoint{Key: "user.profile.read", Label: "IP 领域对象档案", Scope: authz.ScopePlatform,
			DefaultRoles: []kitctx.Role{"support", "admin"}, Enforced: authz.DirectEnforcement()},
		authz.PermissionPoint{Key: "identity.self", Label: "自身身份回显", Scope: authz.ScopeProject,
			DefaultRoles: []kitctx.Role{"support", "auditor", "admin"}, Enforced: authz.DirectEnforcement()},
	)
	if err != nil {
		t.Fatalf("NewPerms：%v", err)
	}
	client := model.NewClient(model.ClientConfig{
		Doer:        doer,
		Policy:      model.NewEndpointPolicy(model.EndpointPolicyConfig{AllowedHosts: []string{"192.168.16.16"}}),
		Timeout:     2 * time.Second,
		MaxAttempts: 1,
	})
	m, err := NewModel(client, "http://192.168.16.16:8080/v1/chat/completions")
	if err != nil {
		t.Fatalf("NewModel：%v", err)
	}
	decl := NewTools(perms, Granted(profileTool(), "user.profile.read"), Granted(selfTool(), "identity.self"))
	loop := NewLoop(m)

	var aud *Audit
	plugins := []kit.Plugin{loop, decl, perms, m} // 故意把 loop 放最前
	if auditSink != nil {
		fan, err := audit.New(audit.BlacklistMasker(), nil, auditSink)
		if err != nil {
			t.Fatalf("audit.New：%v", err)
		}
		aud, err = NewAudit(fan)
		if err != nil {
			t.Fatalf("NewAudit：%v", err)
		}
		plugins = append(plugins, aud)
	}

	k, err := kit.New(kit.Config{})
	if err != nil {
		t.Fatalf("kit.New：%v", err)
	}
	if err := k.Use(plugins...); err != nil {
		t.Fatalf("Use：%v", err)
	}
	if err := k.Build(); err != nil {
		t.Fatalf("Build：%v", err)
	}
	t.Cleanup(func() { _ = k.Stop() })

	ctx := kitctx.WithRequest(context.Background(), kitctx.RequestContext{
		UserID: "u", Role: role, TraceID: "trace",
		Limits: kitctx.Limits{MaxSteps: 4, StepTimeout: 2 * time.Second},
	})
	out, err := loop.Ask(ctx, question)
	if err != nil {
		t.Fatalf("Ask：%v", err)
	}
	if out.Degraded {
		t.Fatalf("被降级了（notice=%q err=%v）⇒ 出域链路没真跑通", out.Notice, out.Err)
	}
	d := doer.(*scriptDoer)
	if len(d.seen) == 0 {
		t.Fatal("模型一次都没被调用")
	}
	return askResult{visible: d.seen[0], verdict: out.Verdict, steps: len(out.Steps), order: k.ApplyOrder(), k: k, aud: aud}
}

// TestPreset_EndToEnd_AssembledAgentRuns 是框架"能被真的用起来"的判据：
// 装配顺序由依赖决定、工具可见性由权限点决定、结论由工具回传的事实决定。
func TestPreset_EndToEnd_AssembledAgentRuns(t *testing.T) {
	doer := &scriptDoer{steps: []string{
		assistantToolCall("call_1", "user.profile", `{"ip":"1.2.3.4"}`),
		assistantText("1.2.3.4 属于 demo-profile-01（机房A）"),
	}}
	r := assembleAndAsk(t, doer, "support", "1.2.3.4 是什么领域对象", nil)
	visible, verdict, steps, order := r.visible, r.verdict, r.steps, r.order

	if strings.Join(order, ",") != "preset.model,preset.loop,preset.tools,preset.perms" {
		t.Fatalf("Apply 顺序应把被依赖的 model 排最前，实得 %v", order)
	}
	if strings.Join(visible, ",") != "user.profile,whoami" {
		t.Fatalf("support 应看到两条工具，实得 %v", visible)
	}
	if steps != 1 {
		t.Fatalf("应恰好记录一次工具调用，实得 %d", steps)
	}
	if !strings.Contains(verdict, "demo-profile-01") {
		t.Fatalf("结论必须由工具回传的事实支撑，实得 %q", verdict)
	}
}

// TestPreset_EndToEnd_RoleChangeHidesTool 是"接口/工具/权限同源"的运行期证据：
// 同一条声明、换一个角色，模型看到的能力集合就变了 —— 而判定只有 tools 那一条谓词。
func TestPreset_EndToEnd_RoleChangeHidesTool(t *testing.T) {
	doer := &scriptDoer{steps: []string{assistantText("我没有这个 IP 的权限")}}
	r := assembleAndAsk(t, doer, "auditor", "1.2.3.4 是什么领域对象", nil)
	visible, verdict, steps := r.visible, r.verdict, r.steps

	if strings.Join(visible, ",") != "whoami" {
		t.Fatalf("auditor 只应看到 whoami（user.profile 的权限点里没有它），实得 %v", visible)
	}
	if steps != 0 {
		t.Fatalf("不可见工具不该被调用，实得 %d 步", steps)
	}
	if verdict == "" {
		t.Fatal("纯文本回答不应为空")
	}
}

// TestPreset_EndToEnd_AuditTrailIsMasked 让"框架被用起来"这条证据覆盖审计：
// 装配后的程序发一条带 imei 的事实 ⇒ sink 只可能拿到脱敏后的载荷，且 Stop 必须 flush。
func TestPreset_EndToEnd_AuditTrailIsMasked(t *testing.T) {
	sink := &closingSink{}
	doer := &scriptDoer{steps: []string{assistantText("已核实")}}
	r := assembleAndAsk(t, doer, "support", "查 1.2.3.4", sink)
	if r.aud == nil {
		t.Fatal("未接审计")
	}
	ctx := kitctx.WithRequest(context.Background(), kitctx.RequestContext{
		UserID: "u1", Role: "support", TraceID: "trace-9",
	})
	if err := r.aud.Emit(ctx, audit.Entry{
		Action:  "agent.ask",
		Target:  "1.2.3.4",
		Outcome: audit.OutcomeOK,
		Fields:  map[string]any{"ip": "1.2.3.4", "imei": "869990001234567"},
	}); err != nil {
		t.Fatalf("Emit：%v", err)
	}
	got := sink.rec.Entries()
	if len(got) != 1 {
		t.Fatalf("sink 收到 %d 条", len(got))
	}
	e := got[0]
	if _, ok := e.Fields["imei"]; ok {
		t.Fatalf("imei 原样进了 sink：%v", e.Fields)
	}
	if e.Fields["ip"] != "1.2.3.4" {
		t.Fatalf("非敏感字段被删：%v", e.Fields)
	}
	if !e.Sanitized() || len(e.Applied) == 0 {
		t.Fatalf("确实脱掉了字段却拿不到证据：applied=%v", e.Applied)
	}
	if e.TraceID != "trace-9" || e.UserID != "u1" || e.Role != "support" {
		t.Fatalf("ctx 身份未补齐：%+v", e)
	}
	if e.Action != "agent.ask" || e.Sign != "agent.ask" {
		t.Fatalf("Action/Sign 口径丢失：%+v", e)
	}
	if err := r.k.Stop(); err != nil {
		t.Fatalf("Stop：%v", err)
	}
	if sink.closed != 1 {
		t.Fatalf("Stop 未 flush 审计 sink（closed=%d）⇒ 撤销只停了一半", sink.closed)
	}
}

// TestPreset_EndToEnd_HTTPServesAssembledAgent 是"一个程序用框架搭起来"的最完整形态：
// HTTP 入口（身份注入）→ 权限裁剪的工具 → 调用循环 → 审计投递，全在一条请求链上。
//
// 用 `srv.Handler` 交给 httptest（真实 TCP 回环 + 真实 HTTP 报文），
// 而不是自己 dial：端口归属仍由 httptest 管，本测试不假设任何固定端口。
func TestPreset_EndToEnd_HTTPServesAssembledAgent(t *testing.T) {
	roles, err := authz.NewRoleSet("support", "auditor", "admin")
	if err != nil {
		t.Fatalf("NewRoleSet：%v", err)
	}
	perms, err := NewPerms(roles,
		authz.PermissionPoint{Key: "user.profile.read", Label: "IP 领域对象档案", Scope: authz.ScopePlatform,
			DefaultRoles: []kitctx.Role{"support", "admin"}, Enforced: authz.DirectEnforcement()},
	)
	if err != nil {
		t.Fatalf("NewPerms：%v", err)
	}
	rt := web.NewRouter(web.Config{
		MaxBodyBytes: 4 << 10,
		Identity: func(r *http.Request) (kitctx.RequestContext, error) {
			role := r.Header.Get("X-Role")
			if role == "" {
				return kitctx.RequestContext{}, web.Errorf(web.ErrKindUnauthenticated, "缺少身份头")
			}
			return kitctx.RequestContext{UserID: "u-http", Role: kitctx.Role(role), TraceID: "trace-http",
				Limits: kitctx.Limits{MaxSteps: 4, StepTimeout: 2 * time.Second}}, nil
		},
	})
	client := model.NewClient(model.ClientConfig{
		Doer:        &scriptDoer{steps: []string{assistantToolCall("c1", "user.profile", `{"ip":"9.9.9.9"}`), assistantText("9.9.9.9 属于 demo-profile-09")}},
		Policy:      model.NewEndpointPolicy(model.EndpointPolicyConfig{AllowedHosts: []string{"192.168.16.16"}}),
		MaxAttempts: 1,
	})
	m, err := NewModel(client, "http://192.168.16.16:8080/v1/chat/completions")
	if err != nil {
		t.Fatalf("NewModel：%v", err)
	}
	loop := NewLoop(m)
	decl := NewTools(perms, Granted(profileTool(), "user.profile.read"))
	snk := &closingSink{}
	fan, err := audit.New(audit.BlacklistMasker(), nil, snk)
	if err != nil {
		t.Fatalf("audit.New：%v", err)
	}
	aud, err := NewAudit(fan)
	if err != nil {
		t.Fatalf("NewAudit：%v", err)
	}
	srv, err := web.NewServer(web.ServerConfig{Router: rt, Addr: "127.0.0.1:0"})
	if err != nil {
		t.Fatalf("NewServer：%v", err)
	}
	sp := NewServer(srv)

	// 接口自己就是一个可被 agent 调用的动作：此处先手写，P5 让注册即同源生成。
	if err := rt.Register(web.Route{Method: "POST", Path: "/engine/ask", Name: "engine.ask", Perm: "user.profile.read", Summary: "问答"},
		func(w http.ResponseWriter, r *http.Request) error {
			var in struct {
				Question string `json:"question"`
			}
			if err := web.DecodeJSON(r, &in); err != nil {
				return err
			}
			role, ok := kitctx.RoleFrom(r.Context())
			if !ok {
				return web.Errorf(web.ErrKindUnauthenticated, "角色不可判定")
			}
			out, err := loop.Ask(r.Context(), in.Question)
			if err != nil {
				return web.Errorf(web.ErrKindInternal, "调用循环失败")
			}
			if err := aud.Emit(r.Context(), audit.Entry{
				Action: "engine.ask", Target: in.Question, Outcome: audit.OutcomeOK,
				Fields: map[string]any{"question": in.Question, "role": string(role), "imei": "869990007654321"},
			}); err != nil {
				return web.Errorf(web.ErrKindInternal, "审计投递失败")
			}
			web.WriteJSON(w, out.Verdict)
			return nil
		}); err != nil {
		t.Fatalf("Register：%v", err)
	}

	k, err := kit.New(kit.Config{})
	if err != nil {
		t.Fatalf("kit.New：%v", err)
	}
	if err := k.Use(sp, loop, decl, perms, m, aud); err != nil {
		t.Fatalf("Use：%v", err)
	}
	if err := k.Build(); err != nil {
		t.Fatalf("Build：%v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	post := func(role, body string) (*http.Response, map[string]any) {
		t.Helper()
		req, err := http.NewRequest("POST", ts.URL+"/engine/ask", strings.NewReader(body))
		if err != nil {
			t.Fatalf("NewRequest：%v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		if role != "" {
			req.Header.Set("X-Role", role)
		}
		resp, err := ts.Client().Do(req)
		if err != nil {
			t.Fatalf("请求失败：%v", err)
		}
		defer resp.Body.Close()
		var env struct {
			Code string `json:"code"`
			Msg  string `json:"msg"`
			Data any    `json:"data"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
			t.Fatalf("响应解码失败：%v", err)
		}
		return resp, map[string]any{"code": env.Code, "msg": env.Msg, "data": env.Data}
	}

	// ① 有权角色：真跑出一轮工具调用并拿到工具回传的事实
	resp, body := post("support", `{"question":"9.9.9.9 是什么领域对象"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("有权请求应 200，实得 %d（%v）", resp.StatusCode, body)
	}
	if verdict, _ := body["data"].(string); !strings.Contains(verdict, "demo-profile-09") {
		t.Fatalf("结论未由工具事实支撑：%v", body)
	}

	// ② 无身份：401，处理函数（含审计投递）根本没被调用
	resp, body = post("", `{"question":"hi"}`)
	if resp.StatusCode != http.StatusUnauthorized || body["code"] != string(web.ErrKindUnauthenticated) {
		t.Fatalf("缺身份应 401/unauthenticated，实得 %d %v", resp.StatusCode, body)
	}

	// ③ 无权角色：工具不可见 ⇒ 走不到 user.profile，但接口本身仍可访问（授权发生在工具可见性上）
	resp, body = post("auditor", `{"question":"9.9.9.9 是什么领域对象"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("auditor 请求应 200（权限只裁工具，不裁接口），实得 %d", resp.StatusCode)
	}

	// ④ 审计：②③① 里只有两次进入处理函数的请求留下事实；imei 从未出现在载荷里
	entries := snk.rec.Entries()
	if len(entries) != 2 {
		t.Fatalf("应有 2 条审计事实（无身份那次不该进处理函数），实得 %d", len(entries))
	}
	for _, e := range entries {
		if _, bad := e.Fields["imei"]; bad {
			t.Fatalf("imei 原样入了 sink：%v", e.Fields)
		}
		if !e.Sanitized() {
			t.Fatalf("确实该有被移除的字段，却没有证据：%+v", e)
		}
		if e.TraceID != "trace-http" || e.Role == "" {
			t.Fatalf("ctx 身份未进审计：%+v", e)
		}
	}

	// ⑤ Stop 必须把服务器与审计 sink 一起收掉
	if err := k.Stop(); err != nil {
		t.Fatalf("Stop：%v", err)
	}
	if err := sp.Serve(); !errors.Is(err, ErrServerStopped) {
		t.Fatalf("停止后再 Serve 应判 ErrServerStopped，实得 %v", err)
	}
	if aud.Fanout() != nil {
		t.Fatal("停止后审计仍可用")
	}
}

// TestPreset_EndToEnd_ModelCallsServedRouteAsTool 是这次沉淀的落点：
// 一个只用一行 `preset.Served` 声明的 HTTP 接口，被模型当成工具选中，
// 并由调用循环**在进程内**执行同一个处理函数（不经网络）。
func TestPreset_EndToEnd_ModelCallsServedRouteAsTool(t *testing.T) {
	askFact := func(w http.ResponseWriter, r *http.Request) error {
		var in askIn
		if err := web.DecodeJSON(r, &in); err != nil {
			in.Q = r.URL.Query().Get("q")
		}
		role, _ := kitctx.RoleFrom(r.Context())
		web.WriteJSON(w, map[string]string{
			"answer": "1.2.3.4 属于 demo-profile-01（机房A）",
			"role":   string(role),
		})
		return nil
	}
	for _, c := range []struct {
		role      kitctx.Role
		wantCalls int
	}{
		{role: "support", wantCalls: 1}, // 权限点里有 support ⇒ 工具可见 ⇒ 处理函数被真调用
		{role: "auditor", wantCalls: 0}, // 无权限点角色 ⇒ 模型根本看不见这个工具
	} {
		var calls int
		h := func(w http.ResponseWriter, r *http.Request) error {
			calls++
			return askFact(w, r)
		}
		src, err := Served(web.Route{Method: "POST", Path: "/engine/ask", Name: "engine.ask"}, askIn{}, "engine.ask", h)
		if err != nil {
			t.Fatalf("Served：%v", err)
		}
		rt := web.NewRouter(web.Config{})
		doer := &scriptDoer{steps: []string{
			assistantToolCall("c1", "engine.ask", `{"q":"1.2.3.4 是什么领域对象"}`),
			assistantText("1.2.3.4 属于 demo-profile-01（机房A）"),
		}}
		client := model.NewClient(model.ClientConfig{
			Doer:        doer,
			Policy:      model.NewEndpointPolicy(model.EndpointPolicyConfig{AllowedHosts: []string{"192.168.16.16"}}),
			MaxAttempts: 1,
		})
		m, err := NewModel(client, "http://192.168.16.16:8080/v1/chat/completions")
		if err != nil {
			t.Fatalf("NewModel：%v", err)
		}
		k, err := kit.New(kit.Config{Web: rt})
		if err != nil {
			t.Fatalf("kit.New：%v", err)
		}
		lp := NewLoop(m)
		if err := k.Use(lp, NewTools(servedPerms(t), src), servedPerms(t), m); err != nil {
			t.Fatalf("Use：%v", err)
		}
		if err := k.Build(); err != nil {
			t.Fatalf("Build：%v", err)
		}
		ctx := kitctx.WithRequest(context.Background(), kitctx.RequestContext{
			UserID: "u-e2e", Role: c.role, TraceID: "tr-e2e",
			Limits: kitctx.Limits{MaxSteps: 4, StepTimeout: 2 * time.Second},
		})
		out, err := lp.Run(ctx, []model.ChatMessage{{Role: "user", Content: "1.2.3.4 是什么领域对象"}})
		if err != nil {
			t.Fatalf("Run：%v", err)
		}
		if out.Degraded {
			t.Fatalf("降级了：%v", out.Err)
		}
		// 路由表与工具表同源：一次声明 ⇒ 各一条
		if n := len(rt.Routes()); n != 1 {
			t.Fatalf("角色 %s：路由表应有 1 条，实得 %d", c.role, n)
		}
		visible := map[string]bool{}
		for _, v := range k.Tools().VisibleByRole(string(c.role)) {
			visible[v.Name] = true
		}
		if got := visible["engine.ask"]; got != (c.wantCalls == 1) {
			t.Fatalf("角色 %s 的工具可见性错：%v", c.role, visible)
		}
		if calls != c.wantCalls {
			t.Fatalf("角色 %s：处理函数被调 %d 次，期望 %d", c.role, calls, c.wantCalls)
		}
		if c.wantCalls == 1 {
			if len(out.Steps) != 1 || out.Steps[0].Name != "engine.ask" {
				t.Fatalf("应留下一条 engine.ask 步骤：%+v", out.Steps)
			}
			if !strings.Contains(out.Verdict, "demo-profile-01") {
				t.Fatalf("结论未落到工具事实：%q", out.Verdict)
			}
			// 回流证据在模型上下文里（轨迹按设计只留摘要，见 kitctx/trace.go）
			var fact string
			for _, msg := range out.Messages {
				if msg.Role == "tool" {
					fact = msg.Content
				}
			}
			if !strings.Contains(fact, "demo-profile-01") {
				t.Fatalf("接口响应未回流进模型上下文：%q", fact)
			}
			if !strings.Contains(fact, `"role":"support"`) {
				t.Fatalf("ctx 身份未传到处理函数（框架不造身份）：%q", fact)
			}
			if strings.Contains(out.Steps[0].ResultSummary, "demo-profile-01") {
				t.Fatalf("步骤摘要里出现了响应原文：%s", out.Steps[0].ResultSummary)
			}
		} else {
			// 无权角色：册内但被拒 ⇒ 处理函数一次都不该被调，事实要留在 Rejected 里，
			// 且被拒的那一步仍以 failed 落进轨迹（可审计"模型试了但没权限"）。
			if !containsName(out.Rejected, "engine.ask") {
				t.Fatalf("被拒调用未记进 Rejected：%v", out.Rejected)
			}
			if len(out.Steps) != 1 || out.Steps[0].Status != kitctx.StepStatusFailed {
				t.Fatalf("被拒步骤应以 failed 留痕：%+v", out.Steps)
			}
		}
		if err := k.Stop(); err != nil {
			t.Fatalf("Stop：%v", err)
		}
	}
}

func containsName(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
