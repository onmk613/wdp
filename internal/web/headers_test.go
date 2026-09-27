package web

// 浏览器侧纵深回归：
//   - 控制台可执行远程命令/删除主机，不得被任意站点 iframe 嵌套；
//   - 状态变更请求带外站 Origin 时必须拒绝（multipart 是跨站表单可发的
//     类型，只靠 Content-Type 挡不住 CSRF）。

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSecurityHeadersOnConsole(t *testing.T) {
	s, _ := newTestServer(t)
	rec := do(t, s.Handler(), "GET", "/", nil, nil)
	for k, want := range map[string]string{
		"X-Frame-Options":         "DENY",
		"X-Content-Type-Options":  "nosniff",
		"Referrer-Policy":         "same-origin",
		"Content-Security-Policy": "frame-ancestors 'none'",
	} {
		if got := rec.Header().Get(k); got != want {
			t.Fatalf("%s 应为 %q，实际 %q", k, want, got)
		}
	}
}

// TestHSTSHonorsTrustedProxyOnly HSTS 设置口径与 requestIsHTTPS 一致：
// X-Forwarded-Proto 只在直连对端为信任反代（回环/显式配置）时采信。
// 非信任对端伪造该头不得触发 HSTS——否则任一客户端可给明文部署钉上
// Strict-Transport-Security，把后续 http 流量错误锁死。
func TestHSTSHonorsTrustedProxyOnly(t *testing.T) {
	s, _ := newTestServer(t)
	h := s.Handler()

	get := func(remote, proto string) string {
		req := httptest.NewRequest("GET", "/", nil)
		req.RemoteAddr = remote
		if proto != "" {
			req.Header.Set("X-Forwarded-Proto", proto)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Header().Get("Strict-Transport-Security")
	}

	// 直连客户端伪造 XFP：不设 HSTS（httptest 缺省 RemoteAddr 192.0.2.1
	// 为非信任对端，这里再显式给一个公网地址）
	if got := get("203.0.113.9:4444", "https"); got != "" {
		t.Fatalf("非信任对端伪造 X-Forwarded-Proto 不得设置 HSTS: %q", got)
	}
	// 明文直连（无 XFP）：不设
	if got := get("203.0.113.9:4444", ""); got != "" {
		t.Fatalf("明文直连不应设置 HSTS: %q", got)
	}
	// 回环反代（本机 nginx 拓扑）+ XFP https：设置
	if got := get("127.0.0.1:5555", "https"); got != "max-age=31536000" {
		t.Fatalf("信任反代 XFP=https 应设置 HSTS: %q", got)
	}
	// 回环反代但 XFP=http（反代明文回源）：不设
	if got := get("127.0.0.1:5555", "http"); got != "" {
		t.Fatalf("信任反代 XFP=http 不应设置 HSTS: %q", got)
	}
}

func TestCrossOriginWriteRejected(t *testing.T) {
	s, _ := newTestServer(t)
	h := s.Handler()
	token := loginSession(t, s)

	// 同源（无 Origin）→ 正常
	if rec := do(t, h, "POST", "/api/hosts", map[string]any{"name": "ok1", "address": "10.0.0.1"}, &token); rec.Code != http.StatusCreated {
		t.Fatalf("无 Origin 的写请求应放行: %d %s", rec.Code, rec.Body)
	}

	// 外站 Origin → 拒绝（即便带上了合法会话 cookie）
	req := httptest.NewRequest("POST", "/api/hosts", strings.NewReader(`{"name":"evil","address":"10.0.0.2"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://evil.example")
	addCookie(req, token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("跨站写请求应 403: %d %s", rec.Code, rec.Body)
	}

	// 同源 Origin → 放行
	req2 := httptest.NewRequest("POST", "/api/hosts", strings.NewReader(`{"name":"ok2","address":"10.0.0.3"}`))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("Origin", "http://"+req2.Host)
	addCookie(req2, token)
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusCreated {
		t.Fatalf("同源写请求应放行: %d %s", rec2.Code, rec2.Body)
	}
}
