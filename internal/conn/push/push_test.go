package push

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"wdp/internal/agent"
	"wdp/internal/conn"
	"wdp/internal/conn/agentc"
	"wdp/internal/model"
	"wdp/internal/pushcerts"
)

// TestPushMTLSDirectConnect 验证 push 自举产物的 mTLS 直连闭环：
// 临时 CA + 共享证书对（服务端文件加载、客户端内联 PEM）+ 只验链不验名。
// 服务端以 push 实际参数组合（--ca/--cert/--key、无 token）装配。
func TestPushMTLSDirectConnect(t *testing.T) {
	certs, err := pushcerts.LoadOrCreate(t.TempDir())
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

	// 与 push.agentHost 相同的连接配置：https + 内联 PEM + 只验链
	h := &model.Host{
		Name: "p", Conn: "agent", AgentURL: ts.URL,
		CAData: certs.CACertPEM, CertData: certs.ClientCertPEM, KeyData: certs.ClientKeyPEM,
		TLSSkipHostVerify: true,
	}
	cn := agentc.New(h, nil)
	if err := cn.Connect(context.Background()); err != nil {
		t.Fatalf("健康检查应通过: %v", err)
	}
	out, err := cn.Exec(context.Background(), conn.ExecRequest{Script: "echo push-mtls-ok"})
	if err != nil || !strings.Contains(out.Stdout, "push-mtls-ok") {
		t.Fatalf("mTLS 直连执行失败: %+v err=%v", out, err)
	}

	// 链校验仍生效：换成另一个会话的 CA 应被拒
	other, err := pushcerts.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h2 := *h
	h2.CAData = other.CACertPEM
	if err := agentc.New(&h2, nil).Connect(context.Background()); err == nil {
		t.Fatal("错误 CA 的链校验应拒绝")
	}
}

// TestAgentHostIPv6URL 验证 agentHost 的 URL 拼装对 IPv6 字面量加方括号。
func TestAgentHostIPv6URL(t *testing.T) {
	certs, err := pushcerts.LoadOrCreate(t.TempDir())
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

	certs, err := pushcerts.LoadOrCreate(t.TempDir())
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
	cn := agentc.New(h, nil)
	if err := cn.Connect(context.Background()); err != nil {
		t.Fatalf("IPv6 健康检查应通过: %v", err)
	}
	out, err := cn.Exec(context.Background(), conn.ExecRequest{Script: "echo v6-ok"})
	if err != nil || !strings.Contains(out.Stdout, "v6-ok") {
		t.Fatalf("IPv6 mTLS 执行失败: %+v err=%v", out, err)
	}
}

// writeCert 落盘证书材料，返回路径。
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
	pushcerts.Reset()
}

// testDC 返回测试专用默认值：显式临时会话目录 + 30 分钟轮换
// （绝不落到真实 ~/.wdp/push-ca）。代数仓库与会话 CA 落盘的域测试在
// internal/pushcerts。
func testDC(t *testing.T) *conn.Defaults {
	t.Helper()
	return &conn.Defaults{AgentCertRotateMin: 30, PushCADir: t.TempDir()}
}

// TestRotateIfDueSameGenNoop 当前代连接的轮换检查应直接放行（不触发 SSH）。
func TestRotateIfDueSameGenNoop(t *testing.T) {
	resetCertStore(t)
	dc := testDC(t)
	_, gen, err := pushcerts.Material(dc)
	if err != nil {
		t.Fatal(err)
	}
	c := &Conn{host: &model.Host{Name: "x"}, gen: gen, dc: dc}
	if err := c.rotateIfDue(context.Background()); err != nil {
		t.Fatalf("同代应放行: %v", err)
	}
}

// TestRotateIfDueStaleGenTriggersMigration 落后代连接的轮换检查应触发迁移；
// SSH 重连失败时错误显式上抛（连接保留旧代记录，下次任务自愈重试）。
func TestRotateIfDueStaleGenTriggersMigration(t *testing.T) {
	resetCertStore(t)
	dc := testDC(t)
	_, gen, err := pushcerts.Material(dc)
	if err != nil {
		t.Fatal(err)
	}
	c := New(&model.Host{Name: "x", Address: "127.0.0.1", Port: 1}, dc)
	c.gen = gen - 1
	if err := c.rotateIfDue(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "cert rotation SSH reconnect failed") {
		t.Fatalf("落后代应触发迁移并显式报错, got: %v", err)
	}
}

// TestNativeExtractFallbackMode push 自举失败回退 SSH 态（agent 未就绪）时
// 原生解压应返回不支持哨兵，模块回退 shell 路径。
func TestNativeExtractFallbackMode(t *testing.T) {
	c := New(&model.Host{Name: "x", Address: "127.0.0.1", Port: 1}, nil)
	if err := c.NativeExtract(context.Background(), "/a.tgz", "/opt/a"); !errors.Is(err, conn.ErrNativeUnsupported) {
		t.Fatalf("回退态应返回不支持: %v", err)
	}
}

// TestAgentStartScriptIdleFlag 自举启动脚本注入 --idle-timeout：
// 默认值与禁用（<=0 不注入）两种形态；路径带空格正确引用。
func TestAgentStartScriptIdleFlag(t *testing.T) {
	s := agentStartScript("/tmp/.wdp-agent-ab12", 7602, 60)
	if !strings.Contains(s, "--idle-timeout 60m") {
		t.Fatalf("应注入 60m 空闲超时: %s", s)
	}
	if !strings.Contains(s, "--cleanup-on-shutdown") {
		t.Fatalf("push 语义的自清理 flag 不应丢失: %s", s)
	}

	off := agentStartScript("/tmp/.wdp-agent-ab12", 7602, 0)
	if strings.Contains(off, "--idle-timeout") {
		t.Fatalf("禁用（0）不应注入 idle flag: %s", off)
	}

	spaced := agentStartScript("/tmp/my agent dir/.wdp-agent-x", 7700, 90)
	if !strings.Contains(spaced, `--idle-timeout 90m`) || !strings.Contains(spaced, `'/tmp/my agent dir/.wdp-agent-x'`) {
		t.Fatalf("带空格路径应正确引用: %s", spaced)
	}
}

// TestIsTransportError 传输层错误判定：*url.Error（不可达/断连）为真；
// HTTP 状态码错误与 nil 为假（agent 活着，不触发自愈）。
func TestIsTransportError(t *testing.T) {
	if isTransportError(nil) {
		t.Fatal("nil 不是传输错误")
	}
	if isTransportError(fmt.Errorf("agent exec HTTP 500: boom")) {
		t.Fatal("HTTP 状态错误不是传输错误")
	}
	ue := &url.Error{Op: "Post", URL: "https://10.0.0.1:7602/exec", Err: errors.New("connection refused")}
	if !isTransportError(ue) {
		t.Fatal("url.Error 应判传输错误")
	}
	if !isTransportError(fmt.Errorf("wrap: %w", ue)) {
		t.Fatal("包装的 url.Error 应判传输错误")
	}
}

// TestHealSkipsContextCancel 用户取消（ctx 结束）不触发自愈重自举——
// 那是运行生命周期在收尾，重连没有意义；也不应触碰连接状态。
func TestHealSkipsContextCancel(t *testing.T) {
	c := New(&model.Host{Name: "x", Address: "127.0.0.1", Port: 1}, nil)
	ue := &url.Error{Op: "Post", URL: "https://127.0.0.1:7602/exec", Err: context.Canceled}
	c.healIfUnreachable(context.Background(), fmt.Errorf("wrap: %w", ue))
	if c.started {
		t.Fatal("context 取消错误不应触发自愈")
	}
	ueDeadline := &url.Error{Op: "Post", URL: "https://127.0.0.1:7602/exec", Err: context.DeadlineExceeded}
	c.healIfUnreachable(context.Background(), fmt.Errorf("wrap: %w", ueDeadline))
	if c.started {
		t.Fatal("整体超时错误不应该触发自愈")
	}

	// 运行已取消（ctx.Err() != nil）：即使错误是真实传输层失败也跳过
	c2 := New(&model.Host{Name: "y", Address: "127.0.0.1", Port: 1}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ueNet := &url.Error{Op: "Post", URL: "https://127.0.0.1:7602/exec", Err: errors.New("connection refused")}
	c2.healIfUnreachable(ctx, ueNet)
	if c2.started {
		t.Fatal("运行已取消时不应触发自愈（收尾期不拖慢失败上报）")
	}
}

// TestIsTransportErrorUnexpectedEOF 响应体中途截断（unexpected EOF）按
// 传输层错误处理，触发自愈。
func TestIsTransportErrorUnexpectedEOF(t *testing.T) {
	if !isTransportError(io.ErrUnexpectedEOF) {
		t.Fatal("unexpected EOF 应判传输错误")
	}
	if !isTransportError(fmt.Errorf("decode: %w", io.ErrUnexpectedEOF)) {
		t.Fatal("包装的 unexpected EOF 应判传输错误")
	}
	if isTransportError(io.EOF) {
		t.Fatal("纯 EOF 不判传输错误（语义不同）")
	}
}

// TestPlatformFromUname uname -sm 输出归一为 os_arch 平台键。
func TestPlatformFromUname(t *testing.T) {
	cases := map[string]string{
		"Linux x86_64\n": "linux_amd64",
		"Linux aarch64":  "linux_arm64",
		"Linux arm64\n":  "linux_arm64",
		"Darwin arm64\n": "darwin_arm64",
		"Darwin x86_64":  "darwin_amd64",
		"Linux i686":     "linux_386",
		"SunOS sun4u":    "",
		"":               "",
		"Linux":          "",
		"weird a b":      "",
	}
	for in, want := range cases {
		if got := platformFromUname(in); got != want {
			t.Fatalf("platformFromUname(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestSelectPushBinary 二进制选择优先级：主机 binary_path > 平台表 >
// 同平台自身；跨平台未配置 → ok=false（调用方回退 SSH，不做无效上传）。
func TestSelectPushBinary(t *testing.T) {
	cfg := map[string]string{"linux_amd64": "/bin/wdp-linux-amd64"}
	const self = "darwin_arm64"

	// 主机显式指定最高优先
	if bin, useSelf, ok := selectPushBinary("/host/bin", "linux_amd64", self, cfg); !ok || useSelf || bin != "/host/bin" {
		t.Fatalf("host binary 应最优先: %q %v %v", bin, useSelf, ok)
	}
	// 平台表命中
	if bin, useSelf, ok := selectPushBinary("", "linux_amd64", self, cfg); !ok || useSelf || bin != "/bin/wdp-linux-amd64" {
		t.Fatalf("平台表应命中: %q %v %v", bin, useSelf, ok)
	}
	// 目标与控制端同平台：用自身
	if bin, useSelf, ok := selectPushBinary("", "darwin_arm64", self, cfg); !ok || !useSelf || bin != "" {
		t.Fatalf("同平台应用自身: %q %v %v", bin, useSelf, ok)
	}
	// 跨平台未配置：拒绝（回退 SSH）
	if _, _, ok := selectPushBinary("", "linux_arm64", self, cfg); ok {
		t.Fatal("跨平台未配置应 ok=false")
	}
	// 平台未知：退回自身尝试（旧行为兼容，由远端启动探测兜底）
	if _, useSelf, ok := selectPushBinary("", "", self, cfg); !ok || !useSelf {
		t.Fatalf("平台未知应用自身: %v %v", useSelf, ok)
	}
	// 空 cfg map 不 panic、同平台仍用自身
	if _, useSelf, ok := selectPushBinary("", "darwin_arm64", self, nil); !ok || !useSelf {
		t.Fatalf("nil cfg 同平台: %v %v", useSelf, ok)
	}
}
