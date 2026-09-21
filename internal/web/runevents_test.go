package web

// SSE 事件流测试：订阅后 notify 送达、慢消费者不阻塞执行路径、
// 端点鉴权与流格式。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestRunHubFanout notify 扇出到全部订阅者；慢消费者（不读的通道）不
// 阻塞 notify（缓冲满即丢）；退订后不再收到。
func TestRunHubFanout(t *testing.T) {
	h := newRunHub()
	ch1 := h.subscribe()
	ch2 := h.subscribe()

	h.notify(runEvent{ID: 1, Status: "running"})
	for _, ch := range []chan runEvent{ch1, ch2} {
		select {
		case e := <-ch:
			if e.ID != 1 || e.Status != "running" {
				t.Fatalf("事件载荷异常: %+v", e)
			}
		case <-time.After(time.Second):
			t.Fatal("订阅者应收到事件")
		}
	}

	// 慢消费者：填满缓冲后再 notify，必须立即返回（不阻塞执行路径）
	h.unsubscribe(ch2)
	h.notify(runEvent{ID: 2, Status: "running"}) // ch1 有空间
	for i := 0; i < runEventBuffer+5; i++ {
		h.notify(runEvent{ID: 3, Status: "running"}) // 全部落在 ch1 满缓冲上丢弃
	}
	select {
	case e := <-ch1:
		if e.ID == 3 {
			t.Fatal("缓冲已满的事件应被丢弃（有轮询兜底），但不应阻塞")
		}
	default:
	}
}

// TestRunStreamEndpoint SSE 端点：鉴权（匿名 401）、流头、开场注释、
// 事件送达格式。
func TestRunStreamEndpoint(t *testing.T) {
	s, _ := newTestServer(t)
	h := s.Handler()

	// 匿名 → 401
	if rec := do(t, h, "GET", "/api/runs/stream", nil, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("匿名 SSE 应 401: %d", rec.Code)
	}

	token := loginSession(t, s)
	// 流式响应：用 httptest + 手动消费 body
	req := httptest.NewRequest(http.MethodGet, "/api/runs/stream", nil)
	addCookie(req, token)
	rec := httptest.NewRecorder()
	ctx, cancel := context.WithCancel(context.Background())
	req = req.WithContext(ctx)
	done := make(chan struct{})
	go func() {
		h.ServeHTTP(rec, req)
		close(done)
	}()

	// 开场注释立即到达（不等首个事件）
	deadline := time.Now().Add(2 * time.Second)
	gotComment := false
	for !gotComment && time.Now().Before(deadline) {
		if strings.Contains(rec.Body.String(), ": connected") {
			gotComment = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream; charset=utf-8" {
		t.Fatalf("Content-Type 应为 event-stream: %q", ct)
	}
	if !gotComment {
		t.Fatal("开场注释应立即下发")
	}

	// 事件送达：notify 一条，body 应出现 event: run + JSON 载荷
	s.runs.notify(runEvent{ID: 42, Status: "succeeded", Summary: "ok"})
	for !strings.Contains(rec.Body.String(), "\"id\":42") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(rec.Body.String(), "event: run") {
		t.Fatalf("事件应带 event: run 前缀: %q", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "\"status\":\"succeeded\"") {
		t.Fatalf("事件载荷异常: %q", rec.Body.String())
	}

	// 客户端断开（ctx 取消）→ handler 返回、订阅清空
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("断开后 handler 应返回")
	}
	s.runs.mu.Lock()
	n := len(s.runs.subs)
	s.runs.mu.Unlock()
	if n != 0 {
		t.Fatalf("断开后订阅应清空: %d", n)
	}
}
