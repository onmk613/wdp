package web

// run 取消注册表语义 + POST /api/runs/{id}/cancel 端点行为。

import (
	"context"
	"encoding/json"
	"fmt"
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

// TestCancelRunRequiresScope 取消越权回归：此前路由只要求全局 run:execute，
// handler 不做任何可见性判定——持作用域 run:execute 的用户可取消任意用户
// 对任意主机的在途 run（详情/exec 流都有校验，唯独取消漏掉）。
func TestCancelRunRequiresScope(t *testing.T) {
	s, st := newAppServer(t, 18851) // 台账里已有 e2e-local（e2e-pool）
	h := s.Handler()
	admin := loginSession2(t, s, "e2e-pass-1")

	// 长执行（在途中）：exec 落 run 后台推进
	rec := do(t, h, "POST", "/api/exec", map[string]any{
		"host_ids": []int64{1}, "script": "sleep 2", "timeout_sec": 10,
	}, &admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("exec 应 200: %d %s", rec.Code, rec.Body)
	}
	var resp struct {
		RunID int64 `json:"run_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp.RunID == 0 {
		t.Fatalf("应返回 run_id: %v %s", err, rec.Body)
	}

	// 作用域外执行者：run:execute 只授予不存在的池（可通过路由 verb 门，
	// 但 run 实际触达 e2e-pool 主机 → 必须拒取消）
	scoped := newUserSession(t, h, admin, "cxlother", "scoped-Pass1", "operator")
	do(t, h, "PUT", fmt.Sprintf("/api/users/%d/scopes", userIDByName(t, st, "cxlother")), map[string]any{
		"scopes": []map[string]any{{"verb": "run:execute", "kind": "pool", "value": "no-such-pool"}},
	}, &admin)
	rec = do(t, h, "POST", fmt.Sprintf("/api/runs/%d/cancel", resp.RunID), map[string]any{}, &scoped)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("作用域外取消应 403（越权取消回归）: %d %s", rec.Code, rec.Body)
	}

	// 全局权限（admin）不受影响：通过可见性门（exec 类 run 不注册取消
	// 句柄，得到 409"不在本进程"而非 403——本用例只锁越权门，注册表
	// 语义由 TestCancelRunEndpoint 覆盖）
	rec = do(t, h, "POST", fmt.Sprintf("/api/runs/%d/cancel", resp.RunID), map[string]any{}, &admin)
	if rec.Code == http.StatusForbidden {
		t.Fatalf("全局权限不应被可见性门拦截: %d %s", rec.Code, rec.Body)
	}
}
