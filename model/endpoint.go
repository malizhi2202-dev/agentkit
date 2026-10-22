package model

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// 端点白名单的哨兵错误（ / NFR-S2 / ）。
//
// 每个错误的文本都以**稳定标识**开头（`endpoint.public` 等），使调用方既能用 errors.Is 做程序判定，
// 也能把 err 直接作为 `LOG:llm.endpoint.rejected` 的 reason 落日志，而不必再自造一份原因枚举。
var (
	// ErrEndpointEmpty 端点为空。
	ErrEndpointEmpty = errors.New("endpoint.empty: 端点为空，按默认拒绝处理")
	// ErrEndpointMalformed 端点语法非法（非 http(s)://host 形态，或含 userinfo）。
	ErrEndpointMalformed = errors.New("endpoint.malformed: 端点格式非法（需为 http(s)://host，且不得含 userinfo）")
	// ErrEndpointScheme 协议不在允许集合内。
	ErrEndpointScheme = errors.New("endpoint.scheme: 仅允许 http / https 端点")
	// ErrEndpointNotAllowlisted 主机不在调用方注入的端点清单内。
	ErrEndpointNotAllowlisted = errors.New("endpoint.not_allowlisted: 主机不在调用方注入的端点清单内")
	// ErrEndpointPublic 端点落在公网地址（**默认拒绝**， 的核心判据）。
	ErrEndpointPublic = errors.New("endpoint.public: 端点落在公网地址，默认拒绝")
	// ErrEndpointUnresolvable 运行期解析失败或结果为空 ⇒ fail-closed 拒绝。
	ErrEndpointUnresolvable = errors.New("endpoint.unresolvable: 端点主机无法解析（fail-closed 拒绝）")
	// ErrRedirectNotAllowed 出域禁止跟随任何重定向。
	ErrRedirectNotAllowed = errors.New("redirect.not_allowed: 出域禁止跟随重定向")
)

// IPResolver 是运行期二次校验的**解析注入点**；stdlib `*net.Resolver` 天然满足该接口
// （`LookupIP(ctx, network, host) ([]net.IP, error)`），测试可注入 fake 以确定性复现
// 「解析到公网 / 解析失败 / 解析为空」三态，无需真实 DNS。
type IPResolver interface {
	LookupIP(ctx context.Context, network, host string) ([]net.IP, error)
}

// privateCIDRs 是除回环外额外放行的私网段（RFC1918 + IPv6 ULA）。
// 刻意**不**含链路本地（169.254/16、fe80::/10）、CGNAT（100.64/10）、组播与未指定地址：
// 这些都不是「内网私有化端点」的正当形态，按 fail-closed 一律拒绝（既有教训）。
var privateCIDRs = mustCIDRs("10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "fc00::/7")

func mustCIDRs(cidrs ...string) []*net.IPNet {
	out := make([]*net.IPNet, 0, len(cidrs))
	for _, c := range cidrs {
		_, block, err := net.ParseCIDR(c)
		if err != nil {
			// 内建常量，非法即为编码错误；此处 panic 而非忽略，避免静默放宽策略。
			panic("model: 内建私网段常量非法: " + c)
		}
		out = append(out, block)
	}
	return out
}

// isPrivateIP 判定地址是否落在允许出域的私网集合内（RFC1918 / 回环 / IPv6 ULA）。
func isPrivateIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if ip.IsLoopback() { // 127.0.0.0/8 与 ::1
		return true
	}
	for _, block := range privateCIDRs {
		if block.Contains(ip) {
			return true
		}
	}
	return false
}

// EndpointPolicyConfig 是**调用方注入**的策略输入。
//
// 框架内不读任何 宿主 配置（ / D1）：允许出域的主机与内网域名后缀
// 一律由调用方（宿主侧为 `既有模型层`，来源 `conf/llm/*.yml` 的 `llm.endpoint_allowlist`）
// 映射后传入。二者皆空 ⇒ 默认 deny（域名一律拒绝，仅 RFC1918/回环字面量可用）。
type EndpointPolicyConfig struct {
	// AllowedHosts 是允许出域的**精确主机名**。兼容裸主机名、"host:port" 与完整 URL 三种配置写法
	// （等价/冗余写法不应因形态差异而静默失配）；端口不参与策略判定。
	AllowedHosts []string
	// AllowedDomainSuffixes 是允许出域的**内网域名后缀**（如 "internal.internal" / ".svc.local"），
	// 按 label 边界匹配 —— "evilinternal.internal" 不匹配后缀 "internal.internal"。
	AllowedDomainSuffixes []string
	// AllowPublicHosts 是**显式批准走公网**的精确主机名（如公网模型 API 的域名）。
	//
	// 默认空 ⇒ 行为与没有本字段完全一致（出域只许内网/私网端点，fail-closed）。
	// 非空时列入的主机**免于私网判定**（加载期不再要求解析结果落在私网段），
	// 但**仍**要求：① DNS 可解析（解析失败即拒绝）；② 不跟随任何重定向；
	// ③ **公网 IP 字面量永远不被放行**（本清单不能把一个公网 IP 变成合法端点）。
	//
	// 授权是**逐主机**的：只有列出的那几台可出域，不含通配、不含后缀。
	AllowPublicHosts []string
	// IPResolver 可选：nil ⇒ `net.DefaultResolver`。
	IPResolver IPResolver
}

// EndpointPolicy 是端点白名单策略（**默认拒绝公网地址**）。
//
// 同一策略实例供**加载期**（ValidateEndpoint）与**运行期**（ValidateRequest）复用：两期共用
// `enforce` 这一个判定，运行期在其上追加「解析结果必须仍在私网段」的二次校验（防 DNS 重绑定 / 配置漂移）。
//
// 策略是**只读**的：构造后再无任何写操作，可被多 goroutine 并发使用。
type EndpointPolicy struct {
	allowedHosts    map[string]struct{}
	allowedSuffixes []string
	allowedPublic   map[string]struct{}
	resolver        IPResolver
}

// NewEndpointPolicy 从注入的清单构造策略。对清单项做规范化（小写、去尾点、去 scheme/端口/路径）。
func NewEndpointPolicy(cfg EndpointPolicyConfig) *EndpointPolicy {
	policy := &EndpointPolicy{
		allowedHosts:  make(map[string]struct{}, len(cfg.AllowedHosts)),
		allowedPublic: make(map[string]struct{}, len(cfg.AllowPublicHosts)),
		resolver:      cfg.IPResolver,
	}
	for _, entry := range cfg.AllowPublicHosts {
		if host := normalizeHostEntry(entry); host != "" {
			policy.allowedPublic[host] = struct{}{}
		}
	}
	for _, entry := range cfg.AllowedHosts {
		if host := normalizeHostEntry(entry); host != "" {
			policy.allowedHosts[host] = struct{}{}
		}
	}
	seen := make(map[string]struct{}, len(cfg.AllowedDomainSuffixes))
	for _, entry := range cfg.AllowedDomainSuffixes {
		suffix := normalizeSuffix(entry)
		if suffix == "" {
			continue
		}
		if _, dup := seen[suffix]; dup {
			continue
		}
		seen[suffix] = struct{}{}
		policy.allowedSuffixes = append(policy.allowedSuffixes, suffix)
	}
	return policy
}

// ValidateEndpoint 是**加载期**校验：语法 + 主机判定（IP 字面量按私网段，域名按注入清单），
// **不做 DNS**（加载期不应产生对外解析行为，且结论必须与网络状态无关）。
// 通过则返回规范化后的端点 URL，供调用方直接落配置。
func (p *EndpointPolicy) ValidateEndpoint(rawURL string) (string, error) {
	raw := strings.TrimSpace(rawURL)
	if raw == "" {
		return "", ErrEndpointEmpty
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("%w: %s: %v", ErrEndpointMalformed, raw, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("%w: %s", ErrEndpointScheme, raw)
	}
	if parsed.User != nil {
		// userinfo 可能把凭据带进日志与审计，且无正当用途 ⇒ 直接判非法。
		return "", fmt.Errorf("%w: %s", ErrEndpointMalformed, raw)
	}
	host := normalizeHost(parsed.Hostname())
	if host == "" {
		return "", fmt.Errorf("%w: %s", ErrEndpointMalformed, raw)
	}
	if _, err := p.enforce(host); err != nil {
		return "", err
	}
	return parsed.String(), nil
}

// ValidateRequest 是**运行期二次校验**：在真正发起出域请求之前，对请求 URL 重新执行同一策略，
// 并对域名主机校验**解析结果**全部落在私网段。任一步失败即不出域（fail-closed）。
func (p *EndpointPolicy) ValidateRequest(ctx context.Context, req *http.Request) error {
	if req == nil || req.URL == nil {
		return fmt.Errorf("%w: 请求或 URL 为 nil", ErrEndpointEmpty)
	}
	host := normalizeHost(req.URL.Hostname())
	if host == "" {
		return fmt.Errorf("%w: %s", ErrEndpointEmpty, req.URL.Redacted())
	}
	literalIP, err := p.enforce(host)
	if err != nil {
		return err
	}
	if literalIP != nil {
		// IP 字面量已由 enforce 判定完毕，无需也无从解析。
		return nil
	}
	if p.publicApproved(host) {
		// 显式批准的公网主机：仍要求 DNS 可解析（无法确认 ⇒ 不出域），
		// 但不再要求解析结果落在私网段 —— 那正是"公网端点"的定义。
		return p.checkResolvable(ctx, host)
	}
	return p.checkResolved(ctx, host)
}

// checkResolvable 只要求主机**可解析**（显式批准的公网端点用）：解析失败或结果为空即拒绝。
func (p *EndpointPolicy) checkResolvable(ctx context.Context, host string) error {
	resolver := p.resolver
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	ips, err := resolver.LookupIP(ctx, "ip", host)
	if err != nil {
		return fmt.Errorf("%w: %s: %v", ErrEndpointUnresolvable, host, err)
	}
	if len(ips) == 0 {
		return fmt.Errorf("%w: %s（解析结果为空）", ErrEndpointUnresolvable, host)
	}
	return nil
}

// checkResolved 解析主机并要求**全部**解析结果落在私网段。
// 解析失败或结果为空同样拒绝：无法确认 ⇒ 不出域（NFR-S2 / 既有教训）。
func (p *EndpointPolicy) checkResolved(ctx context.Context, host string) error {
	resolver := p.resolver
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	ips, err := resolver.LookupIP(ctx, "ip", host)
	if err != nil {
		return fmt.Errorf("%w: %s: %v", ErrEndpointUnresolvable, host, err)
	}
	if len(ips) == 0 {
		return fmt.Errorf("%w: %s（解析结果为空）", ErrEndpointUnresolvable, host)
	}
	for _, ip := range ips {
		if !isPrivateIP(ip) {
			return fmt.Errorf("%w: %s 解析到 %s", ErrEndpointPublic, host, ip)
		}
	}
	return nil
}

// CheckRedirect 供 `http.Client.CheckRedirect` **直接赋值**：出域**禁止跟随任何重定向**
// （ / NFR-S2）。
//
// 这里刻意不区分目标是否也在私网段：重定向等于「由对端指定新的出域目的地」，一旦跟随，
// 白名单与解析期校验都可被绕过一步；出域没有任何必须跟随重定向的正当场景 ⇒ 一律拒绝（fail-closed）。
func (p *EndpointPolicy) CheckRedirect(req *http.Request, via []*http.Request) error {
	target := ""
	if req != nil && req.URL != nil {
		target = req.URL.Host
	}
	return fmt.Errorf("%w: 拒绝跳转到 %q（已跟随 %d 次）", ErrRedirectNotAllowed, target, len(via))
}

// enforce 是加载期与运行期**共用**的第一道判定（只看主机字面量，不查 DNS）：
//   - IP 字面量：必须落在私网段，否则 ErrEndpointPublic（**即便被写进注入清单也拒绝**——
//     清单只能收紧，不能把公网变成合法）；
//   - 域名：必须命中注入的精确主机或内网域名后缀，否则 ErrEndpointNotAllowlisted。
//
// 返回非 nil 的 ip 表示「主机是 IP 字面量」，运行期据此跳过 DNS 解析。
func (p *EndpointPolicy) enforce(host string) (net.IP, error) {
	if ip := net.ParseIP(host); ip != nil {
		if !isPrivateIP(ip) {
			return nil, fmt.Errorf("%w: %s", ErrEndpointPublic, host)
		}
		return ip, nil
	}
	if p.publicApproved(host) {
		// 显式批准的公网主机：授权来自配置清单，私网判定不适用（"公网端点"即其定义）。
		return nil, nil
	}
	if !p.domainAllowlisted(host) {
		return nil, fmt.Errorf("%w: %s", ErrEndpointNotAllowlisted, host)
	}
	return nil, nil
}

// publicApproved 判定主机是否被**显式批准**走公网（`AllowPublicHosts`，逐主机授权）。
func (p *EndpointPolicy) publicApproved(host string) bool {
	_, ok := p.allowedPublic[host]
	return ok
}

// domainAllowlisted 判定域名是否命中注入清单（精确主机或 label 边界后缀）。
func (p *EndpointPolicy) domainAllowlisted(host string) bool {
	if _, ok := p.allowedHosts[host]; ok {
		return true
	}
	for _, suffix := range p.allowedSuffixes {
		if host == suffix || strings.HasSuffix(host, "."+suffix) {
			return true
		}
	}
	return false
}

// normalizeHost 统一主机形态：去空白、小写、去 FQDN 尾点。
func normalizeHost(host string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
}

// normalizeHostEntry 把清单项规范成纯主机名，兼容裸主机名 / "host:port" / 完整 URL 三种配置写法。
func normalizeHostEntry(entry string) string {
	raw := strings.TrimSpace(entry)
	if raw == "" {
		return ""
	}
	if strings.Contains(raw, "://") {
		parsed, err := url.Parse(raw)
		if err != nil {
			return ""
		}
		return normalizeHost(parsed.Hostname())
	}
	if idx := strings.Index(raw, "/"); idx >= 0 {
		raw = raw[:idx]
	}
	if host, _, err := net.SplitHostPort(raw); err == nil {
		raw = host
	}
	return normalizeHost(strings.Trim(raw, "[]"))
}

// normalizeSuffix 规范化内网域名后缀：去空白、小写、去前后导点。
func normalizeSuffix(entry string) string {
	suffix := strings.TrimSpace(entry)
	suffix = strings.TrimPrefix(suffix, ".")
	suffix = strings.TrimSuffix(suffix, ".")
	return strings.ToLower(suffix)
}
