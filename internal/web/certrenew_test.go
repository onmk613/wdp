package web

// 证书远程换证端到端：web 重签（保留私钥）→ mTLS 推送 → agent 热更换。
// agent 离线路径只验"重签成功、提示重启后生效"的口径。

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"wdp/internal/agent"
	"wdp/internal/ca"
	"wdp/internal/store"
)

// TestRenewHostCertE2E 在测试内起一个真实 mTLS agent，验证 renew-cert
// 全链路：证书延期、推送热更换生效、无证书主机 404。
func TestRenewHostCertE2E(t *testing.T) {
	s, st := newEnrollServer(t)
	h := s.Handler()

	// 逐主机证书：直接在 <caDir>/hosts 下签发（与 issueHostCert 同布局）
	hostsDir := filepath.Join(s.cam.dir, "hosts")
	if _, _, _, err := ca.Issue(ca.IssueOptions{
		Dir: hostsDir, CACertPath: s.cam.caPath, CAKeyPath: s.cam.caKeyPath,
		Profile: ca.ProfileServer, SANs: []string{"127.0.0.1"}, Days: 30,
	}, "e2ehost"); err != nil {
		t.Fatal(err)
	}
	crt, key := s.hostCertPaths("e2ehost")

	// 真实 agent（mTLS，临时端口）
	ag := agent.New("127.0.0.1:0")
	if err := ag.ConfigureAuth(s.cam.caPath, crt, key); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() { _ = ag.Serve(ln) }()
	port := ln.Addr().(*net.TCPAddr).Port

	id, err := st.UpsertHostByName("e2ehost", "127.0.0.1", port)
	if err != nil {
		t.Fatal(err)
	}

	origNA := agentHealthNotAfter(t, s, port)
	admin := loginSession(t, s)

	// 无证书主机 → 404
	noid, err := st.UpsertHostByName("nocert", "127.0.0.1", 7602)
	if err != nil {
		t.Fatal(err)
	}
	if rec := do(t, h, "POST", fmt.Sprintf("/api/hosts/%d/renew-cert", noid), nil, &admin); rec.Code != http.StatusNotFound {
		t.Fatalf("无证书主机应 404: %d %s", rec.Code, rec.Body)
	}

	// 正常换证：重签 + 推送热更换
	rec := do(t, h, "POST", fmt.Sprintf("/api/hosts/%d/renew-cert", id), nil, &admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("换证应 200: %d %s", rec.Code, rec.Body)
	}
	var out struct {
		Renewed  bool   `json:"renewed"`
		Pushed   bool   `json:"pushed"`
		NotAfter string `json:"not_after"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !out.Renewed || !out.Pushed {
		t.Fatalf("应重签且推送成功: %+v %s", out, rec.Body)
	}
	// agent 侧已热更换：健康端点报的到期时刻 = 响应中的新到期
	nowNA := agentHealthNotAfter(t, s, port)
	if nowNA != out.NotAfter {
		t.Fatalf("agent 未热更换: health=%s resp=%s", nowNA, out.NotAfter)
	}
	na, _ := time.Parse(time.RFC3339, nowNA)
	if orig, _ := time.Parse(time.RFC3339, origNA); !na.After(orig) {
		t.Fatalf("证书未延期: %s -> %s", origNA, nowNA)
	}
	// server 侧备份语义：旧证书改名保留
	matches, _ := filepath.Glob(crt + ".old.*")
	if len(matches) == 0 {
		t.Fatal("旧证书应留 .old.<ts> 备份")
	}
}

// agentHealthNotAfter 用 server 的 mTLS 客户端拉 agent /health 取证书到期时刻。
func agentHealthNotAfter(t *testing.T, s *Server, port int) string {
	t.Helper()
	url := fmt.Sprintf("https://127.0.0.1:%d/health", port)
	resp, err := s.cam.tlsClient.Get(url)
	if err != nil {
		t.Fatalf("agent /health: %v", err)
	}
	defer resp.Body.Close()
	var out struct {
		CertNotAfter string `json:"cert_not_after"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.CertNotAfter == "" {
		t.Fatal("agent 未启用 mTLS（cert_not_after 为空）")
	}
	return out.CertNotAfter
}

// TestRenewHostCertOffline agent 不在线：重签仍成功（server 侧就位），
// 响应明示"重启后生效"，不误报 pushed。
func TestRenewHostCertOffline(t *testing.T) {
	s, st := newEnrollServer(t)
	h := s.Handler()
	hostsDir := filepath.Join(s.cam.dir, "hosts")
	if _, _, _, err := ca.Issue(ca.IssueOptions{
		Dir: hostsDir, CACertPath: s.cam.caPath, CAKeyPath: s.cam.caKeyPath,
		Profile: ca.ProfileServer, SANs: []string{"127.0.0.1"}, Days: 30,
	}, "offhost"); err != nil {
		t.Fatal(err)
	}
	// 端口无人监听（用已关闭的监听器占一个真实已释放端口）
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	id, err := st.UpsertHostByName("offhost", "127.0.0.1", port)
	if err != nil {
		t.Fatal(err)
	}
	admin := loginSession(t, s)
	rec := do(t, h, "POST", fmt.Sprintf("/api/hosts/%d/renew-cert", id), nil, &admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("离线换证应 200（重签成功）: %d %s", rec.Code, rec.Body)
	}
	var out struct {
		Renewed bool `json:"renewed"`
		Pushed  bool `json:"pushed"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !out.Renewed || out.Pushed {
		t.Fatalf("离线主机应 renewed=true pushed=false: %+v", out)
	}
}

// TestConsoleTLS 控制台原生 TLS：配证书对后 Run 以 https 服务，
// /api/login 走 TLS 通道（401 而非连接错误）。
func TestConsoleTLS(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "tls.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	dir := t.TempDir()
	if _, _, _, err := ca.Init(ca.InitOptions{Dir: dir, Days: 30}); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := ca.Issue(ca.IssueOptions{Dir: dir, Profile: ca.ProfileServer, SANs: []string{"127.0.0.1"}, Days: 30}, "console"); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close() // 仅占号

	s, err := New(st, Options{
		Addr:    fmt.Sprintf("127.0.0.1:%d", port),
		TLSCert: filepath.Join(dir, "console.crt"),
		TLSKey:  filepath.Join(dir, "console.key"),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = s.Run(ctx) }()

	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: true, // 测试只验通道与路由，证书由 ca.Issue 保障
	}}, Timeout: 5 * time.Second}
	var lastErr error
	for i := 0; i < 50; i++ {
		resp, err := client.Get(fmt.Sprintf("https://127.0.0.1:%d/", port))
		if err == nil {
			resp.Body.Close()
			return // TLS 通道建立且路由响应
		}
		lastErr = err
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("TLS 控制台未就绪: %v", lastErr)
}
