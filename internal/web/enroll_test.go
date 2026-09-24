package web

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"wdp/internal/store"
)

// newEnrollServer 构造启用纳管（带 CA）的测试服务（管理员 admin/passw0rd）。
func newEnrollServer(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
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
	s, err := New(st, Options{
		AdminUser: "admin", // AdminPass 留空：不干预直接 CreateUser 写入的 passw0rd
		CADir:     filepath.Join(t.TempDir(), "ca"),
		// 测试走 httptest 明文（无 TLS 反代）：与可信内网部署一样需要显式
		// 打开明文引导开关。默认拒绝的行为由 TestEnrollRefusesPlaintext 覆盖
		AllowPlaintextEnroll: true,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return s, st
}

// TestEnrollFullFlow 纳管全流程：token → 脚本 → CA（指纹校验）→ 二进制 →
// claim（幂等 + 证书签发）→ 证书下载 → done（消费 + 落账）→ 复用拒绝。
func TestEnrollFullFlow(t *testing.T) {
	s, st := newEnrollServer(t)

	// 注入二进制解析器（指向临时 bin 目录）
	binDir := t.TempDir()
	fakeBin := filepath.Join(binDir, "wdp-linux-amd64")
	if err := os.WriteFile(fakeBin, []byte("#!/bin/sh\nfake wdp binary\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	s.binResolver = func(platform string) (string, bool) {
		p := filepath.Join(binDir, "wdp-"+strings.ReplaceAll(platform, "_", "-"))
		if _, err := os.Stat(p); err == nil {
			return p, true
		}
		return "", false
	}

	// 1. 生成 token（需登录）
	token := ""
	tokenCookie := loginSession(t, s)
	rec := do(t, s.Handler(), "POST", "/api/enroll-tokens", map[string]any{"host": "web9"}, &tokenCookie)
	if rec.Code != http.StatusCreated {
		t.Fatalf("生成 token 应 201: %d %s", rec.Code, rec.Body)
	}
	var created struct {
		Token   string `json:"token"`
		Command string `json:"command"`
	}
	json.Unmarshal(rec.Body.Bytes(), &created)
	if len(created.Token) != 48 || !strings.Contains(created.Command, created.Token) {
		t.Fatalf("token/command 异常: %+v", created)
	}
	token = created.Token

	h := s.Handler()

	// 未知 token / 非法平台
	if rec := do(t, h, "GET", "/enroll/nonexistent/script.sh", nil, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("未知 token 应 404: %d", rec.Code)
	}
	// 含 ".." 的路径被 ServeMux 清洗重定向，不会进入 handler（无穿越风险）
	recPT := do(t, h, "GET", "/enroll/"+token+"/binary/../evil", nil, nil)
	if recPT.Code == http.StatusOK || strings.Contains(recPT.Header().Get("Location"), "..") {
		t.Fatalf("路径穿越应被清洗: %d %s", recPT.Code, recPT.Header().Get("Location"))
	}
	if rec := do(t, h, "GET", "/enroll/"+token+"/binary/linux_riscv64", nil, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("无对应二进制应 404: %d", rec.Code)
	}

	// 2. 脚本：含 token、CA 指纹、平台探测
	rec = do(t, h, "GET", "/enroll/"+token+"/script.sh", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("脚本应 200: %d %s", rec.Code, rec.Body)
	}
	script := rec.Body.String()
	for _, want := range []string{token, "linux_amd64", "UNIT=wdp-agent", "systemctl enable --now"} {
		if !strings.Contains(script, want) {
			t.Fatalf("脚本缺 %q:\n%s", want, script)
		}
	}
	fpRe := regexp.MustCompile(`CA_FP='([0-9a-f]{64})'`)
	m := fpRe.FindStringSubmatch(script)
	if m == nil {
		t.Fatalf("脚本缺 CA 指纹:\n%s", script)
	}

	// 3. CA 下载 + 文件指纹与脚本一致
	rec = do(t, h, "GET", "/enroll/"+token+"/ca", nil, nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "BEGIN CERTIFICATE") {
		t.Fatalf("CA 下载异常: %d", rec.Code)
	}
	sum := sha256.Sum256(rec.Body.Bytes())
	if got := hex.EncodeToString(sum[:]); got != m[1] {
		t.Fatalf("CA 文件指纹与脚本不一致: %s vs %s", got, m[1])
	}

	// 4. 二进制下发
	rec = do(t, h, "GET", "/enroll/"+token+"/binary/linux_amd64", nil, nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "fake wdp binary") {
		t.Fatalf("二进制下发异常: %d", rec.Code)
	}

	// 5. 未 claim 不能下载证书
	if rec := do(t, h, "GET", "/enroll/"+token+"/host-cert", nil, nil); rec.Code != http.StatusGone {
		t.Fatalf("未 claim 取证书应 410: %d", rec.Code)
	}

	// 6. claim（幂等）→ 证书签发
	for i := 0; i < 2; i++ {
		rec = do(t, h, "POST", "/enroll/"+token+"/claim", map[string]any{"hostname": "web9", "platform": "linux_amd64"}, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("claim 第 %d 次应 200: %d %s", i+1, rec.Code, rec.Body)
		}
	}
	rec = do(t, h, "GET", "/enroll/"+token+"/host-cert", nil, nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "BEGIN CERTIFICATE") {
		t.Fatalf("主机证书异常: %d %s", rec.Code, rec.Body)
	}
	rec = do(t, h, "GET", "/enroll/"+token+"/host-key", nil, nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "PRIVATE KEY") {
		t.Fatalf("主机私钥异常: %d", rec.Code)
	}

	// 7. done：消费 token + 落账（绑定名 web9 优先于 claim hostname）
	rec = do(t, h, "POST", "/enroll/"+token+"/done", map[string]any{"agent_port": 7602}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("done 应 200: %d %s", rec.Code, rec.Body)
	}
	hosts, _ := st.ListHosts("")
	if len(hosts) != 1 || hosts[0].Name != "web9" || hosts[0].Address == "" || hosts[0].AgentPort != 7602 {
		t.Fatalf("落账异常: %+v", hosts)
	}

	// 8. 复用已消费 token → 410
	if rec := do(t, h, "GET", "/enroll/"+token+"/script.sh", nil, nil); rec.Code != http.StatusGone {
		t.Fatalf("已用 token 应 410: %d", rec.Code)
	}
	if rec := do(t, h, "POST", "/enroll/"+token+"/done", map[string]any{}, nil); rec.Code != http.StatusGone {
		t.Fatalf("重复 done 应 410: %d", rec.Code)
	}
}

// TestEnrollTokenExpiry 过期 token 全端点 410。
func TestEnrollTokenExpiry(t *testing.T) {
	s, st := newEnrollServer(t)
	if err := st.CreateEnrollToken("expiredtoken", "", 7602, -time.Minute); err != nil {
		t.Fatal(err)
	}
	h := s.Handler()
	for _, tc := range []struct {
		method, path string
		body         any
	}{
		{"GET", "/enroll/expiredtoken/script.sh", nil},
		{"GET", "/enroll/expiredtoken/ca", nil},
		{"POST", "/enroll/expiredtoken/claim", map[string]any{"hostname": "h"}},
		{"POST", "/enroll/expiredtoken/done", map[string]any{}},
	} {
		if rec := do(t, h, tc.method, tc.path, tc.body, nil); rec.Code != http.StatusGone {
			t.Fatalf("%s %s 过期应 410: %d", tc.method, tc.path, rec.Code)
		}
	}
}

// TestEnrollScriptCommand advertise 优先于请求 Host。
func TestEnrollScriptCommand(t *testing.T) {
	s, _ := newEnrollServer(t)
	if err := s.st.CreateEnrollToken("advertoken", "", 7602, time.Hour); err != nil {
		t.Fatal(err)
	}
	s.opts.AdvertiseURL = "http://10.9.9.9:9999/"
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/enroll/advertoken/script.sh", nil)
	req.Host = "irrelevant:1"
	s.Handler().ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), "BASE='http://10.9.9.9:9999'") {
		t.Fatalf("advertise 基址未生效: code=%d\n%s", rec.Code, rec.Body)
	}
}

// TestEnrollTokenRequiresPerm 纳管凭证签发需 host:enroll（viewer 403）。
func TestEnrollTokenRequiresPerm(t *testing.T) {
	s, _ := newEnrollServer(t)
	h := s.Handler()
	admin := loginSession(t, s)
	viewer := newUserSession(t, h, admin, "vw1", "viewer-Pass1", "viewer")
	if rec := do(t, h, "POST", "/api/enroll-tokens", map[string]any{"host": "web9"}, &viewer); rec.Code != http.StatusForbidden {
		t.Fatalf("viewer 签发纳管凭证应 403: %d %s", rec.Code, rec.Body)
	}
}

// TestEnrollScriptHostHeaderInjection Host 头不可信：host 部分含引号直接
// 报错；引号落在端口部分时 BASE 为单引号安全引用（不闭合注入）。
func TestEnrollScriptHostHeaderInjection(t *testing.T) {
	s, _ := newEnrollServer(t)
	if err := s.st.CreateEnrollToken("injtoken", "", 7602, time.Hour); err != nil {
		t.Fatal(err)
	}
	h := s.Handler()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/enroll/injtoken/script.sh", nil)
	req.Host = "10.0.0.1':7603"
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("host 部分含引号应 400: %d %s", rec.Code, rec.Body)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/enroll/injtoken/script.sh", nil)
	req.Host = `10.0.0.1:7603'`
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("端口部分引号应仍出脚本（安全引用兜底）: %d %s", rec.Code, rec.Body)
	}
	if want := `BASE='http://10.0.0.1:7603'\'''`; !strings.Contains(rec.Body.String(), want) {
		t.Fatalf("BASE 应为安全单引号引用:\n%s", rec.Body)
	}
}

// TestRemoteIPTrustedProxy XFF 采信语义：直连（非信任对端）伪造 XFF 一律
// 忽略取真实对端；回环（本机反代）与显式 --trust-proxy 对端才采信 XFF。
// 采信的是**最后一段**：追加式反代（nginx $proxy_add_x_forwarded_for）
// 把它看到的真实对端追加在末尾，首段是客户端自带的可伪造值。
func TestRemoteIPTrustedProxy(t *testing.T) {
	s, _ := newEnrollServer(t)
	xff := func(remote, forwarded string) string {
		req := httptest.NewRequest("GET", "/", nil)
		req.RemoteAddr = remote
		if forwarded != "" {
			req.Header.Set("X-Forwarded-For", forwarded)
		}
		return s.remoteIP(req)
	}
	if got := xff("203.0.113.9:4444", "10.0.0.1"); got != "203.0.113.9" {
		t.Errorf("direct client spoofed XFF: got %q, want real peer 203.0.113.9", got)
	}
	if got := xff("127.0.0.1:5555", "10.0.0.1, 127.0.0.2"); got != "127.0.0.2" {
		t.Errorf("loopback proxy must honor last (proxy-appended) XFF entry: got %q, want 127.0.0.2", got)
	}
	if got := xff("127.0.0.1:5555", "not-an-ip"); got != "127.0.0.1" {
		t.Errorf("unparseable XFF should fall back to peer: got %q", got)
	}
	if got := xff("192.0.2.10:6666", "10.0.0.9"); got != "192.0.2.10" {
		t.Errorf("untrusted remote proxy XFF must be ignored: got %q", got)
	}

	st2, err := store.Open(filepath.Join(t.TempDir(), "t2.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st2.Close() })
	hash, _ := bcrypt.GenerateFromPassword([]byte("passw0rd"), bcrypt.MinCost)
	if err := st2.CreateUser("admin", string(hash), "admin"); err != nil {
		t.Fatal(err)
	}
	s2, err := New(st2, Options{TrustedProxies: []string{"198.51.100.0/24"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "198.51.100.7:9999"
	req.Header.Set("X-Forwarded-For", "10.0.0.8")
	if got := s2.remoteIP(req); got != "10.0.0.8" {
		t.Errorf("explicitly trusted proxy should honor XFF: got %q", got)
	}
	req.RemoteAddr = "198.51.100.7:notaport" // SplitHostPort 照常拆出 host
	req.Header.Del("X-Forwarded-For")
	if got := s2.remoteIP(req); got != "198.51.100.7" {
		t.Errorf("non-numeric port RemoteAddr should still resolve peer host: got %q", got)
	}
}
