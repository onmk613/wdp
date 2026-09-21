package web

// Basic 质询头的回归：只发给非 JSON 调用方（Prometheus 等），页面内
// JSON API 的 401 绝不能带——浏览器 fetch 收到 401+WWW-Authenticate
// 会弹原生认证框（登录页刷新/退出后曾必现）。

import (
	"net/http"
	"net/http/httptest"
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
