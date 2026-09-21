package agent

// POST /cert 热更换回归：合法重签（保留私钥）成功换入并落盘；公钥不配对
// 与外来 CA 签发的证书被拒（宁拒不换——换入坏证书会让后续握手全挂）。

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"wdp/internal/ca"
)

// startMTLSAgent 起一个 mTLS agent（临时 CA + ctl 客户端证书），
// 返回（agent, 基址 https://127.0.0.1:port, 客户端, 主机证书三件套路径）。
func startMTLSAgent(t *testing.T) (*Server, string, *http.Client, string, string, string) {
	t.Helper()
	dir := t.TempDir()
	if _, _, _, err := ca.Init(ca.InitOptions{Dir: dir, Days: 3650}); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := ca.Issue(ca.IssueOptions{Dir: dir, Profile: ca.ProfileServer, SANs: []string{"127.0.0.1"}, Days: 30}, "host"); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := ca.Issue(ca.IssueOptions{Dir: dir, Profile: ca.ProfileClient, Days: 30}, "ctl"); err != nil {
		t.Fatal(err)
	}
	caFile := filepath.Join(dir, "ca.crt")
	crt := filepath.Join(dir, "host.crt")
	key := filepath.Join(dir, "host.key")

	s := New("127.0.0.1:0")
	if err := s.ConfigureAuth(caFile, crt, key); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() { _ = s.Serve(ln) }()

	ctlPair, err := tls.LoadX509KeyPair(filepath.Join(dir, "ctl.crt"), filepath.Join(dir, "ctl.key"))
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	caPEM, _ := os.ReadFile(caFile)
	pool.AppendCertsFromPEM(caPEM)
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool, Certificates: []tls.Certificate{ctlPair}},
	}}
	return s, fmt.Sprintf("https://%s", ln.Addr().String()), client, caFile, crt, key
}

func postCert(t *testing.T, base string, client *http.Client, certPath string) (*http.Response, string) {
	t.Helper()
	pemBytes, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Post(base+"/cert", "application/x-pem-file", bytesReader(pemBytes))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	resp.Body.Close()
	return resp, string(body)
}

func bytesReader(b []byte) io.Reader { return &sr{b} }

type sr struct{ b []byte }

func (r *sr) Read(p []byte) (int, error) {
	if len(r.b) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.b)
	r.b = r.b[n:]
	return n, nil
}

func TestCertHotSwap(t *testing.T) {
	s, base, client, caFile, crt, key := startMTLSAgent(t)

	before := s.material.Load().leaf.NotAfter
	// 合法重签：保留私钥、SAN 不变、延期 10 天
	newCrt, _, _, err := ca.Renew(ca.RenewOptions{
		CertPath: crt, KeyPath: key, OutPath: crt,
		CACertPath: caFile, CAKeyPath: filepath.Join(filepath.Dir(caFile), "ca.key"),
		Days: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	resp, body := postCert(t, base, client, newCrt)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("合法换证应 200: %d %s", resp.StatusCode, body)
	}
	var out struct {
		OK       bool   `json:"ok"`
		NotAfter string `json:"not_after"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil || !out.OK {
		t.Fatalf("换证响应异常: %v %s", err, body)
	}
	// 材料已换入：leaf 到期时刻延长
	after := s.material.Load().leaf.NotAfter
	if !after.After(before) {
		t.Fatalf("材料未更新: before=%s after=%s", before, after)
	}
	// 落盘校验：tlsCertFile 内容是新证书（重启后加载的即新证）
	disk, err := os.ReadFile(s.tlsCertFile)
	if err != nil {
		t.Fatal(err)
	}
	blk, _ := pem.Decode(disk)
	leaf, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if !leaf.NotAfter.Equal(after) {
		t.Fatalf("落盘证书与内存材料不一致: disk=%s mem=%s", leaf.NotAfter, after)
	}

	// 拒绝路径：同 CA 新钥证书（公钥不配对）
	foreign, _, _, err := ca.Issue(ca.IssueOptions{
		Dir: filepath.Dir(caFile), Profile: ca.ProfileServer, SANs: []string{"127.0.0.1"}, Days: 10,
	}, "otherkey")
	if err != nil {
		t.Fatal(err)
	}
	if resp, body := postCert(t, base, client, foreign); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("公钥不配对应 400: %d %s", resp.StatusCode, body)
	}

	// 拒绝路径：外来 CA 对同一钥匙重签（链验证失败）
	otherCA := t.TempDir()
	if _, _, _, err := ca.Init(ca.InitOptions{Dir: otherCA, Days: 3650}); err != nil {
		t.Fatal(err)
	}
	evil, _, _, err := ca.Renew(ca.RenewOptions{
		CertPath: crt, KeyPath: key, OutPath: filepath.Join(otherCA, "evil.crt"),
		CACertPath: filepath.Join(otherCA, "ca.crt"), CAKeyPath: filepath.Join(otherCA, "ca.key"),
		Days: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp, body := postCert(t, base, client, evil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("外来 CA 证书应 400: %d %s", resp.StatusCode, body)
	}
	// 拒绝后材料未被污染
	if !s.material.Load().leaf.NotAfter.Equal(after) {
		t.Fatal("被拒绝的换证不应改动材料")
	}
}
