package agentc

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"wdp/internal/model"
)

// TestLogsFetchOK /logs 拉取成功：正文原样返回。
func TestLogsFetchOK(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/logs" || r.Method != http.MethodGet {
			t.Errorf("应为 GET /logs，实际 %s %s", r.Method, r.URL.Path)
		}
		_, _ = w.Write([]byte("time=now level=INFO msg=exec\n"))
	}))
	t.Cleanup(ts.Close)
	conn := New(&model.Host{Name: "p", Conn: "agent", AgentURL: ts.URL}, nil)
	b, err := conn.Logs(context.Background())
	if err != nil {
		t.Fatalf("拉取应成功: %v", err)
	}
	if !strings.Contains(string(b), "level=INFO") {
		t.Fatalf("日志内容应原样返回: %q", b)
	}
}

// TestLogsFetchError 拉取失败（403）显式上抛错误详情。
func TestLogsFetchError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusForbidden)
	}))
	t.Cleanup(ts.Close)
	conn := New(&model.Host{Name: "p", Conn: "agent", AgentURL: ts.URL}, nil)
	_, err := conn.Logs(context.Background())
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("403 应显式报错，实际: %v", err)
	}
}

// TestShutdownAndCleanupError 远程清理被拒（403）时错误显式上抛；
// 请求体按新协议携带 system 名称与 files。
func TestShutdownAndCleanupError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil { // 消费 body（JSON）
			t.Fatal(err)
		}
		http.Error(w, "cleanup rejected", http.StatusForbidden)
	}))
	t.Cleanup(ts.Close)
	conn := New(&model.Host{Name: "p", Conn: "agent", AgentURL: ts.URL}, nil)
	err := conn.ShutdownAndCleanup(context.Background(), "my-unit", []string{"/tmp/x"})
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("403 应显式报错，实际: %v", err)
	}
}
