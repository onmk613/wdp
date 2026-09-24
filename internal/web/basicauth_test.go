package web

// Basic 质询头的回归：只发给非 JSON 调用方（Prometheus 等），页面内
// JSON API 的 401 绝不能带——浏览器 fetch 收到 401+WWW-Authenticate
// 会弹原生认证框（登录页刷新/退出后曾必现）。

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBasicChallengeOnlyForNonJSON(t *testing.T) {
	s, _ := newTestServer(t)
	h := s.Handler()

	get := func(accept string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/api/hosts", nil)
		if accept != "" {
			req.Header.Set("Accept", accept)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	// 无 Accept（curl/浏览器直访）与 Prometheus 文本格式：带质询头
	for _, accept := range []string{"", "text/plain;version=0.0.4", "text/html"} {
		rec := get(accept)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("未认证应 401: %d", rec.Code)
		}
		if got := rec.Header().Get("WWW-Authenticate"); got == "" {
			t.Errorf("Accept %q 的 401 应带 Basic 质询头", accept)
		}
	}
	// 页面内 JSON API：不带质询头（否则浏览器弹原生认证框）
	for _, accept := range []string{"application/json", "application/json, text/plain, */*"} {
		rec := get(accept)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("未认证应 401: %d", rec.Code)
		}
		if got := rec.Header().Get("WWW-Authenticate"); got != "" {
			t.Errorf("Accept %q 的 401 不应带质询头（浏览器会弹原生认证框）: %q", accept, got)
		}
	}
}

// TestBasicAuthRateLimited：Basic 路径失败同样计入登录限速——此前登录
// 端点有 5 次/5 分钟锁定而 Basic 路径无任何节流，公网暴露下任意 GET
// 都是无锁定、无审计的凭据爆破面。锁定期间正确凭据同样拒绝（与登录
// 端点行为一致）。
func TestBasicAuthRateLimited(t *testing.T) {
	s, _ := newTestServer(t)
	h := s.Handler()

	get := func(user, pass string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/api/me", nil)
		req.SetBasicAuth(user, pass)
		req.Header.Set("Accept", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	// 正确凭据可进
	if rec := get("admin", "passw0rd"); rec.Code != http.StatusOK {
		t.Fatalf("正确凭据应 200: %d %s", rec.Code, rec.Body)
	}
	// 连续失败达到阈值
	for i := 0; i < loginMaxFails; i++ {
		if rec := get("admin", "wrong"); rec.Code != http.StatusUnauthorized {
			t.Fatalf("错误凭据应 401: %d", rec.Code)
		}
	}
	// 锁定：正确凭据也被拒
	if rec := get("admin", "passw0rd"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("锁定期间正确凭据同样应 401: %d %s", rec.Code, rec.Body)
	}
}

// TestDecodeJSONTrailingRejected：JSON 体后跟多余数据应 400——此前
// dec.Decode 成功即放行，{"a":1}xxx 可通过校验。
func TestDecodeJSONTrailingRejected(t *testing.T) {
	s, _ := newTestServer(t)
	h := s.Handler()
	token := loginSession(t, s)

	req := httptest.NewRequest("POST", "/api/login", strings.NewReader(`{"user":"a","password":"b"}xxx`))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("尾随数据应 400: %d %s", rec.Code, rec.Body)
	}
}

// TestEmptyContentTypeRejected：变更类请求缺 Content-Type 应 415——
// 无体 POST（fetch 缺省不带 CT）可被无 CT 跨站请求触达，纵深防御不
// 留豁免口。
func TestEmptyContentTypeRejected(t *testing.T) {
	s, _ := newTestServer(t)
	h := s.Handler()
	token := loginSession(t, s)

	req := httptest.NewRequest("POST", "/api/logout", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("空 CT 的 POST 应 415: %d %s", rec.Code, rec.Body)
	}
}
