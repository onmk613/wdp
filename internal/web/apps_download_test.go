package web

// 下载端点与校验增强（行号透出 / 底本非最新 WARN）测试。

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"wdp/internal/store"
)

// TestDownloadChart 下载版本制品：默认最新 + 指定版本 + 404。
func TestDownloadChart(t *testing.T) {
	s, _ := newTestServer(t)
	h := s.Handler()
	token := loginSession(t, s)

	var app store.App
	if rec := doJSON(t, h, "POST", "/api/apps/spec", ideSpecBody("dlapp", "1.0.0",
		"name: dlapp\nversion: 1.0.0\n", "{}\n", ideDeployOK), &token, &app); rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	// 追加一版（默认下载应为最新）
	part := map[string]any{
		"version":      "1.0.1",
		"base_version": "1.0.0",
		"files": []map[string]any{
			{"path": "chart.yaml", "content": "name: dlapp\nversion: 1.0.1\n"},
			{"path": "deploy.yaml", "content": ideDeployOK},
		},
	}
	if rec := do(t, h, "PUT", "/api/apps/1/spec", part, &token); rec.Code != http.StatusOK {
		t.Fatalf("save v2: %d %s", rec.Code, rec.Body)
	}

	get := func(url string) (*httptest.ResponseRecorder, []byte) {
		t.Helper()
		rec := do(t, h, "GET", url, nil, &token)
		return rec, rec.Body.Bytes()
	}
	// 默认最新
	rec, body := get("/api/apps/1/download")
	if rec.Code != http.StatusOK {
		t.Fatalf("download: %d %s", rec.Code, rec.Body)
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, `filename="dlapp-1.0.1.tgz"`) {
		t.Fatalf("Content-Disposition 应带版本文件名: %q", cd)
	}
	if !bytes.HasPrefix(body, []byte{0x1f, 0x8b}) {
		t.Fatalf("响应体应为 gzip（tgz 魔数）: %x", body[:4])
	}
	// 指定版本
	rec, _ = get("/api/apps/1/download?version=1.0.0")
	if rec.Code != http.StatusOK {
		t.Fatalf("指定版本下载应 200: %d", rec.Code)
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "dlapp-1.0.0.tgz") {
		t.Fatalf("指定版本的文件名应带该版本号: %q", cd)
	}
	// 不存在的版本 → 404
	rec, _ = get("/api/apps/1/download?version=9.9.9")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("不存在版本应 404: %d", rec.Code)
	}
	// 下载进审计
	var logs []store.AuditLog
	if rec := doJSON(t, h, "GET", "/api/audit?limit=50", nil, &token, &logs); rec.Code == http.StatusOK {
		found := false
		for _, l := range logs {
			if l.Action == "download" && l.Name == "dlapp" {
				found = true
			}
		}
		if !found {
			t.Fatalf("下载应进审计日志")
		}
	}
}

// TestValidateIssueLine lint 行号透出：未知模块的 ERROR 带非零 line。
func TestValidateIssueLine(t *testing.T) {
	s, _ := newTestServer(t)
	h := s.Handler()
	token := loginSession(t, s)
	// deploy 第 4 行是坏模块（- nosuchmod）
	deploy := "- name: p\n  hosts: all\n  tasks:\n    - nosuchmod: x\n"
	var resp issuesResp
	doJSON(t, h, "POST", "/api/apps/validate",
		ideSpecBody("lapp", "1.0.0", "name: lapp\nversion: 1.0.0\n", "{}\n", deploy), &token, &resp)
	found := false
	for _, is := range resp.Issues {
		if is.Level == "ERROR" && strings.Contains(is.Msg, "unknown module") {
			found = true
			if is.Line != 4 {
				t.Fatalf("未知模块 ERROR 应带行号 4，实际 %d: %+v", is.Line, is)
			}
		}
	}
	if !found {
		t.Fatalf("应检出未知模块: %+v", resp.Issues)
	}
}

// TestValidateBaseNotLatest 编辑模式底本非最新 → WARN。
func TestValidateBaseNotLatest(t *testing.T) {
	s, _ := newTestServer(t)
	h := s.Handler()
	token := loginSession(t, s)

	var app store.App
	if rec := doJSON(t, h, "POST", "/api/apps/spec", ideSpecBody("blapp", "1.0.0",
		"name: blapp\nversion: 1.0.0\n", "{}\n", ideDeployOK), &token, &app); rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	part := map[string]any{
		"version":      "1.0.1",
		"base_version": "1.0.0",
		"files": []map[string]any{
			{"path": "chart.yaml", "content": "name: blapp\nversion: 1.0.1\n"},
			{"path": "deploy.yaml", "content": ideDeployOK},
		},
	}
	if rec := do(t, h, "PUT", "/api/apps/1/spec", part, &token); rec.Code != http.StatusOK {
		t.Fatalf("save v2: %d %s", rec.Code, rec.Body)
	}

	// 基于旧底本 1.0.0 校验 → WARN
	v := map[string]any{
		"app_id":       app.ID,
		"version":      "1.0.2",
		"base_version": "1.0.0",
		"files": []map[string]any{
			{"path": "chart.yaml", "content": "name: blapp\nversion: 1.0.2\n"},
			{"path": "deploy.yaml", "content": ideDeployOK},
		},
	}
	var resp issuesResp
	doJSON(t, h, "POST", "/api/apps/validate", v, &token, &resp)
	found := false
	for _, is := range resp.Issues {
		if is.Level == "WARN" && strings.Contains(is.Msg, "不是最新版本") {
			found = true
		}
	}
	if !found {
		t.Fatalf("旧底本校验应出「底本非最新」WARN: %+v", resp.Issues)
	}

	// 基于最新底本 → 无该 WARN
	v["base_version"] = "1.0.1"
	resp = issuesResp{}
	doJSON(t, h, "POST", "/api/apps/validate", v, &token, &resp)
	for _, is := range resp.Issues {
		if is.Level == "WARN" && strings.Contains(is.Msg, "不是最新版本") {
			t.Fatalf("最新底本不应出该 WARN: %+v", resp.Issues)
		}
	}
}
