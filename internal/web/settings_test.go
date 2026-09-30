package web

// 设置页端点与运行时生效路径的测试：admin 门禁、文档校验、乐观锁、
// 明文纳管开关即时生效、数据库搬迁（VACUUM INTO + ResolveStore 切换
// + 停机终态同步）。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// putSettings PUT 辅助：返回状态码与响应体。
func putSettings(t *testing.T, h http.Handler, token string, body map[string]any) (int, map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest("PUT", "/api/settings", strings.NewReader(string(raw)))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// getSettings GET 辅助。
func getSettings(t *testing.T, h http.Handler, token string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest("GET", "/api/settings", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// dummyHash 测试用 bcrypt 占位（不可登录，仅占行）。
const dummyHash = "$2a$04$abcdefghijabcdefghijabcdefghijabcdefghijabcdefghijabcdefghij"

// TestSettingsAdminOnly 设置端点 admin 专属：未登录 401、非 admin 403。
func TestSettingsAdminOnly(t *testing.T) {
	s, _ := newEnrollServer(t)
	h := s.Handler()
	admin := loginSession(t, s)

	if err := s.st.CreateUser("viewer1", dummyHash, "viewer"); err != nil {
		t.Fatal(err)
	}
	tok, err := s.sessions.issue("viewer1")
	if err != nil {
		t.Fatal(err)
	}

	if rec := do(t, h, "GET", "/api/settings", nil, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("未登录应 401: %d", rec.Code)
	}
	req, _ := http.NewRequest("GET", "/api/settings", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: tok})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("viewer 应 403: %d", rec.Code)
	}
	if code, _ := getSettings(t, h, admin); code != http.StatusOK {
		t.Fatalf("admin 应 200: %d", code)
	}
}

// TestSettingsPutValidateAndLive PUT 校验 + 即时生效（明文纳管开关翻转
// 立刻改变 enroll 闸门行为）。
func TestSettingsPutValidateAndLive(t *testing.T) {
	s, _ := newEnrollServer(t) // flag AllowPlaintextEnroll=true
	h := s.Handler()
	token := loginSession(t, s)

	// 非法值拒绝
	if code, _ := putSettings(t, h, token, map[string]any{
		"settings": map[string]any{"probe_every_sec": 1}, "version": 0,
	}); code != http.StatusBadRequest {
		t.Fatalf("probe 1s 应 400: %d", code)
	}
	if code, _ := putSettings(t, h, token, map[string]any{
		"settings": map[string]any{"alert_crit_pct": 80, "alert_warn_pct": 90}, "version": 0,
	}); code != http.StatusBadRequest {
		t.Fatalf("crit<warn 应 400: %d", code)
	}

	// 合法保存：关掉明文纳管（设置显式 false 压过 flag true）
	if code, _ := putSettings(t, h, token, map[string]any{
		"settings": map[string]any{"allow_plaintext_enroll": false}, "version": 0,
	}); code != http.StatusOK {
		t.Fatalf("保存应 200: %d", code)
	}
	if s.allowPlaintextEnroll() {
		t.Fatal("设置 false 应立即压过 flag true")
	}
	if rec := do(t, h, "POST", "/api/enroll-tokens", map[string]any{"host": "x"}, &token); rec.Code != http.StatusBadRequest {
		t.Fatalf("明文纳管应被拒: %d %s", rec.Code, rec.Body)
	}

	// 再打开：恢复放行
	if code, _ := putSettings(t, h, token, map[string]any{
		"settings": map[string]any{"allow_plaintext_enroll": true}, "version": 1,
	}); code != http.StatusOK {
		t.Fatalf("重新打开失败: %d", code)
	}
	if !s.allowPlaintextEnroll() {
		t.Fatal("重新打开应生效")
	}
}

// TestSettingsOptimisticLock 版本乐观锁与局部保存合并。
func TestSettingsOptimisticLock(t *testing.T) {
	s, _ := newEnrollServer(t)
	h := s.Handler()
	token := loginSession(t, s)

	if code, _ := putSettings(t, h, token, map[string]any{
		"settings": map[string]any{"probe_every_sec": 60}, "version": 0,
	}); code != http.StatusOK {
		t.Fatal("首保存应成功")
	}
	if code, out := putSettings(t, h, token, map[string]any{
		"settings": map[string]any{"probe_every_sec": 90}, "version": 0,
	}); code != http.StatusConflict {
		t.Fatalf("过期版本应 409: %d %v", code, out)
	}
	// 局部保存合并：不带 probe_every_sec 不清掉它
	if code, _ := putSettings(t, h, token, map[string]any{
		"settings": map[string]any{"session_ttl_min": 30}, "version": 1,
	}); code != http.StatusOK {
		t.Fatalf("局部保存失败: %d", code)
	}
	if d := s.effectiveSettings(); d.ProbeEverySec == nil || *d.ProbeEverySec != 60 {
		t.Fatalf("合并应保留 probe_every_sec=60: %+v", d)
	}
	if s.probeEvery() != 60*time.Second {
		t.Fatalf("probeEvery 应 60s: %v", s.probeEvery())
	}
	if s.sessions.idleTTL() != 30*time.Minute {
		t.Fatalf("会话窗口应 30m: %v", s.sessions.idleTTL())
	}
}

// TestSettingsUnknownFieldsIgnored 未知字段（如历史版本的 db_path）被
// 校验层忽略——文档演进不破老库。
func TestSettingsUnknownFieldsIgnored(t *testing.T) {
	s, _ := newEnrollServer(t)
	h := s.Handler()
	token := loginSession(t, s)

	// 直接落一份带未知字段的文档，再走正常保存
	if _, err := s.st.SaveSettings(`{"db_path":"/tmp/should-be-ignored.db","probe_every_sec":120}`, -1, "seed"); err != nil {
		t.Fatal(err)
	}
	// 注意不拿 s.settings.mu：PUT handler 内部会拿同一把锁（非重入）
	if err := s.loadSettingsInto(); err != nil {
		t.Fatal(err)
	}
	if d := s.effectiveSettings(); d.ProbeEverySec == nil || *d.ProbeEverySec != 120 {
		t.Fatalf("已知字段应加载: %+v", d)
	}
	// 保存（version=1，seed 写入的版本）不带 db_path，文档里也不应残留
	if code, _ := putSettings(t, h, token, map[string]any{
		"settings": map[string]any{"session_ttl_min": 45}, "version": 1,
	}); code != http.StatusOK {
		t.Fatalf("保存应 200: %d", code)
	}
	row, err := s.st.LoadSettings()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(row.Data, "db_path") {
		t.Fatalf("未知字段不应回写: %s", row.Data)
	}
}
