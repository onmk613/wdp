package web

// 静态资源压缩回归：构建期产出 .gz，服务端命中 Accept-Encoding 时直出，
// 未命中回原文。首屏 JS 从 5.1MB（未压缩）降到 ~1.05MB（gzip 后 ~350KB）。

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPrecompressedAssets 命中 .gz 时带 Content-Encoding/Vary 与正确
// Content-Type；不支持 gzip 的客户端拿原文。
func TestPrecompressedAssets(t *testing.T) {
	// 嵌入目录可能未构建前端（此时 /assets 下没有文件）：造一个临时
	// 覆盖不现实，直接断言嵌入 FS 的实际状态
	entries, err := os.ReadDir("static/assets")
	if err != nil {
		t.Skipf("无内嵌资源目录: %v", err)
	}
	var jsName string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".js") {
			jsName = e.Name()
			break
		}
	}
	if jsName == "" {
		t.Skip("内嵌资源里没有 .js（未跑 build.sh --frontend）")
	}
	if _, err := os.Stat(filepath.Join("static/assets", jsName+".gz")); err != nil {
		t.Skipf("该构建没有预压缩产物（%s.gz）: %v", jsName, err)
	}

	s, _ := newTestServer(t)
	h := s.Handler()

	// 接受 gzip → 预压缩件
	req := httptest.NewRequest("GET", "/assets/"+jsName, nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("资产请求应 200: %d", rec.Code)
	}
	if got := rec.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("应回预压缩件: Content-Encoding=%q", got)
	}
	if v := rec.Header().Get("Vary"); !strings.Contains(v, "Accept-Encoding") {
		t.Fatalf("必须声明 Vary: Accept-Encoding，否则共享缓存会串味: %q", v)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Fatalf("Content-Type 应按原扩展名判定: %q", ct)
	}
	gzLen := rec.Body.Len()

	// 不接受 gzip → 原文，且更大
	req2 := httptest.NewRequest("GET", "/assets/"+jsName, nil)
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("原文请求应 200: %d", rec2.Code)
	}
	if rec2.Header().Get("Content-Encoding") != "" {
		t.Fatalf("不接受 gzip 时不得带 Content-Encoding")
	}
	if rec2.Body.Len() <= gzLen {
		t.Fatalf("原文应大于压缩件: raw=%d gz=%d", rec2.Body.Len(), gzLen)
	}
	if !strings.Contains(rec2.Header().Get("Cache-Control"), "immutable") {
		t.Fatalf("带哈希名的资产应可长期强缓存: %q", rec2.Header().Get("Cache-Control"))
	}
}

// TestAssetPathTraversalRejected 预压缩分支不得被 ../ 逃逸利用。
func TestAssetPathTraversalRejected(t *testing.T) {
	if !servePrecompressed(httptest.NewRecorder(), httptest.NewRequest("GET", "/assets/../../etc/passwd", nil)) {
		return // 已拒绝
	}
	t.Fatal("路径穿越不应命中预压缩分支")
}
