package web

// run 取消注册表语义 + POST /api/runs/{id}/cancel 端点行为。

import (
	"context"
	"net/http"
	"strconv"
	"testing"
	"time"

	"wdp/internal/store"
)

// TestRunCancelRegistry 注册/武装/请求/注销的状态机语义。
func TestRunCancelRegistry(t *testing.T) {
	s := &Server{}
	s.runCancels = map[int64]*runCancelState{}

	s.registerRunCancels([]int64{7, 8})
	// 排队期请求取消：armed 时应拒绝启动
	if !s.requestRunCancel(7) {
		t.Fatal("已注册 run 应可请求取消")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if s.armRunCancel(7, cancel) {
		t.Fatal("排队期已请求取消的 run 不应再启动")
	}
	// 正常路径：武装后请求取消要打到 cancel 句柄
	cancelled := make(chan struct{})
	go func() { <-ctx.Done(); close(cancelled) }()
	if !s.armRunCancel(8, cancel) {
		t.Fatal("未请求取消的 run 应正常武装")
	}
	if !s.requestRunCancel(8) {
		t.Fatal("武装后的 run 应可请求取消")
	}
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("请求取消应触发 cancel 句柄")
	}
	if !s.runCancelRequested(8) {
		t.Fatal("requested 标志应置位")
	}
	// 注销后请求取消返回 false（run 已终态）
	s.unregisterRunCancel(8)
	if s.requestRunCancel(8) {
		t.Fatal("已注销 run 不应再受理取消")
	}
}

// TestCancelRunEndpoint 端点状态门卫：终态 run 拒绝、未知 run 404/存储错误。
func TestCancelRunEndpoint(t *testing.T) {
	s, st := newTestServer(t)
	h := s.Handler()
	token := loginSession(t, s)

	// 造一条已结束的 run：取消应 409
	id, err := st.CreateRun(store.RunInput{
		Kind: "app", AppName: "demo", Version: "1.0.0", Status: "succeeded",
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = st.FinishRun(id, "succeeded", "ok")
	rec := do(t, h, "POST", "/api/runs/"+strconv.FormatInt(id, 10)+"/cancel", map[string]any{}, &token)
	if rec.Code != http.StatusConflict {
		t.Fatalf("终态 run 取消应 409: %d %s", rec.Code, rec.Body)
	}
	// 不存在的 run：404（GetRun 报 not found → writeStoreErr）
	rec = do(t, h, "POST", "/api/runs/99999/cancel", map[string]any{}, &token)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("未知 run 应 404: %d %s", rec.Code, rec.Body)
	}
}
