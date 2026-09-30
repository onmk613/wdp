package web

// 纳管引导链路回归：
//   - 明文 HTTP 下默认拒绝下发一键命令（脚本以 root 执行，脚本内嵌的
//     CA 指纹与脚本同源，挡不住主动中间人）；
//   - --advertise 只接受 https（除非显式打开明文开关）；
//   - CSR 换证绑定 claim 来源 IP，token 锁定到首个公钥（异钥 410）。

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"wdp/internal/ca"
	"wdp/internal/store"
)

// TestEnrollRefusesPlaintext 默认（未开 AllowPlaintextEnroll）拒绝明文引导。
func TestEnrollRefusesPlaintext(t *testing.T) {
	st := newEnrollOnlyStore(t)
	s, err := New(st, Options{AdminUser: "admin", CADir: t.TempDir()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	token := loginSession(t, s)

	rec := do(t, s.Handler(), "POST", "/api/enroll-tokens", map[string]any{"host": "web1"}, &token)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("明文下默认应拒绝签发一键命令: %d %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "plaintext") {
		t.Fatalf("拒绝理由应指明明文通道: %s", rec.Body)
	}

	// 显式 advertise https 时放行（部署在 TLS 反代之后的形态）
	s.opts.AdvertiseURL = "https://wdp.example.com"
	rec = do(t, s.Handler(), "POST", "/api/enroll-tokens", map[string]any{"host": "web1"}, &token)
	if rec.Code != http.StatusCreated {
		t.Fatalf("https advertise 下应放行: %d %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "https://wdp.example.com/enroll/") {
		t.Fatalf("一键命令应使用 https 基址: %s", rec.Body)
	}

	// http advertise 在未开开关时必须拒绝
	s.opts.AdvertiseURL = "http://wdp.example.com"
	if rec := do(t, s.Handler(), "POST", "/api/enroll-tokens", map[string]any{"host": "web2"}, &token); rec.Code != http.StatusBadRequest {
		t.Fatalf("http advertise 应被拒: %d %s", rec.Code, rec.Body)
	}
}

// TestEnrollCSRBoundToClaimSource CSR 换证绑定 claim 来源 IP，且 token
// 锁定到首个公钥（token 会经 URL 进日志/历史：来源绑定挡异地取用，
// 异钥拒绝挡"泄露后换钥匙冒名"）。
func TestEnrollCSRBoundToClaimSource(t *testing.T) {
	s, st := newEnrollServer(t)

	if err := st.CreateEnrollToken("tok-key", "keyhost", 7602, time.Hour); err != nil {
		t.Fatal(err)
	}
	// claim（默认 httptest 来源 192.0.2.1）
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/enroll/tok-key/claim", strings.NewReader(`{"hostname":"keyhost"}`))
	req.Header.Set("Content-Type", "application/json")
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("claim 应成功: %d %s", rec.Code, rec.Body)
	}

	// 目标机本地生成密钥 + CSR（两次生成 = 两把不同的钥匙）
	dir := t.TempDir()
	mkCSR := func(name string) []byte {
		t.Helper()
		if _, err := ca.GenCSR(filepath.Join(dir, name+".key"), filepath.Join(dir, name+".csr")); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(filepath.Join(dir, name+".csr"))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	postCSR := func(token string, pem []byte, remote string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/enroll/"+token+"/csr", bytes.NewReader(pem))
		r.RemoteAddr = remote
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		return w
	}

	// 同一来源：首把钥匙换到证书，同钥重试幂等
	first := mkCSR("a")
	if w := postCSR("tok-key", first, "192.0.2.1:5555"); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "BEGIN CERTIFICATE") {
		t.Fatalf("claim 来源应可换证书: %d %s", w.Code, strings.TrimSpace(w.Body.String()))
	}
	if w := postCSR("tok-key", first, "192.0.2.1:5555"); w.Code != http.StatusOK {
		t.Fatalf("同钥重试应幂等: %d %s", w.Code, w.Body)
	}
	// 换第二把钥匙：token 已锁定到首个公钥 → 410
	if w := postCSR("tok-key", mkCSR("b"), "192.0.2.1:5555"); w.Code != http.StatusGone {
		t.Fatalf("异钥 CSR 应 410: %d %s", w.Code, w.Body)
	}
	// 非 CSR 载荷：400
	if w := postCSR("tok-key", []byte("garbage"), "192.0.2.1:5555"); w.Code != http.StatusBadRequest {
		t.Fatalf("坏 CSR 应 400: %d %s", w.Code, w.Body)
	}

	// 换来源提交 CSR：拒绝（模拟日志泄露后的异地取用）
	if err := st.CreateEnrollToken("tok-ip", "iph", 7602, time.Hour); err != nil {
		t.Fatal(err)
	}
	req2 := httptest.NewRequest("POST", "/enroll/tok-ip/claim", strings.NewReader(`{"hostname":"iph"}`))
	req2.Header.Set("Content-Type", "application/json")
	req2.RemoteAddr = "192.0.2.9:1234"
	s.Handler().ServeHTTP(httptest.NewRecorder(), req2)

	if w := postCSR("tok-ip", mkCSR("c"), "203.0.113.7:1234"); w.Code != http.StatusForbidden {
		t.Fatalf("异地提交 CSR 应 403: %d %s", w.Code, w.Body)
	}
}

// TestEnrollBinarySHA256Endpoint 摘要端点回显真实二进制摘要（脚本据此校验）。
func TestEnrollBinarySHA256Endpoint(t *testing.T) {
	s, st := newEnrollServer(t)
	if err := st.CreateEnrollToken("tok-sum", "", 7602, time.Hour); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "/enroll/tok-sum/binary-sha256/linux_amd64", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	// 测试环境没有 bin/ 目录：应 404 而不是 500/空摘要
	if w.Code == http.StatusOK && strings.TrimSpace(w.Body.String()) == "" {
		t.Fatal("摘要端点不得返回空摘要")
	}
	if w.Code != http.StatusNotFound && w.Code != http.StatusOK {
		t.Fatalf("摘要端点应 200/404: %d %s", w.Code, w.Body)
	}
}

// newEnrollOnlyStore 建一个只有 admin 账号的库（供自定义 Options 的用例）。
func newEnrollOnlyStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	hash, err := bcrypt.GenerateFromPassword([]byte("passw0rd"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateUser("admin", string(hash), "admin"); err != nil {
		t.Fatal(err)
	}
	return st
}
