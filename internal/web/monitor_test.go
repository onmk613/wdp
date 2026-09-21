package web

// 监控的 HTTP 面：basic auth 抓取代理、趋势查询的作用域校验。
// 解析与采样的行为测试在 internal/worker（随实现迁移）。

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"wdp/internal/store"
)

func TestBasicAuthAndProxy404(t *testing.T) {
	s, st := newTestServer(t)
	h := s.Handler()
	// 无凭据 → 401 带 WWW-Authenticate
	rec := do(t, h, "GET", "/api/hosts/1/metrics", nil, nil)
	if rec.Code != http.StatusUnauthorized || rec.Header().Get("WWW-Authenticate") == "" {
		t.Fatalf("应 401+challenge: %d", rec.Code)
	}
	// basic auth 错误密码 → 401
	req := authedRequest(t, "admin", "wrong", "/api/hosts/1/metrics")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("错密码应 401: %d", rec.Code)
	}
	// basic auth 正确 → 404（主机不存在）
	if _, err := st.CreateHost(&store.Host{Name: "ba", Address: "127.0.0.1", AgentPort: 1}); err != nil {
		t.Fatal(err)
	}
	rec2 := httptest.NewRecorder()
	req = authedRequest(t, "admin", "passw0rd", "/api/hosts/9/metrics")
	h.ServeHTTP(rec2, req)
	if rec2.Code != http.StatusNotFound {
		t.Fatalf("basic auth 应通过（404=主机不存在）: %d %s", rec2.Code, rec2.Body)
	}
	// basic auth 不适用于变更端点（POST login 无 cookie 不受影响）
}

func authedRequest(t *testing.T, user, pass, url string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, url, nil)
	req.SetBasicAuth(user, pass)
	return req
}

// TestHostSeriesScope 趋势查询按主机作用域校验（越权 403）；from 早于
// 聚合保留期被钳制（查询本身仍成功）。
func TestHostSeriesScope(t *testing.T) {
	s, st := newTestServer(t)
	h := s.Handler()
	admin := loginSession(t, s)
	ida, err := st.CreateHost(&store.Host{Name: "ser-a", Address: "127.0.0.1", AgentPort: 1, Pools: []string{"pool-a"}})
	if err != nil {
		t.Fatal(err)
	}
	idb, err := st.CreateHost(&store.Host{Name: "ser-b", Address: "127.0.0.1", AgentPort: 1, Pools: []string{"pool-b"}})
	if err != nil {
		t.Fatal(err)
	}
	scoped := newUserSession(t, h, admin, "serscoped", "scoped-Pass1", "viewer")
	if rec := do(t, h, "PUT", fmt.Sprintf("/api/users/%d/scopes", userIDByName(t, st, "serscoped")), map[string]any{
		"scopes": []map[string]any{{"verb": "host:view", "kind": "pool", "value": "pool-a"}},
	}, &admin); rec.Code != http.StatusOK {
		t.Fatalf("追加授权应 200: %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, h, "GET", fmt.Sprintf("/api/hosts/%d/series?metric=cpu_usage_pct", idb), nil, &scoped); rec.Code != http.StatusForbidden {
		t.Fatalf("范围外趋势查询应 403: %d", rec.Code)
	}
	if rec := do(t, h, "GET", fmt.Sprintf("/api/hosts/%d/series?metric=cpu_usage_pct&from=1", ida), nil, &scoped); rec.Code != http.StatusOK {
		t.Fatalf("范围内查询（from 超早被钳制）应 200: %d %s", rec.Code, rec.Body)
	}
}
