package web

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"wdp/internal/store"
)

// newTestServer 构造带固定管理员（admin/passw0rd）的测试服务。
func newTestServer(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	dataDir := t.TempDir()
	st, err := store.Open(filepath.Join(dataDir, "test.db"))
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
	// DataDir 必须显式给：缺省会落到相对路径 wdp-data/apps（包目录下），
	// 版本制品跨测试运行残留，后续运行的"版本已存在"预检会被误伤
	s, err := New(st, Options{AdminUser: "admin", DataDir: dataDir}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return s, st
}

func do(t *testing.T, h http.Handler, method, path string, body any, cookie *string) *httptest.ResponseRecorder {
	t.Helper()
	var rd *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if cookie != nil && *cookie != "" {
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: *cookie})
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func loginSession(t *testing.T, s *Server) string {
	t.Helper()
	rec := do(t, s.Handler(), "POST", "/api/login", map[string]string{"user": "admin", "password": "passw0rd"}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("login 应 200: %d %s", rec.Code, rec.Body)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie {
			return c.Value
		}
	}
	t.Fatal("login 未下发会话 cookie")
	return ""
}

// TestAuthFlow 登录 → 会话访问 → 登出失效；错误凭据与未登录拒绝。
func TestAuthFlow(t *testing.T) {
	s, _ := newTestServer(t)
	h := s.Handler()

	// 未登录拒绝
	if rec := do(t, h, "GET", "/api/hosts", nil, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("未登录应 401: %d", rec.Code)
	}
	// 错误密码
	if rec := do(t, h, "POST", "/api/login", map[string]string{"user": "admin", "password": "wrong"}, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("错误凭据应 401: %d", rec.Code)
	}
	// 不存在的用户同样 401（不泄露存在性）
	if rec := do(t, h, "POST", "/api/login", map[string]string{"user": "nobody", "password": "x"}, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("不存在用户应 401: %d", rec.Code)
	}

	token := loginSession(t, s)
	if rec := do(t, h, "GET", "/api/me", nil, &token); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "admin") {
		t.Fatalf("me 应回显用户: %d %s", rec.Code, rec.Body)
	}

	// 登出后会话失效
	if rec := do(t, h, "POST", "/api/logout", map[string]any{}, &token); rec.Code != http.StatusOK {
		t.Fatalf("logout 应 200: %d", rec.Code)
	}
	if rec := do(t, h, "GET", "/api/hosts", nil, &token); rec.Code != http.StatusUnauthorized {
		t.Fatalf("登出后应 401: %d", rec.Code)
	}
}

// TestHostAPI 主机 CRUD 全链路（含校验与 404）。
func TestHostAPI(t *testing.T) {
	s, _ := newTestServer(t)
	h := s.Handler()
	token := loginSession(t, s)

	rec := do(t, h, "POST", "/api/hosts", map[string]any{
		"Name": "web1", "Address": "10.0.0.11", "AgentPort": 7602, "Group": "web", "Labels": `{"env":"prod"}`,
	}, &token)
	if rec.Code != http.StatusCreated {
		t.Fatalf("创建应 201: %d %s", rec.Code, rec.Body)
	}
	var created store.Host
	json.Unmarshal(rec.Body.Bytes(), &created)
	if created.ID == 0 || created.Status != "unknown" {
		t.Fatalf("创建返回异常: %+v", created)
	}

	// 列表
	rec = do(t, h, "GET", "/api/hosts", nil, &token)
	var list []*store.Host
	json.Unmarshal(rec.Body.Bytes(), &list)
	if len(list) != 1 || list[0].Name != "web1" {
		t.Fatalf("列表异常: %s", rec.Body)
	}

	// 更新
	rec = do(t, h, "PUT", "/api/hosts/1", map[string]any{"Address": "10.0.0.12", "AgentPort": 7700}, &token)
	if rec.Code != http.StatusOK {
		t.Fatalf("更新应 200: %d %s", rec.Code, rec.Body)
	}
	// 非法 labels
	if rec := do(t, h, "PUT", "/api/hosts/1", map[string]any{"Address": "10.0.0.12", "Labels": "not-json"}, &token); rec.Code != http.StatusBadRequest {
		t.Fatalf("非法 labels 应 400: %d", rec.Code)
	}

	// 404 与非法 id
	if rec := do(t, h, "GET", "/api/hosts/99", nil, &token); rec.Code != http.StatusNotFound {
		t.Fatalf("不存在应 404: %d", rec.Code)
	}
	if rec := do(t, h, "GET", "/api/hosts/abc", nil, &token); rec.Code != http.StatusBadRequest {
		t.Fatalf("非法 id 应 400: %d", rec.Code)
	}

	// 探活（指向不存在的地址 → offline，状态写回）
	rec = do(t, h, "POST", "/api/hosts/1/probe", map[string]any{}, &token)
	if rec.Code != http.StatusOK {
		t.Fatalf("探活应 200: %d %s", rec.Code, rec.Body)
	}
	var pr ProbeResult
	json.Unmarshal(rec.Body.Bytes(), &pr)
	if pr.Status != "offline" || pr.Error == "" {
		t.Fatalf("不可达主机应 offline 带原因: %+v", pr)
	}
	rec = do(t, h, "GET", "/api/hosts/1", nil, &token)
	var got store.Host
	json.Unmarshal(rec.Body.Bytes(), &got)
	if got.Status != "offline" {
		t.Fatalf("探活状态应写回: %+v", got)
	}

	// 删除
	if rec := do(t, h, "DELETE", "/api/hosts/1", nil, &token); rec.Code != http.StatusOK {
		t.Fatalf("删除应 200: %d", rec.Code)
	}
	if rec := do(t, h, "DELETE", "/api/hosts/1", nil, &token); rec.Code != http.StatusNotFound {
		t.Fatalf("重复删除应 404: %d", rec.Code)
	}
}

// TestCSRFGuard 变更类请求带非 JSON Content-Type 被拒。
func TestCSRFGuard(t *testing.T) {
	s, _ := newTestServer(t)
	token := loginSession(t, s)
	req := httptest.NewRequest("POST", "/api/hosts", strings.NewReader("name=x"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("表单 Content-Type 应 415: %d", rec.Code)
	}
}

// TestLoginContentType 登录端点自身也要求 JSON Content-Type：decodeJSON
// 不查 CT，跨站表单可以 text/plain 携带 JSON 体（无需预检的 CSRF 面）。
func TestLoginContentType(t *testing.T) {
	s, _ := newTestServer(t)
	req := httptest.NewRequest("POST", "/api/login", strings.NewReader(`{"user":"admin","password":"passw0rd"}`))
	req.Header.Set("Content-Type", "text/plain")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("非 JSON CT 应 415: %d", rec.Code)
	}
}

// TestLoginLockout 连续失败 5 次锁定 5 分钟：锁定期间即使凭据正确也 429。
func TestLoginLockout(t *testing.T) {
	s, _ := newTestServer(t)
	h := s.Handler()
	for i := 0; i < loginMaxFails; i++ {
		if rec := do(t, h, "POST", "/api/login", map[string]string{"user": "admin", "password": "wrong"}, nil); rec.Code != http.StatusUnauthorized {
			t.Fatalf("第 %d 次失败应 401: %d", i+1, rec.Code)
		}
	}
	if rec := do(t, h, "POST", "/api/login", map[string]string{"user": "admin", "password": "passw0rd"}, nil); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("锁定期间正确密码也应 429: %d", rec.Code)
	}
}

// TestLoginLockoutReset 成功登录清零失败计数（不会累计跨成功登录的失败）。
func TestLoginLockoutReset(t *testing.T) {
	s, _ := newTestServer(t)
	h := s.Handler()
	for i := 0; i < loginMaxFails-1; i++ {
		do(t, h, "POST", "/api/login", map[string]string{"user": "admin", "password": "wrong"}, nil)
	}
	if rec := do(t, h, "POST", "/api/login", map[string]string{"user": "admin", "password": "passw0rd"}, nil); rec.Code != http.StatusOK {
		t.Fatalf("未达阈值前正确密码应可登录: %d", rec.Code)
	}
	for i := 0; i < loginMaxFails-1; i++ {
		do(t, h, "POST", "/api/login", map[string]string{"user": "admin", "password": "wrong"}, nil)
	}
	if rec := do(t, h, "POST", "/api/login", map[string]string{"user": "admin", "password": "passw0rd"}, nil); rec.Code != http.StatusOK {
		t.Fatalf("成功登录后计数应清零: %d", rec.Code)
	}
}

// TestSessionSweep 过期条目由周期清扫兜底删除（浏览器丢 cookie 后无人
// lookup，只靠命中删除会慢性泄漏）。
func TestSessionSweep(t *testing.T) {
	tbl := sessionTable{sessns: map[string]session{}}
	stale, err := tbl.issue("u1")
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := tbl.issue("u2")
	if err != nil {
		t.Fatal(err)
	}
	tbl.mu.Lock()
	tbl.sessns[stale] = session{user: "u1", exp: time.Now().Add(-time.Minute)}
	tbl.mu.Unlock()

	tbl.sweep()

	tbl.mu.Lock()
	_, hasStale := tbl.sessns[stale]
	_, hasFresh := tbl.sessns[fresh]
	tbl.mu.Unlock()
	if hasStale {
		t.Fatal("过期条目应被清扫")
	}
	if !hasFresh {
		t.Fatal("未过期条目应保留")
	}
}

// TestBasicAuthDisabled 禁用账号不能经 Basic Auth 访问（与登录端点同口径，
// 否则禁用只挡得住会话路径）。
func TestBasicAuthDisabled(t *testing.T) {
	s, st := newTestServer(t)
	h := s.Handler()
	hash, err := bcrypt.GenerateFromPassword([]byte("basicd-Pass1"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateUser("basicd", string(hash), "viewer"); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/api/hosts", nil)
	req.SetBasicAuth("basicd", "basicd-Pass1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("禁用前 Basic Auth 应可用: %d %s", rec.Code, rec.Body)
	}
	u, err := st.UserByName("basicd")
	if err != nil {
		t.Fatal(err)
	}
	dis := true
	if err := st.UpdateUser(u.ID, "", &dis); err != nil {
		t.Fatal(err)
	}
	req2 := httptest.NewRequest("GET", "/api/hosts", nil)
	req2.SetBasicAuth("basicd", "basicd-Pass1")
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusUnauthorized {
		t.Fatalf("禁用后 Basic Auth 应 401: %d %s", rec2.Code, rec2.Body)
	}
}

// TestExecTimeoutCap 单主机超时上限 3600（与前端一致），超限 400。
func TestExecTimeoutCap(t *testing.T) {
	s, st := newTestServer(t)
	h := s.Handler()
	token := loginSession(t, s)
	if _, err := st.CreateHost(&store.Host{Name: "cap-host", Address: "127.0.0.1", AgentPort: 1}); err != nil {
		t.Fatal(err)
	}
	if rec := do(t, h, "POST", "/api/exec", map[string]any{"host_ids": []int64{1}, "script": "echo x", "timeout_sec": 999999}, &token); rec.Code != http.StatusBadRequest {
		t.Fatalf("超上限 timeout 应 400: %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, h, "POST", "/api/exec", map[string]any{"host_ids": []int64{1}, "script": "echo x", "timeout_sec": maxExecTimeoutSec}, &token); rec.Code != http.StatusOK {
		t.Fatalf("恰为上限应接受: %d %s", rec.Code, rec.Body)
	}
}

// TestBootstrapAdminGenerated 首启无账号且未给密码时自动生成管理员。
func TestBootstrapAdminGenerated(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	// 输出打到测试进程 stdout 即可，不校验内容
	if _, err := New(st, Options{AdminUser: "root"}, nil); err != nil {
		t.Fatal(err)
	}
	u, err := st.UserByName("root")
	if err != nil || u.PasswordHash == "" {
		t.Fatalf("管理员应已创建: %+v %v", u, err)
	}
	if err := bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte("")); err == nil {
		t.Fatal("生成密码不应为空串")
	}
}

// TestAdminPassEnsure 显式配置的管理员密码：改值后重启即改密（旧密码失效）。
func TestAdminPassEnsure(t *testing.T) {
	_, st := newTestServer(t) // admin / passw0rd

	// 相同密码重启：幂等，不破坏
	s2, err := New(st, Options{AdminUser: "admin", AdminPass: "passw0rd"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rec := do(t, s2.Handler(), "POST", "/api/login", map[string]string{"user": "admin", "password": "passw0rd"}, nil); rec.Code != http.StatusOK {
		t.Fatalf("相同密码重启后登录应成功: %d", rec.Code)
	}

	// 换新密码重启：旧失效、新生效
	s3, err := New(st, Options{AdminUser: "admin", AdminPass: "new-pass-123"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rec := do(t, s3.Handler(), "POST", "/api/login", map[string]string{"user": "admin", "password": "passw0rd"}, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("旧密码应失效: %d", rec.Code)
	}
	if rec := do(t, s3.Handler(), "POST", "/api/login", map[string]string{"user": "admin", "password": "new-pass-123"}, nil); rec.Code != http.StatusOK {
		t.Fatalf("新密码应生效: %d", rec.Code)
	}
}

// TestIndexServed 内嵌单页可访问。源码树未构建前端（static/ 仅 .keep）时
// 返回 503 引导页；两种形态都必须自识别为 wdp console。
func TestIndexServed(t *testing.T) {
	s, _ := newTestServer(t)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	switch rec.Code {
	case http.StatusOK:
		if !strings.Contains(rec.Body.String(), "wdp console") {
			t.Fatalf("已构建首页异常: %s", rec.Body)
		}
	case http.StatusServiceUnavailable:
		if !strings.Contains(rec.Body.String(), "wdp console") || !strings.Contains(rec.Body.String(), "npm run build") {
			t.Fatalf("未构建引导页异常: %s", rec.Body)
		}
	default:
		t.Fatalf("首页状态异常: %d %s", rec.Code, rec.Body)
	}
}

// TestSPAFallback 前端为 history 路由：未知业务路径（/apps/3/edit 等）必须
// 回退 index.html（已构建）或 503 引导页（未构建）；/api/*、/enroll/* 下的
// 未知路径维持 404，API 客户端不能拿到 HTML。
func TestSPAFallback(t *testing.T) {
	s, _ := newTestServer(t)
	h := s.Handler()
	for _, p := range []string{"/hosts", "/apps/3/edit", "/runs"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", p, nil))
		switch rec.Code {
		case http.StatusOK:
			if !strings.Contains(rec.Body.String(), "wdp console") {
				t.Fatalf("%s 应回退单页: %s", p, rec.Body)
			}
		case http.StatusServiceUnavailable:
			if !strings.Contains(rec.Body.String(), "npm run build") {
				t.Fatalf("%s 应回退引导页: %s", p, rec.Body)
			}
		default:
			t.Fatalf("%s 状态异常: %d", p, rec.Code)
		}
	}
	for _, p := range []string{"/api/nonexistent", "/enroll/t/whatever"} {
		if rec := do(t, h, "GET", p, nil, nil); rec.Code != http.StatusNotFound {
			t.Fatalf("%s 应为 404: %d", p, rec.Code)
		}
	}
}
