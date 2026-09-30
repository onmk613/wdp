package web

// agentClientCache 的分桶正确性：mTLS 客户端必须按证书校验名（台账地址）
// 分桶——此前只按形态分桶，首台主机的 ServerName 被固化进共享 Transport，
// 批量执行访问其余主机时证书校验全部 hostname mismatch。并发用例与
// ExecService.ExecOnHosts 的每主机 goroutine 形态一致，-race 下锁住回归。

import (
	"fmt"
	"net/http"
	"sync"
	"testing"

	"wdp/internal/store"
)

func clientServerName(t *testing.T, cl *http.Client) string {
	t.Helper()
	tr, ok := cl.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport 应为 *http.Transport: %T", cl.Transport)
	}
	if tr.TLSClientConfig == nil {
		return ""
	}
	return tr.TLSClientConfig.ServerName
}

func TestAgentClientCachePerHostServerName(t *testing.T) {
	s, _ := newEnrollServer(t)

	ca := s.agentClients.clientFor(s, &store.Host{Name: "a", Address: "10.0.0.1:7600"})
	cb := s.agentClients.clientFor(s, &store.Host{Name: "b", Address: "10.0.0.2:7600"})
	if ca == nil || cb == nil {
		t.Fatal("材料完好时 clientFor 不应返回 nil")
	}
	if ca == cb {
		t.Fatal("不同校验名的主机必须拿到不同客户端（否则证书校验错位）")
	}
	if got := clientServerName(t, ca); got != "10.0.0.1" {
		t.Fatalf("主机 a 的 ServerName 应为 10.0.0.1: %q", got)
	}
	if got := clientServerName(t, cb); got != "10.0.0.2" {
		t.Fatalf("主机 b 的 ServerName 应为 10.0.0.2: %q", got)
	}

	// 同校验名（不同端口/别名）复用同一实例——证书身份只看名字
	c2 := s.agentClients.clientFor(s, &store.Host{Name: "a2", Address: "10.0.0.1:7601"})
	if c2 != ca {
		t.Fatal("同校验名应复用同一客户端实例")
	}

	// 明文主机：共享 plain 客户端，不带 TLS 配置
	p1 := s.agentClients.clientFor(s, &store.Host{Name: "p", Address: "10.0.0.9:7600", AllowPlaintext: true})
	p2 := s.agentClients.clientFor(s, &store.Host{Name: "p2", Address: "10.0.0.10:7600", AllowPlaintext: true})
	if p1 == nil || p2 == nil || p1 != p2 {
		t.Fatal("明文主机应共享同一 plain 客户端")
	}
	if got := clientServerName(t, p1); got != "" {
		t.Fatalf("明文客户端不应携带 TLS ServerName: %q", got)
	}
}

func TestAgentClientCacheConcurrentAccess(t *testing.T) {
	s, _ := newEnrollServer(t)
	hosts := make([]*store.Host, 8)
	for i := range hosts {
		hosts[i] = &store.Host{Name: fmt.Sprintf("h%d", i), Address: fmt.Sprintf("10.0.0.%d:7600", i+1)}
	}
	var wg sync.WaitGroup
	for round := 0; round < 4; round++ {
		for _, h := range hosts {
			wg.Add(1)
			go func(h *store.Host) {
				defer wg.Done()
				cl := s.agentClients.clientFor(s, h)
				if cl == nil {
					t.Error("材料完好时 clientFor 不应返回 nil")
					return
				}
				if again := s.agentClients.clientFor(s, h); again != cl {
					t.Errorf("主机 %s 应始终复用同一客户端实例", h.Name)
				}
			}(h)
		}
	}
	wg.Wait()
}
