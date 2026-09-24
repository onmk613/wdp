package web

// 应用级执行授权回归：run:execute 是可作用域化的权限点，"有该 verb"不等于
// "能跑这个应用"。此前 handleRunApps 只做主机侧交集，run:execute@poolA 的
// 账号可以把任意应用（含 scope 属 poolB 的）部署到 poolA 主机上，并能借
// 报错文案逐 ID 枚举应用名。

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// uploadChartAs 以给定会话上传一个 chart，返回应用 ID。
func uploadChartAs(t *testing.T, h http.Handler, cookie string, name, version string) int64 {
	t.Helper()
	body := &bytes.Buffer{}
	mw := newMultipartWriter(body, nil, "tgz", name+".tgz", buildChartTgz(t, name, version, "from-"+version))
	req := httptest.NewRequest("POST", "/api/apps/upload", body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	addCookie(req, cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("上传 %s 应 201: %d %s", name, rec.Code, rec.Body)
	}
	var app struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &app); err != nil || app.ID == 0 {
		t.Fatalf("解析应用 ID 失败: %v %s", err, rec.Body)
	}
	return app.ID
}

// TestRunAppsRequiresAppScope 范围外应用必须 403（而不是照跑）。
func TestRunAppsRequiresAppScope(t *testing.T) {
	s, st := newAppServer(t, 18790) // 台账里已有 e2e-local（e2e-pool）
	h := s.Handler()
	admin := loginSession2(t, s, "e2e-pass-1")

	// 两个应用：一个归属执行者被授权的池，一个归属别的池
	inScope := uploadChartAs(t, h, admin, "inscope", "1.0.0")
	outScope := uploadChartAs(t, h, admin, "outscope", "1.0.0")
	// 应用作用域走 PUT /api/apps/{id}/scope（BatchAssign 是主机台账的接口）
	if rec := do(t, h, "PUT", fmt.Sprintf("/api/apps/%d/scope", inScope), map[string]any{
		"pools": []string{"e2e-pool"},
	}, &admin); rec.Code != http.StatusOK {
		t.Fatalf("设置范围内应用 scope 应 200: %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, h, "PUT", fmt.Sprintf("/api/apps/%d/scope", outScope), map[string]any{
		"pools": []string{"other-pool"},
	}, &admin); rec.Code != http.StatusOK {
		t.Fatalf("设置范围外应用 scope 应 200: %d %s", rec.Code, rec.Body)
	}
	_ = st

	// 执行者：run:execute 仅限 e2e-pool
	scoped := newUserSession(t, h, admin, "appsc", "scoped-Pass1", "operator")
	do(t, h, "PUT", fmt.Sprintf("/api/users/%d/scopes", userIDByName(t, st, "appsc")), map[string]any{
		"scopes": []map[string]any{{"verb": "run:execute", "kind": "pool", "value": "e2e-pool"}},
	}, &admin)

	// 范围外应用 → 403，且文案不泄露应用名
	rec := do(t, h, "POST", "/api/runs", map[string]any{
		"items":    []map[string]any{{"app_id": outScope, "version": "1.0.0"}},
		"selector": map[string]any{"pools": []string{"e2e-pool"}},
	}, &scoped)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("范围外应用应 403: %d %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "outscope") {
		t.Fatalf("拒绝文案不应回显应用名（可被用来枚举）: %s", rec.Body)
	}

	// 范围内应用 → 放行（避免误杀）
	if rec := do(t, h, "POST", "/api/runs", map[string]any{
		"items":    []map[string]any{{"app_id": inScope, "version": "1.0.0"}},
		"selector": map[string]any{"pools": []string{"e2e-pool"}},
	}, &scoped); rec.Code != http.StatusOK && rec.Code != http.StatusAccepted {
		t.Fatalf("范围内应用应放行: %d %s", rec.Code, rec.Body)
	}
}
