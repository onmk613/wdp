package module

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newHTTPServer 起一个本地 HTTP 服务（固定内容 + 404 路径）。
func newHTTPServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/missing") {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		fmt.Fprint(w, body)
	}))
}

func TestGetURLDownloadAndIdempotent(t *testing.T) {
	const body = "artifact-bytes-v1"
	srv := newHTTPServer(t, body)
	defer srv.Close()

	rc, fake := newTestRC(t)
	mod := &GetURLModule{}
	sum := sha256hex([]byte(body))

	r1 := mod.Run(rc, map[string]any{"url": srv.URL + "/app.tgz", "dest": "/opt/app.tgz", "sha256": sum}, "")
	if r1.Failed {
		t.Fatalf("首次下载失败: %s", r1.Msg)
	}
	if !r1.Changed {
		t.Fatal("首次下载应 changed")
	}
	if got, _ := fake.File("/opt/app.tgz"); got != body {
		t.Fatalf("远端内容 %q", got)
	}

	// 幂等：远端 sha256 已匹配 → 跳过下载、不变更
	r2 := mod.Run(rc, map[string]any{"url": srv.URL + "/app.tgz", "dest": "/opt/app.tgz", "sha256": sum}, "")
	if r2.Failed {
		t.Fatalf("二次执行失败: %s", r2.Msg)
	}
	if r2.Changed {
		t.Fatal("sha256 一致时不应 changed")
	}

	// 内容漂移：远端无该内容 → 实际下载后校验失败、不落盘
	srv2 := newHTTPServer(t, "artifact-bytes-v2")
	defer srv2.Close()
	r3 := mod.Run(rc, map[string]any{"url": srv2.URL + "/app.tgz", "dest": "/opt/fresh.tgz", "sha256": sum}, "")
	if !r3.Failed || !strings.Contains(r3.Msg, "校验失败") {
		t.Fatalf("内容漂移应校验失败: %+v", r3)
	}
	if _, exists := fake.File("/opt/fresh.tgz"); exists {
		t.Fatal("校验失败时不应写远端")
	}
	if got, _ := fake.File("/opt/app.tgz"); got != body {
		t.Fatalf("原目标不应被覆盖: %q", got)
	}
}

func TestGetURLHeaderArg(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		fmt.Fprint(w, "ok")
	}))
	defer srv.Close()

	rc, fake := newTestRC(t)
	mod := &GetURLModule{}
	r := mod.Run(rc, map[string]any{
		"url":     srv.URL,
		"dest":    "/opt/secret",
		"headers": map[string]any{"Authorization": "Bearer tok-1"},
	}, "")
	if r.Failed {
		t.Fatalf("带 header 下载失败: %s", r.Msg)
	}
	if gotAuth != "Bearer tok-1" {
		t.Fatalf("请求头未生效: %q", gotAuth)
	}
	if got, _ := fake.File("/opt/secret"); got != "ok" {
		t.Fatalf("远端内容 %q", got)
	}
	// headers 类型非法
	if r := mod.Run(rc, map[string]any{"url": srv.URL, "dest": "/x", "headers": "nope"}, ""); !r.Failed {
		t.Fatal("headers 非 map 应失败")
	}
}

func TestGetURLHTTPStatusAndArgs(t *testing.T) {
	srv := newHTTPServer(t, "x")
	defer srv.Close()
	rc, _ := newTestRC(t)
	mod := &GetURLModule{}

	r := mod.Run(rc, map[string]any{"url": srv.URL + "/missing", "dest": "/opt/x"}, "")
	if !r.Failed || !strings.Contains(r.Msg, "404") {
		t.Fatalf("非 200 应失败: %+v", r)
	}
	if r := mod.Run(rc, map[string]any{"dest": "/x"}, ""); !r.Failed {
		t.Fatal("缺 url 应失败")
	}
	if r := mod.Run(rc, map[string]any{"url": srv.URL}, ""); !r.Failed {
		t.Fatal("缺 dest 应失败")
	}
	if r := mod.Run(rc, map[string]any{"url": srv.URL, "dest": "/x", "sha256": "zz"}, ""); !r.Failed {
		t.Fatal("非法 sha256 应失败")
	}
	if r := mod.Run(rc, map[string]any{"url": srv.URL, "dest": "/x", "timeout_secs": 0}, ""); !r.Failed {
		t.Fatal("非法 timeout_secs 应失败")
	}
}

func TestGetURLCheckMode(t *testing.T) {
	srv := newHTTPServer(t, "new-content")
	defer srv.Close()

	rc, fake := newTestRC(t)
	rc.CheckMode = true
	rc.DiffMode = true
	mod := &GetURLModule{}
	r := mod.Run(rc, map[string]any{"url": srv.URL + "/cfg", "dest": "/opt/cfg"}, "")
	if r.Failed {
		t.Fatalf("check 失败: %s", r.Msg)
	}
	if !r.Changed || !strings.Contains(r.Msg, "[check]") {
		t.Fatalf("check 应预估变更: %+v", r)
	}
	if r.Diff == "" {
		t.Fatal("diff 模式应产出内容差异")
	}
	if _, exists := fake.File("/opt/cfg"); exists {
		t.Fatal("check 模式不应写远端")
	}

	// 远端已有同内容 → check 结论为一致
	fake.Files["/opt/cfg"] = []byte("new-content")
	r = mod.Run(rc, map[string]any{"url": srv.URL + "/cfg", "dest": "/opt/cfg"}, "")
	if r.Failed || r.Changed {
		t.Fatalf("内容一致 check 不应变更: %+v", r)
	}
}
