package model

import (
	"encoding/json"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

// recordingDoer 记录实际发出的 payload。
// 这正是「注入 HTTPDoer」的唯一目的：审计落的是自报证据，
// 只有让调用方注入 doer，证据才能与实际 payload 做交叉核验。
type recordingDoer struct {
	lastBody []byte
	calls    int
	resp     *http.Response
	err      error
}

func (d *recordingDoer) Do(req *http.Request) (*http.Response, error) {
	d.calls++
	if req.Body != nil {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		_ = req.Body.Close()
		d.lastBody = body
	}
	if d.err != nil {
		return nil, d.err
	}
	return d.resp, nil
}

// 编译期契约：stdlib *http.Client 必须可直接作为 HTTPDoer 注入（ 只用 stdlib net/http）。
var _ HTTPDoer = (*http.Client)(nil)

// 编译期契约：审计侧的记录型 doer 必须可注入。
var _ HTTPDoer = (*recordingDoer)(nil)

func TestHTTPDoer_InjectedFakeSeesActualPayload(t *testing.T) {
	doer := &recordingDoer{resp: &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"choices":[]}`))}}

	req := ChatRequest{
		Messages: []ChatMessage{{Role: "user", Content: "查一下 1.2.3.4"}},
		Tools: VisibleDecls([]ToolDecl{
			{Name: "user.profile", Params: map[string]any{"type": "object"}, Summary: "IP 档案", RequiredRoles: []string{"admin"}},
		}),
	}
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal ChatRequest: %v", err)
	}
	httpReq, err := http.NewRequest(http.MethodPost, "http://llm.internal/v1/chat/completions", strings.NewReader(string(raw)))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if _, err := doer.Do(httpReq); err != nil {
		t.Fatalf("do: %v", err)
	}
	if doer.calls != 1 {
		t.Fatalf("want 1 call, got %d", doer.calls)
	}
	if string(doer.lastBody) != string(raw) {
		t.Fatalf("注入的 doer 未看到实际 payload:\n got=%s\nwant=%s", doer.lastBody, raw)
	}
}

func TestToolDecl_VisibleShapeExactlyFourKeys(t *testing.T) {
	decl := ToolDecl{
		Name:          "query_log",
		Params:        map[string]any{"type": "object", "properties": map[string]any{"kw": map[string]any{"type": "string"}}},
		Summary:       "按关键词查询日志",
		RequiredRoles: []string{"admin"},
		Source:        "log",
		ResultShape:   "items[]{time,path,status}",
	}

	visible := decl.Visible()
	if visible.Name != decl.Name || visible.Summary != decl.Summary {
		t.Fatalf("name/summary 未映射: %+v", visible)
	}
	if !reflect.DeepEqual(visible.Params, decl.Params) {
		t.Fatalf("params 未逐值映射: %#v", visible.Params)
	}
	if visible.Role != "admin" {
		t.Fatalf("role 未映射: %q", visible.Role)
	}

	raw, err := json.Marshal(visible)
	if err != nil {
		t.Fatalf("marshal ToolVisible: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal ToolVisible: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("模型可见外形必须恰为 4 个键，got %d: %s", len(got), raw)
	}
	for _, key := range []string{"name", "params", "summary", "role"} {
		if _, ok := got[key]; !ok {
			t.Fatalf("缺少 契约点 要求的键 %q: %s", key, raw)
		}
	}
}

func TestToolDecl_VisibleRoleMapping(t *testing.T) {
	cases := []struct {
		name  string
		roles []string
		want  string
	}{
		{name: "单角色原样", roles: []string{"admin"}, want: "admin"},
		{name: "多角色去重且稳定排序", roles: []string{"support", "admin", "support"}, want: "admin,support"},
		{name: "未声明角色", roles: nil, want: ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := (ToolDecl{RequiredRoles: c.roles}).Visible().Role; got != c.want {
				t.Fatalf("role=%q, want %q", got, c.want)
			}
		})
	}
}

func TestToolDecl_VisibleDoesNotMutateRequiredRoles(t *testing.T) {
	decl := ToolDecl{Name: "correlate", RequiredRoles: []string{"support", "admin"}}
	_ = decl.Visible()
	if !reflect.DeepEqual(decl.RequiredRoles, []string{"support", "admin"}) {
		t.Fatalf("Visible 篡改了内部 RequiredRoles: %#v", decl.RequiredRoles)
	}
}

func TestChatRequest_InternalFieldsStayOffWire(t *testing.T) {
	req := ChatRequest{
		Messages: []ChatMessage{{Role: "user", Content: "hi"}},
		Tools: VisibleDecls([]ToolDecl{{
			Name:          "query_code",
			Params:        map[string]any{"type": "object"},
			Summary:       "代码问答",
			RequiredRoles: []string{"admin"},
			Source:        "hermes",
			ResultShape:   "answer{owner,source}",
		}}),
	}
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal ChatRequest: %v", err)
	}
	for _, forbidden := range []string{"RequiredRoles", "required_roles", "Source", "source", "ResultShape", "result_shape"} {
		if strings.Contains(string(raw), `"`+forbidden+`"`) {
			t.Fatalf("内部字段 %q 泄漏到模型可见 payload: %s", forbidden, raw)
		}
	}
	var got struct {
		Tools []map[string]any `json:"tools"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal ChatRequest: %v", err)
	}
	if len(got.Tools) != 1 || len(got.Tools[0]) != 4 {
		t.Fatalf("$.tools[] 必须恰为 4 个键: %s", raw)
	}
}

func TestChatRequest_MarshalDeterministic(t *testing.T) {
	build := func() ChatRequest {
		return ChatRequest{
			Messages: []ChatMessage{{Role: "system", Content: "s"}, {Role: "user", Content: "u"}},
			Tools: VisibleDecls([]ToolDecl{{
				Name:    "user.profile",
				Params:  map[string]any{"b": 2, "a": map[string]any{"z": 1, "y": 2}},
				Summary: "IP 档案",
			}}),
		}
	}
	first, err := json.Marshal(build())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	second, err := json.Marshal(build())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(first) != string(second) {
		t.Fatalf("同一请求两次序列化不一致，HMAC 交叉核验不可复现:\n%s\n%s", first, second)
	}
}

func TestChatResponse_ParsesToolCalls(t *testing.T) {
	body := `{"choices":[{"index":0,"message":{"role":"assistant","content":"","tool_calls":[
		{"id":"call_1","type":"function","function":{"name":"user.profile","arguments":"{\"ip\":\"1.2.3.4\"}"}}]},
		"finish_reason":"tool_calls"}]}`
	var resp ChatResponse
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal ChatResponse: %v", err)
	}
	if len(resp.Choices) != 1 {
		t.Fatalf("choices=%d", len(resp.Choices))
	}
	choice := resp.Choices[0]
	if choice.FinishReason != "tool_calls" {
		t.Fatalf("finish_reason=%q", choice.FinishReason)
	}
	if len(choice.Message.ToolCalls) != 1 {
		t.Fatalf("tool_calls=%d", len(choice.Message.ToolCalls))
	}
	call := choice.Message.ToolCalls[0]
	if call.ID != "call_1" || call.Function.Name != "user.profile" {
		t.Fatalf("tool call 未解析: %+v", call)
	}
	args, err := call.Args()
	if err != nil {
		t.Fatalf("Args: %v", err)
	}
	if args["ip"] != "1.2.3.4" {
		t.Fatalf("args=%#v", args)
	}
}

func TestToolCall_ArgsRejectsMalformedArguments(t *testing.T) {
	call := ToolCall{ID: "call_2", Function: ToolFunction{Name: "user.profile", Arguments: "{not-json"}}
	if _, err := call.Args(); err == nil {
		t.Fatal("非法 arguments 必须返回可识别错误，而不是静默空 map")
	}
}

//	的框架侧前提：协议层零 宿主依赖。
//
// 只扫描本任务产出的 protocol.go，避免约束同包其它任务新增的文件。
func TestProtocol_StdlibOnlyImports(t *testing.T) {
	const file = "protocol.go"
	allowed := map[string]bool{
		"encoding/json": true,
		"fmt":           true,
		"net/http":      true,
		"sort":          true,
		"strings":       true,
	}

	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, file, nil, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	for _, spec := range parsed.Imports {
		path := strings.Trim(spec.Path.Value, `"`)
		if !allowed[path] {
			t.Fatalf("%s 引入了非 stdlib 白名单依赖 %q（契约点: agentkit 零 宿主依赖）", file, path)
		}
	}
	if len(parsed.Imports) == 0 {
		t.Fatalf("%s 未引入 net/http，HTTPDoer 契约缺失", file)
	}
}
