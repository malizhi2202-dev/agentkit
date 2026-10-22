package model

import (
	"context"
	"errors"
	"go/parser"
	"go/token"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeResolver 是运行期二次校验的注入点替身：不发真实 DNS，使「解析到公网 / 解析失败」两态可确定性复现。
type fakeResolver struct {
	ips  []net.IP
	err  error
	call int
	host string
}

func (r *fakeResolver) LookupIP(_ context.Context, _, host string) ([]net.IP, error) {
	r.call++
	r.host = host
	if r.err != nil {
		return nil, r.err
	}
	return r.ips, nil
}

// 编译期契约：stdlib *net.Resolver 必须可直接注入（与 HTTPDoer 同构的注入风格）。
var _ IPResolver = (*net.Resolver)(nil)
var _ IPResolver = (*fakeResolver)(nil)

func mustRequest(t *testing.T, rawURL string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, rawURL, nil)
	if err != nil {
		t.Fatalf("构造请求 %q 失败: %v", rawURL, err)
	}
	return req
}

func internalPolicy() *EndpointPolicy {
	return NewEndpointPolicy(EndpointPolicyConfig{
		AllowedHosts:          []string{"llm-gw"},
		AllowedDomainSuffixes: []string{"internal.internal"},
	})
}

// / NFR-S2：加载期默认拒绝公网端点，且错误必须可识别（供 LOG:llm.endpoint.rejected）。
func TestEndpoint_LoadRejectsPublicEndpoint(t *testing.T) {
	policy := internalPolicy()

	cases := []struct {
		name string
		url  string
		want error
	}{
		{"公网域名不在注入清单内", "https://api.openai.com/v1/chat/completions", ErrEndpointNotAllowlisted},
		{"公网域名后缀相似但边界不符", "https://internal.internal.attacker.com/v1", ErrEndpointNotAllowlisted},
		{"公网 IP 字面量", "https://8.8.8.8/v1", ErrEndpointPublic},
		{"公网 IPv6 字面量", "https://[2001:db8::1]/v1", ErrEndpointPublic},
		{"链路本地地址默认拒（fail-closed）", "http://169.254.169.254/latest/meta-data", ErrEndpointPublic},
		{"未指定地址默认拒", "http://0.0.0.0:8080/v1", ErrEndpointPublic},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := policy.ValidateEndpoint(tc.url)
			if err == nil {
				t.Fatalf("公网端点 %q 必须被加载期拒绝，实际放行并返回 %q", tc.url, got)
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("错误必须可识别：want errors.Is(%v), got %v", tc.want, err)
			}
		})
	}
}

// RFC1918 / 回环 / IPv6 ULA 一律放行；非私网段（含 CGNAT、链路本地、组播）一律拒绝。
func TestEndpoint_LoadAcceptsOnlyPrivateLiterals(t *testing.T) {
	policy := NewEndpointPolicy(EndpointPolicyConfig{})

	accepted := []string{
		"http://10.1.2.3:8080/v1",
		"http://172.16.0.1/v1",
		"http://172.31.255.254/v1",
		"http://192.168.1.1/v1",
		"http://127.0.0.1:9000/v1",
		"http://[::1]:8080/v1",
		"http://[fd00::1]/v1",
	}
	for _, raw := range accepted {
		got, err := policy.ValidateEndpoint(raw)
		if err != nil {
			t.Fatalf("私网端点 %q 应被放行，实际被拒: %v", raw, err)
		}
		if got != raw {
			t.Fatalf("放行的端点应原样返回规范化 URL：got %q want %q", got, raw)
		}
	}

	rejected := []string{
		"http://8.8.8.8/v1",
		"http://172.32.0.1/v1",
		"http://192.169.0.1/v1",
		"http://11.0.0.1/v1",
		"http://100.64.0.1/v1",  // CGNAT（RFC6598）不在 RFC1918 内
		"http://169.254.1.1/v1", // 链路本地
		"http://224.0.0.1/v1",   // 组播
		"http://[fe80::1]/v1",   // IPv6 链路本地
		"http://[2001:db8::1]/v1",
	}
	for _, raw := range rejected {
		if _, err := policy.ValidateEndpoint(raw); !errors.Is(err, ErrEndpointPublic) {
			t.Fatalf("非私网端点 %q 必须被拒（ErrEndpointPublic），实际: %v", raw, err)
		}
	}
}

// 内网域名仅放行调用方注入的清单；清单项同时接受裸主机名与 URL/带端口写法（配置形态由调用方决定）。
func TestEndpoint_LoadAcceptsAllowlistedInternalDomains(t *testing.T) {
	policy := NewEndpointPolicy(EndpointPolicyConfig{
		AllowedHosts:          []string{"llm-gw", "https://llm-alt.internal.internal", "llm-port:8080"},
		AllowedDomainSuffixes: []string{".svc.local", "internal.internal"},
	})

	accepted := []string{
		"https://llm-gw/v1",
		"https://llm-gw.internal.internal/v1",
		"http://a.b.internal.internal:8000/v1",
		"http://internal.internal/v1",
		"https://llm-alt.internal.internal/v1",
		"http://llm-port:9999/v1", // 主机名匹配，端口不参与策略
		"http://any.svc.local/v1",
	}
	for _, raw := range accepted {
		if _, err := policy.ValidateEndpoint(raw); err != nil {
			t.Fatalf("清单内端点 %q 应被放行，实际被拒: %v", raw, err)
		}
	}

	rejected := []string{
		"http://evilinternal.internal/v1", // 后缀边界（非 label 边界）
		"http://internal.internal.evil.com/v1",
		"http://llm-gw.attacker.com/v1",
		"http://llm-gw2/v1",
	}
	for _, raw := range rejected {
		if _, err := policy.ValidateEndpoint(raw); !errors.Is(err, ErrEndpointNotAllowlisted) {
			t.Fatalf("清单外端点 %q 必须被拒（ErrEndpointNotAllowlisted），实际: %v", raw, err)
		}
	}
}

func TestEndpoint_LoadRejectsMalformed(t *testing.T) {
	policy := NewEndpointPolicy(EndpointPolicyConfig{AllowedDomainSuffixes: []string{"internal.internal"}})

	cases := []struct {
		name string
		url  string
		want error
	}{
		{"空配置", "", ErrEndpointEmpty},
		{"纯空白", "   ", ErrEndpointEmpty},
		{"缺少 scheme", "llm.internal.internal/v1", ErrEndpointScheme},
		{"非 http(s) 协议", "ftp://10.0.0.1/v1", ErrEndpointScheme},
		{"缺少主机", "http:///v1", ErrEndpointMalformed},
		{"含 userinfo（可能把凭据带进日志）", "http://user:pw@10.0.0.1/v1", ErrEndpointMalformed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := policy.ValidateEndpoint(tc.url); !errors.Is(err, tc.want) {
				t.Fatalf("端点 %q 应返回 %v，实际: %v", tc.url, tc.want, err)
			}
		})
	}
}

// 注入的清单只能「进一步收紧」，绝不能把公网地址变成合法端点（「公网地址一律拒绝」）。
func TestEndpoint_AllowlistCannotEnablePublicIP(t *testing.T) {
	policy := NewEndpointPolicy(EndpointPolicyConfig{AllowedHosts: []string{"8.8.8.8", "https://1.1.1.1"}})

	for _, raw := range []string{"https://8.8.8.8/v1", "https://1.1.1.1/v1"} {
		if _, err := policy.ValidateEndpoint(raw); !errors.Is(err, ErrEndpointPublic) {
			t.Fatalf("即便写进清单，公网 IP %q 也必须被拒，实际: %v", raw, err)
		}
	}
}

// 运行期二次校验：清单内域名解析到公网地址 ⇒ 拒绝（防 DNS 重绑定 / 配置漂移）。
func TestEndpoint_RuntimeRejectsPublicResolvedIP(t *testing.T) {
	resolver := &fakeResolver{ips: []net.IP{net.ParseIP("10.0.0.5"), net.ParseIP("8.8.8.8")}}
	policy := NewEndpointPolicy(EndpointPolicyConfig{
		AllowedDomainSuffixes: []string{"internal.internal"},
		IPResolver:            resolver,
	})

	err := policy.ValidateRequest(context.Background(), mustRequest(t, "https://llm.internal.internal/v1/chat/completions"))
	if !errors.Is(err, ErrEndpointPublic) {
		t.Fatalf("解析结果含公网地址必须拒绝（ErrEndpointPublic），实际: %v", err)
	}
	if resolver.call != 1 || resolver.host != "llm.internal.internal" {
		t.Fatalf("应对实际主机做一次解析：calls=%d host=%q", resolver.call, resolver.host)
	}
}

func TestEndpoint_RuntimeAcceptsPrivateResolvedIP(t *testing.T) {
	resolver := &fakeResolver{ips: []net.IP{net.ParseIP("192.168.3.4"), net.ParseIP("fd00::2")}}
	policy := NewEndpointPolicy(EndpointPolicyConfig{
		AllowedDomainSuffixes: []string{"internal.internal"},
		IPResolver:            resolver,
	})

	if err := policy.ValidateRequest(context.Background(), mustRequest(t, "https://llm.internal.internal/v1")); err != nil {
		t.Fatalf("全部解析结果落在私网段应放行，实际被拒: %v", err)
	}
}

func TestEndpoint_RuntimeRejectsUnresolvable(t *testing.T) {
	t.Run("解析报错", func(t *testing.T) {
		policy := NewEndpointPolicy(EndpointPolicyConfig{
			AllowedDomainSuffixes: []string{"internal.internal"},
			IPResolver:            &fakeResolver{err: errors.New("no such host")},
		})
		err := policy.ValidateRequest(context.Background(), mustRequest(t, "https://llm.internal.internal/v1"))
		if !errors.Is(err, ErrEndpointUnresolvable) {
			t.Fatalf("解析失败必须 fail-closed 拒绝（ErrEndpointUnresolvable），实际: %v", err)
		}
	})
	t.Run("解析结果为空", func(t *testing.T) {
		policy := NewEndpointPolicy(EndpointPolicyConfig{
			AllowedDomainSuffixes: []string{"internal.internal"},
			IPResolver:            &fakeResolver{},
		})
		err := policy.ValidateRequest(context.Background(), mustRequest(t, "https://llm.internal.internal/v1"))
		if !errors.Is(err, ErrEndpointUnresolvable) {
			t.Fatalf("空解析结果必须拒绝（ErrEndpointUnresolvable），实际: %v", err)
		}
	})
}

// 运行期同样先过白名单：清单外主机连 DNS 都不查（fail-closed，且不产生对外解析行为）。
func TestEndpoint_RuntimeRejectsHostOutsideAllowlist(t *testing.T) {
	resolver := &fakeResolver{ips: []net.IP{net.ParseIP("10.0.0.5")}}
	policy := NewEndpointPolicy(EndpointPolicyConfig{
		AllowedDomainSuffixes: []string{"internal.internal"},
		IPResolver:            resolver,
	})

	err := policy.ValidateRequest(context.Background(), mustRequest(t, "https://api.openai.com/v1"))
	if !errors.Is(err, ErrEndpointNotAllowlisted) {
		t.Fatalf("清单外主机必须拒绝（ErrEndpointNotAllowlisted），实际: %v", err)
	}
	if resolver.call != 0 {
		t.Fatalf("清单外主机不应触发解析，实际解析 %d 次", resolver.call)
	}
}

// IP 字面量无需 DNS 即可判定；公网字面量在运行期同样被拒。
func TestEndpoint_RuntimeRejectsPublicIPLiteralWithoutDNS(t *testing.T) {
	resolver := &fakeResolver{}
	policy := NewEndpointPolicy(EndpointPolicyConfig{IPResolver: resolver})

	err := policy.ValidateRequest(context.Background(), mustRequest(t, "http://8.8.8.8/v1"))
	if !errors.Is(err, ErrEndpointPublic) {
		t.Fatalf("公网 IP 字面量必须拒绝（ErrEndpointPublic），实际: %v", err)
	}
	if resolver.call != 0 {
		t.Fatalf("IP 字面量不应触发解析，实际解析 %d 次", resolver.call)
	}

	if err := policy.ValidateRequest(context.Background(), mustRequest(t, "http://10.0.0.9/v1")); err != nil {
		t.Fatalf("私网 IP 字面量应放行，实际: %v", err)
	}
}

// 「同一策略供加载期与运行期复用」：同一份判定对同一输入给出一致结论（同一哨兵错误）。
func TestEndpoint_SamePolicyForLoadAndRuntime(t *testing.T) {
	policy := NewEndpointPolicy(EndpointPolicyConfig{
		AllowedDomainSuffixes: []string{"internal.internal"},
		IPResolver:            &fakeResolver{ips: []net.IP{net.ParseIP("10.0.0.5")}},
	})

	if _, err := policy.ValidateEndpoint("http://10.1.1.1:8080/v1"); err != nil {
		t.Fatalf("加载期应放行私网端点，实际: %v", err)
	}
	if err := policy.ValidateRequest(context.Background(), mustRequest(t, "http://10.1.1.1:8080/v1")); err != nil {
		t.Fatalf("运行期应放行同一私网端点，实际: %v", err)
	}

	_, loadErr := policy.ValidateEndpoint("https://api.openai.com/v1")
	runErr := policy.ValidateRequest(context.Background(), mustRequest(t, "https://api.openai.com/v1"))
	if loadErr == nil || runErr == nil {
		t.Fatalf("加载期与运行期都必须拒绝公网端点：loadErr=%v runErr=%v", loadErr, runErr)
	}
	if loadErr.Error() != runErr.Error() {
		t.Fatalf("同一策略两期结论应一致：load=%q run=%q", loadErr, runErr)
	}
}

// 可识别性：错误文本带稳定标识（供 LOG:llm.endpoint.rejected 的 reason 落点），且 errors.Is 可编程判定。
func TestEndpoint_RejectionsAreIdentifiable(t *testing.T) {
	policy := internalPolicy()
	cases := []struct {
		url   string
		token string
		want  error
	}{
		{"https://api.openai.com/v1", "endpoint.not_allowlisted", ErrEndpointNotAllowlisted},
		{"https://8.8.8.8/v1", "endpoint.public", ErrEndpointPublic},
		{"", "endpoint.empty", ErrEndpointEmpty},
		{"ftp://10.0.0.1/v1", "endpoint.scheme", ErrEndpointScheme},
		{"http:///v1", "endpoint.malformed", ErrEndpointMalformed},
	}
	for _, tc := range cases {
		_, err := policy.ValidateEndpoint(tc.url)
		if err == nil {
			t.Fatalf("%q 应被拒", tc.url)
		}
		if !errors.Is(err, tc.want) {
			t.Fatalf("%q 的哨兵错误不符: %v", tc.url, err)
		}
		if !strings.HasPrefix(err.Error(), tc.token) {
			t.Fatalf("%q 的错误文本须以稳定标识 %q 开头（供日志 reason），实际 %q", tc.url, tc.token, err)
		}
	}
}

// / NFR-S2：禁跟随重定向出网——任何重定向一律拒绝（fail-closed，含指向内网的跳转）。
func TestEndpoint_CheckRedirectRejectsAll(t *testing.T) {
	policy := internalPolicy()
	target := mustRequest(t, "https://llm.internal.internal/v2")
	err := policy.CheckRedirect(target, []*http.Request{mustRequest(t, "https://llm.internal.internal/v1")})
	if !errors.Is(err, ErrRedirectNotAllowed) {
		t.Fatalf("重定向必须被拒（ErrRedirectNotAllowed），实际: %v", err)
	}
	if !strings.HasPrefix(err.Error(), "redirect.not_allowed") {
		t.Fatalf("重定向拒绝错误须带稳定标识，实际 %q", err)
	}

	// 集成验证：真实 http.Client 挂上该策略后，302 目标一次都不会被访问。
	var hits int
	dst := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits++ }))
	defer dst.Close()
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, dst.URL, http.StatusFound)
	}))
	defer src.Close()

	client := &http.Client{CheckRedirect: policy.CheckRedirect}
	resp, err := client.Get(src.URL)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err == nil {
		t.Fatal("客户端不应跟随重定向成功")
	}
	if !errors.Is(err, ErrRedirectNotAllowed) {
		t.Fatalf("客户端错误须可识别为 ErrRedirectNotAllowed，实际: %v", err)
	}
	if hits != 0 {
		t.Fatalf("重定向目标被访问 %d 次，应为 0（禁跟随出网）", hits)
	}
}

// 默认 deny：未注入任何清单时，只有私网字面量可出域，域名一律拒绝。
// 这正是 「默认拒公网」在零配置下的形态（无配置 ⇒ 无出域）。
func TestGuard_EndpointPolicyDefaultDeny(t *testing.T) {
	policy := NewEndpointPolicy(EndpointPolicyConfig{})

	for _, raw := range []string{
		"https://api.openai.com/v1",
		"https://llm.internal.internal/v1",
		"https://any.internal.example/v1",
	} {
		if _, err := policy.ValidateEndpoint(raw); !errors.Is(err, ErrEndpointNotAllowlisted) {
			t.Fatalf("零配置策略必须拒绝域名 %q，实际: %v", raw, err)
		}
	}
	if _, err := policy.ValidateEndpoint("http://10.0.0.1/v1"); err != nil {
		t.Fatalf("零配置策略仍应放行 RFC1918 字面量，实际: %v", err)
	}
}

//	的框架侧前提：端点策略零 宿主依赖。
//
// 只扫描本任务产出的 endpoint.go，避免约束同包其它任务新增的文件。
func TestGuard_EndpointPolicyStdlibOnlyImports(t *testing.T) {
	const file = "endpoint.go"
	allowed := map[string]bool{
		"context":  true,
		"errors":   true,
		"fmt":      true,
		"net":      true,
		"net/http": true,
		"net/url":  true,
		"strings":  true,
	}

	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, file, nil, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	for _, spec := range parsed.Imports {
		path := strings.Trim(spec.Path.Value, `"`)
		// 白名单是唯一判据：它只列 stdlib 与本包兄弟包 ⇒ 任何落在宿主 module 其它子树里的
		// import 都会被这一条拦下（不再单列"禁宿主 module 名"，那既冗余又把宿主标识写进测试文件）。
		if !allowed[path] {
			t.Fatalf("%s 引入了非 stdlib 白名单依赖 %q（契约点: 本包零依赖）", file, path)
		}
	}
}
