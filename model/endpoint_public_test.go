package model

import (
	"context"
	"errors"
	"net"
	"net/http"
	"testing"
)

// 显式批准公网端点（`AllowPublicHosts`）的四条边界：
// ① 默认（未配置）⇒ 公网域名仍被拒（fail-closed 口径不变）；
// ② 列入清单 ⇒ 加载期放行；
// ③ 运行期仍要求可解析，但**不**再要求落在私网段；
// ④ 公网 IP 字面量**永远**不被本清单放行。

func TestEndpoint_PublicHostRejectedByDefault(t *testing.T) {
	// 未配置 AllowPublicHosts ⇒ 域名即便在内网清单里，运行期解析到公网 IP 仍被拒。
	resolver := &fakeResolver{ips: []net.IP{net.ParseIP("203.0.113.10")}}
	policy := NewEndpointPolicy(EndpointPolicyConfig{
		AllowedHosts: []string{"api.example.com"},
		IPResolver:   resolver,
	})

	req, err := http.NewRequest(http.MethodPost, "https://api.example.com/v1/chat/completions", nil)
	if err != nil {
		t.Fatalf("构造请求失败：%v", err)
	}
	if err := policy.ValidateRequest(context.Background(), req); !errors.Is(err, ErrEndpointPublic) {
		t.Fatalf("未配置 AllowPublicHosts ⇒ 解析到公网 IP 必须被拒，实际 err=%v", err)
	}
}

func TestEndpoint_ApprovedPublicHostPassesLoad(t *testing.T) {
	policy := NewEndpointPolicy(EndpointPolicyConfig{
		AllowPublicHosts: []string{"api.example.com"},
	})

	endpoint, err := policy.ValidateEndpoint("https://api.example.com/v1/chat/completions")
	if err != nil {
		t.Fatalf("列入 AllowPublicHosts 的主机应通过加载期校验：%v", err)
	}
	if endpoint != "https://api.example.com/v1/chat/completions" {
		t.Fatalf("端点应原样返回，实际=%q", endpoint)
	}
}

func TestEndpoint_ApprovedPublicHostNeedsResolvableOnly(t *testing.T) {
	resolver := &fakeResolver{ips: []net.IP{net.ParseIP("203.0.113.10")}}
	policy := NewEndpointPolicy(EndpointPolicyConfig{
		AllowPublicHosts: []string{"api.example.com"},
		IPResolver:       resolver,
	})

	req, err := http.NewRequest(http.MethodPost, "https://api.example.com/v1/chat/completions", nil)
	if err != nil {
		t.Fatalf("构造请求失败：%v", err)
	}
	if err := policy.ValidateRequest(context.Background(), req); err != nil {
		t.Fatalf("已批准公网主机解析到公网 IP 时应放行（不再要求私网段）：%v", err)
	}
}

func TestEndpoint_ApprovedPublicHostStillRejectedWhenUnresolvable(t *testing.T) {
	resolver := &fakeResolver{err: errors.New("dns down")}
	policy := NewEndpointPolicy(EndpointPolicyConfig{
		AllowPublicHosts: []string{"api.example.com"},
		IPResolver:       resolver,
	})

	req, err := http.NewRequest(http.MethodPost, "https://api.example.com/v1/chat/completions", nil)
	if err != nil {
		t.Fatalf("构造请求失败：%v", err)
	}
	if err := policy.ValidateRequest(context.Background(), req); !errors.Is(err, ErrEndpointUnresolvable) {
		t.Fatalf("已批准公网主机仍须可解析，实际 err=%v", err)
	}
}

func TestEndpoint_AllowPublicHostsCannotEnablePublicIPLiteral(t *testing.T) {
	policy := NewEndpointPolicy(EndpointPolicyConfig{
		AllowPublicHosts: []string{"203.0.113.10"},
	})

	if _, err := policy.ValidateEndpoint("https://203.0.113.10/v1/chat/completions"); !errors.Is(err, ErrEndpointPublic) {
		t.Fatalf("公网 IP 字面量不得被 AllowPublicHosts 放行，实际 err=%v", err)
	}
}
