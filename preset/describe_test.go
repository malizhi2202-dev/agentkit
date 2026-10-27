package preset

import (
	"net/http"
	"testing"

	"github.com/malizhi2202-dev/agentkit/audit"
	"github.com/malizhi2202-dev/agentkit/web"
)

// Describe 是管理面内省的唯一出口：nil 安全 + 关键字段存在，缺一项管理面就会瞎。
func TestAuditDescribe(t *testing.T) {
	var nilAudit *Audit
	info := nilAudit.Describe()
	if info.Name != NameAudit {
		t.Fatalf("nil Audit.Describe().Name=%q，期望 %q", info.Name, NameAudit)
	}
	if info.Notes["sinks"] == "" || info.Notes["ready"] == "" {
		t.Fatalf("nil Audit.Describe() 缺少 sinks/ready 注解：%+v", info.Notes)
	}

	fan, err := audit.New(audit.BlacklistMasker(), nil, audit.Nop())
	if err != nil {
		t.Fatalf("audit.New：%v", err)
	}
	a, err := NewAudit(fan)
	if err != nil {
		t.Fatalf("NewAudit：%v", err)
	}
	if got := a.Describe(); got.Notes["sinks"] != "1" {
		t.Fatalf("Describe().Notes[sinks]=%q，期望 1", got.Notes["sinks"])
	}
}

func TestServerDescribe(t *testing.T) {
	var nilServer *Server
	if got := nilServer.Describe(); got.Notes["addr"] != "" {
		t.Fatalf("nil Server.Describe().Notes[addr]=%q，期望空", got.Notes["addr"])
	}

	rt := web.NewRouter(web.Config{})
	if err := rt.Handle("GET", "/ping", func(w http.ResponseWriter, _ *http.Request) error {
		web.WriteJSON(w, "pong")
		return nil
	}); err != nil {
		t.Fatalf("Handle：%v", err)
	}
	srv, err := web.NewServer(web.ServerConfig{Router: rt, Addr: "127.0.0.1:0"})
	if err != nil {
		t.Fatalf("NewServer：%v", err)
	}
	sp := NewServer(srv)
	info := sp.Describe()
	if info.Name != NameServer {
		t.Fatalf("Describe().Name=%q，期望 %q", info.Name, NameServer)
	}
	if info.Notes["addr"] != "127.0.0.1:0" {
		t.Fatalf("Describe().Notes[addr]=%q", info.Notes["addr"])
	}
}
