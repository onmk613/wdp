package agentc

import (
	"context"
	"crypto/ed25519"
	crand "crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"wdp/internal/conn"
	"wdp/internal/model"
)

// TestNativeExtract 原生解压：新 agent 200 成功；无该端点的旧版 agent
// 返回 404 → ErrNativeUnsupported（模块据此回退 shell 路径）。
func TestNativeExtract(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /archive", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("src") == "" || r.URL.Query().Get("dest") == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`{"ok":true,"files":3}`))
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	if err := New(&model.Host{AgentURL: ts.URL}, nil).NativeExtract(context.Background(), "/a.tgz", "/opt/a"); err != nil {
		t.Fatalf("原生解压应成功: %v", err)
	}

	old := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(old.Close)
	err := New(&model.Host{AgentURL: old.URL}, nil).NativeExtract(context.Background(), "/a.tgz", "/opt/a")
	if !errors.Is(err, conn.ErrNativeUnsupported) {
		t.Fatalf("旧版 agent 应返回不支持哨兵: %v", err)
	}
}

// TestBaseURLAssembly 验证连接地址拼装：裸域名/IPv4/IPv6 拼接 agent 端口
// （IPv6 字面量自动加方括号），已含端口的 host:port 与 [ipv6]:port 原样保留。
func TestBaseURLAssembly(t *testing.T) {
	cases := []struct {
		name string
		host *model.Host
		want string
	}{
		{"裸域名拼默认端口", &model.Host{Address: "node1.example.com"}, "http://node1.example.com:7602"},
		{"裸IPv4拼默认端口", &model.Host{Address: "10.0.0.5"}, "http://10.0.0.5:7602"},
		{"裸IPv6拼默认端口", &model.Host{Address: "fd00::5"}, "http://[fd00::5]:7602"},
		{"IPv6自定义端口", &model.Host{Address: "fd00::5", AgentPort: 7700}, "http://[fd00::5]:7700"},
		{"IPv4含端口原样", &model.Host{Address: "10.0.0.5:9000"}, "http://10.0.0.5:9000"},
		{"IPv6含端口原样", &model.Host{Address: "[fd00::5]:9000"}, "http://[fd00::5]:9000"},
		{"TLS方案", &model.Host{Address: "10.0.0.5", TLS: true}, "https://10.0.0.5:7602"},
		// 仅内联私钥（KeyData）同样构成 TLS 启用条件：漏判会让该主机静默走明文
		{"仅内联私钥", &model.Host{Address: "10.0.0.5", KeyData: []byte("-----BEGIN PRIVATE KEY-----")}, "https://10.0.0.5:7602"},
	}
	for _, tc := range cases {
		if got := New(tc.host, nil).base; got != tc.want {
			t.Errorf("%s: base=%q want %q", tc.name, got, tc.want)
		}
	}
}

// TestHTTPURLWithTLSMaterialFails http:// 的 agent_url 与 TLS 材料（tls:true
// /ca_file/cert_file/key_file）并存：一切请求必须失败——静默忽略 TLS 会把
// 脚本/become 密码/文件内容全部明文传输。即使材料本身加载失败，矛盾判定
// 也要先行。
func TestHTTPURLWithTLSMaterialFails(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("矛盾配置下不应发出任何请求")
	}))
	t.Cleanup(ts.Close)
	for _, tc := range []struct {
		name string
		host *model.Host
	}{
		{"tls开关", &model.Host{AgentURL: ts.URL, TLS: true}},
		{"ca文件（不存在，加载也会失败）", &model.Host{AgentURL: ts.URL, CAFile: "/nonexistent/ca.pem"}},
		{"客户端证书", &model.Host{AgentURL: ts.URL, CertFile: "/nonexistent/c.pem", KeyFile: "/nonexistent/k.pem"}},
		{"仅内联私钥", &model.Host{AgentURL: ts.URL, KeyData: []byte("-----BEGIN PRIVATE KEY-----")}},
	} {
		c := New(tc.host, nil)
		err := c.Connect(context.Background())
		if err == nil || !strings.Contains(err.Error(), "plaintext") {
			t.Fatalf("%s: Connect 应 fail-loud 指出配置矛盾: %v", tc.name, err)
		}
		// 不经 Connect 的路径（agentctl status 直调 Health）同样失败
		if _, herr := c.Health(context.Background()); herr == nil || !strings.Contains(herr.Error(), "plaintext") {
			t.Fatalf("%s: Health 应 fail-loud: %v", tc.name, herr)
		}
	}
}

// TestPlanStatusEscapesRunID run_id 参与 URL 查询串：含 &/? 等保留字的
// 取值必须转义，否则被服务端拆成额外参数。
func TestPlanStatusEscapesRunID(t *testing.T) {
	const weird = "a&b=c?d/e f"
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("run_id"); got != weird {
			t.Errorf("run_id 应被正确转义还原，得 %q", got)
		}
		_, _ = w.Write([]byte(`{"run_id":"x","state":"done"}`))
	}))
	t.Cleanup(ts.Close)
	c := New(&model.Host{AgentURL: ts.URL}, nil)
	if _, err := c.PlanStatus(context.Background(), weird, 0); err != nil {
		t.Fatalf("转义后请求应成功: %v", err)
	}
}

// TestTLSServerNameInference ServerName 回退分支：自定义入口 + 自建 CA
// （ca_file 与内联 ca_data 等价）按 host 字段校验；Address 带端口时剥掉
// （证书 SAN 从不含端口）。
func TestTLSServerNameInference(t *testing.T) {
	caPEM := selfSignedCAPEM(t)
	cases := []struct {
		name string
		host *model.Host
		want string
	}{
		{"ca_data 带端口剥除", &model.Host{AgentURL: "https://127.0.0.1:9443", Address: "node1.example.com:7602", CAData: caPEM}, "node1.example.com"},
		{"ca_file 无端口原样", &model.Host{AgentURL: "https://127.0.0.1:9443", Address: "node1.example.com", CAFile: writeTemp(t, caPEM)}, "node1.example.com"},
		{"无 CA 不推断（按 URL 主机）", &model.Host{AgentURL: "https://127.0.0.1:9443", Address: "node1.example.com"}, ""},
		{"显式 server_name 优先", &model.Host{AgentURL: "https://127.0.0.1:9443", Address: "n:1", CAData: caPEM, TLSServerName: "explicit.example.com"}, "explicit.example.com"},
	}
	for _, tc := range cases {
		cfg, err := buildTLSConfig(tc.host)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if cfg.ServerName != tc.want {
			t.Errorf("%s: ServerName=%q want %q", tc.name, cfg.ServerName, tc.want)
		}
	}
}

// selfSignedCAPEM 生成测试用自签 CA PEM（ServerName 推断只需可解析的证书）。
func selfSignedCAPEM(t *testing.T) []byte {
	t.Helper()
	_, key, err := ed25519.GenerateKey(crand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(crand.Reader, tpl, tpl, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func writeTemp(t *testing.T, b []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}
