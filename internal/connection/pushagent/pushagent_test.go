package pushagent

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"wdp/internal/agent"
	"wdp/internal/ca"
	"wdp/internal/connection"
	"wdp/internal/connection/agentconn"
	"wdp/internal/model"
)

// TestPushMTLSDirectConnect 验证 push 自举产物的 mTLS 直连闭环：
// 临时 CA + 共享证书对（服务端文件加载、客户端内联 PEM）+ 只验链不验名。
// 服务端以 push 实际参数组合（--ca/--cert/--key、无 token）装配。
func TestPushMTLSDirectConnect(t *testing.T) {
	certs, err := ca.IssueEphemeral()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	// 模拟 SSH 上传：服务端三件套落盘（agent 端 ConfigureAuth 从文件加载）
	caPath := writeCert(t, dir, "ca.crt", certs.CACertPEM)
	srvCrt := writeCert(t, dir, "srv.crt", certs.ServerCertPEM)
	srvKey := writeCert(t, dir, "srv.key", certs.ServerKeyPEM)

	s := agent.New(":0")
	if err := s.ConfigureAuth(caPath, srvCrt, srvKey); err != nil {
		t.Fatal(err)
	}
	serverCert, err := tls.X509KeyPair(certs.ServerCertPEM, certs.ServerKeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(certs.CACertPEM)
	ts := httptest.NewUnstartedServer(s.Handler())
	ts.TLS = &tls.Config{
		Certificates: []tls.Certificate{serverCert},
		ClientCAs:    pool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		MinVersion:   tls.VersionTLS12,
	}
	ts.StartTLS()
	t.Cleanup(ts.Close)

	// 与 pushagent.agentHost 相同的连接配置：https + 内联 PEM + 只验链
	h := &model.Host{
		Name: "p", Conn: "agent", AgentURL: ts.URL,
		CAData: certs.CACertPEM, CertData: certs.ClientCertPEM, KeyData: certs.ClientKeyPEM,
		TLSSkipHostVerify: true,
	}
	conn := agentconn.New(h, nil)
	if err := conn.Connect(context.Background()); err != nil {
		t.Fatalf("健康检查应通过: %v", err)
	}
	out, err := conn.Exec(context.Background(), connection.ExecRequest{Script: "echo push-mtls-ok"})
	if err != nil || !strings.Contains(out.Stdout, "push-mtls-ok") {
		t.Fatalf("mTLS 直连执行失败: %+v err=%v", out, err)
	}

	// 链校验仍生效：换成另一个会话的 CA 应被拒
	other, err := ca.IssueEphemeral()
	if err != nil {
		t.Fatal(err)
	}
	h2 := *h
	h2.CAData = other.CACertPEM
	if err := agentconn.New(&h2, nil).Connect(context.Background()); err == nil {
		t.Fatal("错误 CA 的链校验应拒绝")
	}
}

// TestAgentHostIPv6URL 验证 agentHost 的 URL 拼装对 IPv6 字面量加方括号。
func TestAgentHostIPv6URL(t *testing.T) {
	certs, err := ca.IssueEphemeral()
	if err != nil {
		t.Fatal(err)
	}
	c := New(&model.Host{Name: "x", Address: "fd00::5"}, nil)
	if got := c.agentHost(7602, certs).AgentURL; got != "https://[fd00::5]:7602" {
		t.Fatalf("IPv6 URL 应带方括号: %q", got)
	}
	c4 := New(&model.Host{Name: "y", Address: "10.0.0.5"}, nil)
	if got := c4.agentHost(7602, certs).AgentURL; got != "https://10.0.0.5:7602" {
		t.Fatalf("IPv4 URL 拼装错误: %q", got)
	}
}

// TestIPv6LoopbackDirectConnect IPv6 端到端：[::1] 上起 mTLS agent，
// 经 Address=::1（走 URL 拼装方括号路径）完成健康检查与执行。
// 环境无 IPv6 回环时跳过。
func TestIPv6LoopbackDirectConnect(t *testing.T) {
	l, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Skipf("环境不支持 IPv6 回环监听: %v", err)
	}

	certs, err := ca.IssueEphemeral()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	caPath := writeCert(t, dir, "ca.crt", certs.CACertPEM)
	srvCrt := writeCert(t, dir, "srv.crt", certs.ServerCertPEM)
	srvKey := writeCert(t, dir, "srv.key", certs.ServerKeyPEM)
	s := agent.New(":0")
	if err := s.ConfigureAuth(caPath, srvCrt, srvKey); err != nil {
		t.Fatal(err)
	}
	serverCert, err := tls.X509KeyPair(certs.ServerCertPEM, certs.ServerKeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(certs.CACertPEM)
	ts := httptest.NewUnstartedServer(s.Handler())
	ts.Listener = l
	ts.TLS = &tls.Config{
		Certificates: []tls.Certificate{serverCert},
		ClientCAs:    pool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		MinVersion:   tls.VersionTLS12,
	}
	ts.StartTLS()
	t.Cleanup(ts.Close)

	port := l.Addr().(*net.TCPAddr).Port
	h := &model.Host{
		Name: "v6", Conn: "agent", Address: "::1", AgentPort: port,
		CAData: certs.CACertPEM, CertData: certs.ClientCertPEM, KeyData: certs.ClientKeyPEM,
		TLSSkipHostVerify: true,
	}
	conn := agentconn.New(h, nil)
	if err := conn.Connect(context.Background()); err != nil {
		t.Fatalf("IPv6 健康检查应通过: %v", err)
	}
	out, err := conn.Exec(context.Background(), connection.ExecRequest{Script: "echo v6-ok"})
	if err != nil || !strings.Contains(out.Stdout, "v6-ok") {
		t.Fatalf("IPv6 mTLS 执行失败: %+v err=%v", out, err)
	}
}

func writeCert(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// resetCertStore 重置进程级证书仓库（测试隔离）。
func resetCertStore(t *testing.T) {
	t.Helper()
	certStore.Lock()
	certStore.certs, certStore.gen, certStore.at = nil, 0, time.Time{}
	certStore.Unlock()
}

// rotDC 返回启用 30 分钟轮换的显式默认值（DI 后不再改全局配置）。
func rotDC() *connection.Defaults {
	return &connection.Defaults{AgentCertRotateMin: 30}
}

// TestCertRotationStore 验证代数仓库：默认不轮换；到期换新代且新旧信任链独立。
func TestCertRotationStore(t *testing.T) {
	resetCertStore(t)

	// 默认（不轮换）：多次取值同代
	c1, g1, err := certMaterial(nil)
	if err != nil || g1 != 1 {
		t.Fatalf("首代应为 gen=1: gen=%d err=%v", g1, err)
	}
	if _, g2, _ := certMaterial(nil); g2 != 1 {
		t.Fatalf("未配置轮换不应换代: gen=%d", g2)
	}

	// 配置 30 分钟，回拨时钟 31 分钟 → 换新代，信任链独立
	certStore.Lock()
	certStore.at = time.Now().Add(-31 * time.Minute)
	certStore.Unlock()
	c3, g3, err := certMaterial(rotDC())
	if err != nil || g3 != 2 {
		t.Fatalf("到期应换为 gen=2: gen=%d err=%v", g3, err)
	}
	pool1 := x509.NewCertPool()
	pool1.AppendCertsFromPEM(c1.CACertPEM)
	block, _ := pem.Decode(c3.ServerCertPEM)
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool1, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err == nil {
		t.Fatal("新旧代信任链应相互独立")
	}

	// 未到期 → 不换
	if _, g4, _ := certMaterial(nil); g4 != 2 {
		t.Fatalf("未到期不应换代: gen=%d", g4)
	}
}

// TestRotateIfDueSameGenNoop 当前代连接的轮换检查应直接放行（不触发 SSH）。
func TestRotateIfDueSameGenNoop(t *testing.T) {
	resetCertStore(t)
	_, gen, err := certMaterial(rotDC())
	if err != nil {
		t.Fatal(err)
	}
	c := &Conn{host: &model.Host{Name: "x"}, gen: gen, dc: rotDC()}
	if err := c.rotateIfDue(context.Background()); err != nil {
		t.Fatalf("同代应放行: %v", err)
	}
}

// TestRotateIfDueStaleGenTriggersMigration 落后代连接的轮换检查应触发迁移；
// SSH 重连失败时错误显式上抛（连接保留旧代记录，下次任务自愈重试）。
func TestRotateIfDueStaleGenTriggersMigration(t *testing.T) {
	resetCertStore(t)
	_, gen, err := certMaterial(rotDC())
	if err != nil {
		t.Fatal(err)
	}
	c := New(&model.Host{Name: "x", Address: "127.0.0.1", Port: 1}, rotDC())
	c.gen = gen - 1
	if err := c.rotateIfDue(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "证书轮换") {
		t.Fatalf("落后代应触发迁移并显式报错, got: %v", err)
	}
}

// TestNativeExtractFallbackMode push 自举失败回退 SSH 态（agent 未就绪）时
// 原生解压应返回不支持哨兵，模块回退 shell 路径。
func TestNativeExtractFallbackMode(t *testing.T) {
	c := New(&model.Host{Name: "x", Address: "127.0.0.1", Port: 1}, nil)
	if err := c.NativeExtract(context.Background(), "/a.tgz", "/opt/a"); !errors.Is(err, connection.ErrNativeUnsupported) {
		t.Fatalf("回退态应返回不支持: %v", err)
	}
}
