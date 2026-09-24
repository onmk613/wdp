package web

// 中间件不能吞掉可选能力：SSE 依赖 ResponseWriter 断言 http.Flusher。
// 这里直接对生产处理器链（Handler）断言，避免"测试直连裸 mux、
// 生产 500"的接线分叉再次发生。

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestHandlerPreservesFlusher Handler() 返回的完整链路上，包装后的
// ResponseWriter 仍须满足 http.Flusher（以及支持 ResponseController 穿透）。
func TestHandlerPreservesFlusher(t *testing.T) {
	s, _ := newTestServer(t)

	var flusherOK, unwrapOK bool
	probe := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, flusherOK = w.(http.Flusher)
		unwrapOK = w.(interface{ Unwrap() http.ResponseWriter }) != nil
	})
	s.metricsMiddleware(probe).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	if !flusherOK {
		t.Fatal("指标中间件必须保留 http.Flusher（否则 /api/runs/stream 生产环境恒 500）")
	}
	if !unwrapOK {
		t.Fatal("包装器应实现 Unwrap 供 http.ResponseController 穿透")
	}
}

// TestRunStreamThroughProductionChain SSE 端点经 Handler()（生产同款链路）
// 返回 200 与 event-stream 头，而不是 "streaming unsupported" 500。
func TestRunStreamThroughProductionChain(t *testing.T) {
	s, _ := newTestServer(t)
	token := loginSession(t, s)

	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/runs/stream", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("SSE 经生产链路应 200（曾因中间件吞掉 Flusher 恒 500）: %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream; charset=utf-8" {
		t.Fatalf("Content-Type 应为 event-stream: %q", ct)
	}
}
