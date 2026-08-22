package agent

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"wdp/internal/ca"
	"wdp/internal/connection"
	"wdp/internal/connection/agentconn"
	"wdp/internal/model"
)

// startMTLS 启动带客户端证书校验的测试 agent（服务端证书 SAN=127.0.0.1）。
func startMTLS(t *testing.T, pinSelf bool, extraPins []string) (string, string, string, string) {
	return startMTLSNamed(t, "127.0.0.1", pinSelf, extraPins)
}

// startMTLSNamed 服务端证书以 serverName 作为 SAN 签发（测试主机名不匹配场景）。
func startMTLSNamed(t *testing.T, serverName string, pinSelf bool, extraPins []string) (string, string, string, string) {
	t.Helper()
	dir := t.TempDir()
	if _, _, _, err := ca.Init(dir, ""); err != nil {
		t.Fatal(err)
	}
	srvCrt, srvKey, _, err := ca.Issue(ca.IssueOptions{Dir: dir}, serverName)
	if err != nil {
		t.Fatal(err)
	}
	ctlCrt, ctlKey, fp, err := ca.Issue(ca.IssueOptions{Dir: dir, Client: true}, "ctl")
	if err != nil {
		t.Fatal(err)
	}
	pins := append([]string{}, extraPins...)
	if pinSelf {
		pins = append(pins, fp)
	}
	s := New(":0")
	if err := s.ConfigureAuth(filepath.Join(dir, ca.CAFile), srvCrt, srvKey); err != nil {
		t.Fatal(err)
	}
	if err := s.PinClientFingerprints(pins); err != nil {
		t.Fatal(err)
	}
	serverCert, err := tls.LoadX509KeyPair(srvCrt, srvKey)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewUnstartedServer(s.Handler())
	// 显式提供服务端证书（httptest 缺省会附加内置自签证书）
	ts.TLS = &tls.Config{
		Certificates: []tls.Certificate{serverCert},
		ClientCAs:    s.clientCAs,
		ClientAuth:   tls.RequireAndVerifyClientCert,
	}
	ts.StartTLS()
	t.Cleanup(ts.Close)
	return ts.URL, filepath.Join(dir, ca.CAFile), ctlCrt, ctlKey
}

func mtlsHost(url, caFile, crt, key string) *model.Host {
	return &model.Host{Name: "m", Conn: "agent", AgentURL: url, CAFile: caFile, CertFile: crt, KeyFile: key}
}

func TestPinAllowsPinnedCert(t *testing.T) {
	url, caPath, crt, key := startMTLS(t, true, nil) // 名单含本次签发的客户端证书

	conn := agentconn.New(mtlsHost(url, caPath, crt, key), nil)
	out, err := conn.Exec(context.Background(), connection.ExecRequest{Script: "echo pinned-ok"})
	if err != nil || !strings.Contains(out.Stdout, "pinned-ok") {
		t.Fatalf("准许指纹应放行: %+v err=%v", out, err)
	}
}

func TestPinRejectsUnpinnedCert(t *testing.T) {
	// 名单只放一个随机指纹（不是真实客户端证书的）
	url, caPath, crt, key := startMTLS(t, false, []string{strings.Repeat("ab", 32)})
	conn := agentconn.New(mtlsHost(url, caPath, crt, key), nil)
	if _, err := conn.Exec(context.Background(), connection.ExecRequest{Script: "echo x"}); err == nil {
		t.Fatal("未在准许名单的证书应被拒")
	}
}

func TestPinInvalidFormat(t *testing.T) {
	s := New(":0")
	if err := s.PinClientFingerprints([]string{"not-a-fp"}); err == nil {
		t.Fatal("非法指纹格式应报错")
	}
}

// TestTLSSkipHostVerifyAndServerName 服务端证书 SAN 与连接地址不符时的处置：
// 默认主机名不匹配拒绝；tls_server_name 改按证书 SAN 中的名称校验放行；
// tls_skip_host_verify 跳过主机名但保留 CA 链校验放行（错误 CA 仍拒绝）。
func TestTLSSkipHostVerifyAndServerName(t *testing.T) {
	url, caPath, crt, key := startMTLSNamed(t, "agent1", true, nil)
	ctx := context.Background()

	// 默认：证书 SAN=agent1，连接地址是 127.0.0.1 → 主机名不匹配，拒绝
	if err := agentconn.New(mtlsHost(url, caPath, crt, key), nil).Connect(ctx); err == nil {
		t.Fatal("主机名不匹配应拒绝")
	}

	// tls_server_name=agent1：改按证书 SAN 中的名称校验 → 放行
	h := mtlsHost(url, caPath, crt, key)
	h.TLSServerName = "agent1"
	if err := agentconn.New(h, nil).Connect(ctx); err != nil {
		t.Fatalf("tls_server_name 应放行: %v", err)
	}

	// tls_skip_host_verify：跳过主机名、保留链校验 → 放行
	h2 := mtlsHost(url, caPath, crt, key)
	h2.TLSSkipHostVerify = true
	if err := agentconn.New(h2, nil).Connect(ctx); err != nil {
		t.Fatalf("tls_skip_host_verify 应放行: %v", err)
	}

	// tls_skip_host_verify 不放松链校验：换成无关 CA 仍拒绝
	other := t.TempDir()
	if _, _, _, err := ca.Init(other, ""); err != nil {
		t.Fatal(err)
	}
	h3 := mtlsHost(url, filepath.Join(other, ca.CAFile), crt, key)
	h3.TLSSkipHostVerify = true
	if err := agentconn.New(h3, nil).Connect(ctx); err == nil {
		t.Fatal("链校验应仍然生效（错误 CA 拒绝）")
	}
}

// TestDefaultServerNameFromHost 端口转发/NAT 场景（agent_url 入口 ≠ 证书 SAN）：
// 默认按 host 字段（未填即 inventory 主机名）校验，无需 tls_server_name。
func TestDefaultServerNameFromHost(t *testing.T) {
	ctx := context.Background()

	// 证书 SAN=host 字段值（IP），经 agent_url 127.0.0.1 入口连接 → 默认放行
	url, caPath, crt, key := startMTLSNamed(t, "10.0.0.14", true, nil)
	h := &model.Host{Name: "web1", Conn: "agent", Address: "10.0.0.14",
		AgentURL: url, CAFile: caPath, CertFile: crt, KeyFile: key}
	if err := agentconn.New(h, nil).Connect(ctx); err != nil {
		t.Fatalf("默认按 host 字段校验应放行: %v", err)
	}

	// 证书 SAN=inventory 主机名（未填 host 字段，Address 缺省即主机名）→ 默认放行
	url2, caPath2, crt2, key2 := startMTLSNamed(t, "web1", true, nil)
	h2 := &model.Host{Name: "web1", Conn: "agent", Address: "web1",
		AgentURL: url2, CAFile: caPath2, CertFile: crt2, KeyFile: key2}
	if err := agentconn.New(h2, nil).Connect(ctx); err != nil {
		t.Fatalf("默认按主机名校验应放行: %v", err)
	}

	// 与两者都无关的 SAN 仍拒绝（默认目标不是入口地址）
	url3, caPath3, crt3, key3 := startMTLSNamed(t, "other-name", true, nil)
	h3 := &model.Host{Name: "web1", Conn: "agent", Address: "10.0.0.14",
		AgentURL: url3, CAFile: caPath3, CertFile: crt3, KeyFile: key3}
	if err := agentconn.New(h3, nil).Connect(ctx); err == nil {
		t.Fatal("SAN 与主机名/host 字段均不符应拒绝")
	}
}

// TestSystemPoolFailClosed：不配置 CA 时走系统证书池 → 自签服务端证书应握手失败（不静默放行）。
func TestSystemPoolFailClosed(t *testing.T) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "self"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames: []string{"127.0.0.1"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, _ := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	cert := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	ts := httptest.NewUnstartedServer(New(":0").Handler())
	ts.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	ts.StartTLS()
	t.Cleanup(ts.Close)

	// 无 CA 配置 → 系统池 → 自签证书被拒
	conn := agentconn.New(&model.Host{Name: "s", Conn: "agent", AgentURL: ts.URL, TLS: true}, nil)
	if err := conn.Connect(context.Background()); err == nil {
		t.Fatal("系统池模式下自签证书应被拒绝（fail-closed）")
	}

	// insecure_skip_verify 显式声明后放行
	conn2 := agentconn.New(&model.Host{Name: "s", Conn: "agent", AgentURL: ts.URL, TLS: true, InsecureSkipVerify: true}, nil)
	if err := conn2.Connect(context.Background()); err != nil {
		t.Fatalf("insecure_skip_verify 应放行: %v", err)
	}
}

// TestFailLoudOnBadCA：CA 文件缺失时 Connect 应给出明确错误（不静默降级）。
func TestFailLoudOnBadCA(t *testing.T) {
	_, _, crt, key := startMTLS(t, false, nil)
	host := &model.Host{Name: "b", Conn: "agent", AgentURL: "https://127.0.0.1:1",
		CAFile: filepath.Join(os.TempDir(), "no-such-ca.crt"), CertFile: crt, KeyFile: key}
	conn := agentconn.New(host, nil)
	err := conn.Connect(context.Background())
	if err == nil || !strings.Contains(err.Error(), "CA") {
		t.Fatalf("应报 CA 加载错误，实际: %v", err)
	}
}
