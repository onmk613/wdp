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

// startMTLS 启动带客户端证书校验的测试 agent。pinSelf=true 时把本次签发的
// 客户端证书指纹加入准许名单；pins 为额外名单（与 pinSelf 可叠加）。
// 返回（URL、CA 路径、控制端证书路径、控制端私钥路径）。
func startMTLS(t *testing.T, pinSelf bool, extraPins []string) (string, string, string, string) {
	t.Helper()
	dir := t.TempDir()
	if _, _, _, err := ca.Init(dir, ""); err != nil {
		t.Fatal(err)
	}
	srvCrt, srvKey, _, err := ca.Issue(dir, "127.0.0.1", nil, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	ctlCrt, ctlKey, fp, err := ca.Issue(dir, "ctl", nil, true, 0)
	if err != nil {
		t.Fatal(err)
	}
	pins := append([]string{}, extraPins...)
	if pinSelf {
		pins = append(pins, fp)
	}
	s := New(":0")
	if err := s.ConfigureAuth("", "", filepath.Join(dir, ca.CAFile), srvCrt, srvKey); err != nil {
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

	conn := agentconn.New(mtlsHost(url, caPath, crt, key))
	out, err := conn.Exec(context.Background(), connection.ExecRequest{Script: "echo pinned-ok"})
	if err != nil || !strings.Contains(out.Stdout, "pinned-ok") {
		t.Fatalf("准许指纹应放行: %+v err=%v", out, err)
	}
}

func TestPinRejectsUnpinnedCert(t *testing.T) {
	// 名单只放一个随机指纹（不是真实客户端证书的）
	url, caPath, crt, key := startMTLS(t, false, []string{strings.Repeat("ab", 32)})
	conn := agentconn.New(mtlsHost(url, caPath, crt, key))
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
	conn := agentconn.New(&model.Host{Name: "s", Conn: "agent", AgentURL: ts.URL, TLS: true})
	if err := conn.Connect(context.Background()); err == nil {
		t.Fatal("系统池模式下自签证书应被拒绝（fail-closed）")
	}

	// insecure_skip_verify 显式声明后放行
	conn2 := agentconn.New(&model.Host{Name: "s", Conn: "agent", AgentURL: ts.URL, TLS: true, InsecureSkipVerify: true})
	if err := conn2.Connect(context.Background()); err != nil {
		t.Fatalf("insecure_skip_verify 应放行: %v", err)
	}
}

// TestFailLoudOnBadCA：CA 文件缺失时 Connect 应给出明确错误（不静默降级）。
func TestFailLoudOnBadCA(t *testing.T) {
	_, _, crt, key := startMTLS(t, false, nil)
	host := &model.Host{Name: "b", Conn: "agent", AgentURL: "https://127.0.0.1:1",
		CAFile: filepath.Join(os.TempDir(), "no-such-ca.crt"), CertFile: crt, KeyFile: key}
	conn := agentconn.New(host)
	err := conn.Connect(context.Background())
	if err == nil || !strings.Contains(err.Error(), "CA") {
		t.Fatalf("应报 CA 加载错误，实际: %v", err)
	}
}
