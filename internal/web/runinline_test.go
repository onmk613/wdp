package web

// 应用执行的自定义目标（selector.kind=inline）：受理与校验、selector 只记
// 主机名（inventory 文本/凭据不落库）、执行收敛到 unreachable/failed。

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestRunInlineValidation inline 选择器的受理校验。
func TestRunInlineValidation(t *testing.T) {
	s, _ := newTestServer(t)
	h := s.Handler()
	token := loginSession(t, s)
	post := func(inv string) *httptest.ResponseRecorder {
		t.Helper()
		return do(t, h, "POST", "/api/runs", map[string]any{
			"items":    []map[string]any{{"app_id": 1}},
			"selector": map[string]any{"kind": "inline", "inventory": inv},
		}, &token)
	}
	if rec := post(""); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "inventory") {
		t.Fatalf("空 inventory 应 400: %d %s", rec.Code, rec.Body)
	}
	if rec := post(":::not yaml"); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "解析失败") {
		t.Fatalf("坏 YAML 应 400: %d %s", rec.Code, rec.Body)
	}
	// 无 hosts 段：解析成功但零主机 → 400
	if rec := post("all: {vars: {x: 1}}"); rec.Code != http.StatusBadRequest {
		t.Fatalf("零主机应 400: %d %s", rec.Code, rec.Body)
	}
}

// TestRunInlineAcceptedSubmit 合法 inline 提交：run 建立、selector 记主机名
// （不含 inventory 原文），执行异步收敛为 failed（目标 ssh 不可达）。
func TestRunInlineAcceptedSubmit(t *testing.T) {
	s, st := newTestServer(t)
	h := s.Handler()
	token := loginSession(t, s)

	// 应用 1 存在即可（runAppsItems 校验版本）——造一个最小 chart 应用
	uploadMinimalApp(t, s, h, &token, "inline-demo")

	inv := "webservers:\n  hosts:\n    ghost1: {host: 127.0.0.1, conn: ssh, port: 1, connect_timeout: 1, password: should-not-persist}\n"
	rec := do(t, h, "POST", "/api/runs", map[string]any{
		"items":    []map[string]any{{"app_id": 1}},
		"selector": map[string]any{"kind": "inline", "inventory": inv},
	}, &token)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("inline 提交应 202: %d %s", rec.Code, rec.Body)
	}
	var resp struct {
		RunIDs []int64 `json:"run_ids"`
		Hosts  int     `json:"hosts"`
	}
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Hosts != 1 || len(resp.RunIDs) != 1 {
		t.Fatalf("应提交 1 台 1 run: %s", rec.Body)
	}
	run := waitExecRun(t, st, resp.RunIDs[0])
	if run.Status != "failed" {
		t.Fatalf("不可达目标应 failed: %+v", run)
	}
	// selector 只记主机名，inventory 原文（含 password）不得落库
	if strings.Contains(run.Selector, "password") || strings.Contains(run.Selector, "127.0.0.1") {
		t.Fatalf("selector 不得携带 inventory 原文: %s", run.Selector)
	}
	if !strings.Contains(run.Selector, `"ghost1"`) {
		t.Fatalf("selector 应记主机名: %s", run.Selector)
	}
}

// uploadMinimalApp 上传一个可执行（立即失败）的最小 chart 应用，app id = 1。
func uploadMinimalApp(t *testing.T, s *Server, h http.Handler, token *string, name string) {
	t.Helper()
	// 复用 apps_test 的 chart 打包与上传工具；应用落在 id=1（新库自增）
	body := &bytes.Buffer{}
	mw := newMultipartWriter(body, nil, "tgz", name+".tgz", buildChartTgz(t, name, "1.0.0", "echo inline-ok"))
	req := httptest.NewRequest("POST", "/api/apps/upload", body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	addCookie(req, *token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("上传应用应 201: %d %s", rec.Code, rec.Body)
	}
}
