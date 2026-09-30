package web

// 可用性改造的回归测试：启动对账（重启后悬空 run 收口）、应用级执行
// 准入（同应用并发 409 并反馈冲突方）、观测端点（metrics/pprof 的
// admin 门）。

import (
	"net/http"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"wdp/internal/store"
)

// TestReconcileStaleRunsOnStartup 启动对账：预置 queued/running 的 run
// （模拟上个进程崩溃残留），再走一次 New（同一 store = 重启）——悬空
// run 应全部 failed 且带原因；已终结的 run 不受影响。
func TestReconcileStaleRunsOnStartup(t *testing.T) {
	s, st := newTestServer(t)

	q, err := st.CreateRun(store.RunInput{Kind: "app", AppID: 1, AppName: "ghost", Version: "1.0.0", Phase: "deploy", Selector: "{}", User: "alice", Status: "queued"})
	if err != nil {
		t.Fatal(err)
	}
	rn, err := st.CreateRun(store.RunInput{Kind: "app", AppID: 1, AppName: "ghost", Version: "1.0.0", Phase: "deploy", Seq: 1, Selector: "{}", User: "bob", Status: "running"})
	if err != nil {
		t.Fatal(err)
	}
	done, err := st.CreateRun(store.RunInput{Kind: "exec", Selector: "{}", User: "carol", Status: "running"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.FinishRun(done, "succeeded", "ok"); err != nil {
		t.Fatal(err)
	}

	// 重启 = 同一 store 再构造一次 server（New 里跑对账）
	if _, err := New(st, s.opts, s.logger); err != nil {
		t.Fatal(err)
	}

	runs, err := st.ListRuns(10)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[int64]*store.Run{}
	for _, r := range runs {
		byID[r.ID] = r
	}
	for _, id := range []int64{q, rn} {
		got, ok := byID[id]
		if !ok {
			t.Fatalf("run %d 应存在", id)
		}
		if got.Status != "failed" || !strings.Contains(got.Summary, "重启") {
			t.Fatalf("悬空 run %d 应被对账为 failed+原因: %+v", id, got)
		}
		if got.FinishedAt == "" {
			t.Fatalf("悬空 run %d 应有收口时间", id)
		}
	}
	if got := byID[done]; got.Status != "succeeded" {
		t.Fatalf("已终结 run 不应被动: %+v", got)
	}
}

// TestRunAppAdmission 同应用并发准入：应用已有一条 running run 时，
// 再次发起应 409 且反馈冲突应用与相位；在途终结后放行。
func TestRunAppAdmission(t *testing.T) {
	s, st := newTestServer(t)
	h := s.Handler()
	token := loginSession(t, s)

	// 一台主机（准入检查在执行之前，无需真实可达）
	if _, err := st.CreateHost(&store.Host{Name: "adm-host", Address: "127.0.0.1", AgentPort: 1, Pools: []string{"adm-pool"}}); err != nil {
		t.Fatal(err)
	}

	var app store.App
	if rec := doJSON(t, h, "POST", "/api/apps/spec", ideSpecBody("admapp", "1.0.0",
		"name: admapp\nversion: 1.0.0\n", "{}\n", ideDeployOK), &token, &app); rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}

	// 预置在途执行（模拟：另一用户正跑这个应用）
	if _, err := st.CreateRun(store.RunInput{Kind: "app", AppID: app.ID, AppName: app.Name, Version: "1.0.0", Phase: "deploy", Selector: "{}", User: "alice", Status: "running"}); err != nil {
		t.Fatal(err)
	}

	body := map[string]any{
		"items":    []map[string]any{{"app_id": app.ID, "version": "1.0.0", "phase": "deploy"}},
		"selector": map[string]any{"kind": "pool", "value": "adm-pool"},
	}
	rec := do(t, h, "POST", "/api/runs", body, &token)
	if rec.Code != http.StatusConflict {
		t.Fatalf("同应用并发应 409: %d %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "admapp") || !strings.Contains(rec.Body.String(), "deploy") {
		t.Fatalf("409 应反馈冲突应用与相位: %s", rec.Body)
	}

	// 在途终结后：不再因准入拒绝（后续失败与否取决于主机可达性）
	if _, err := st.ReconcileStaleRuns("test cleanup"); err != nil {
		t.Fatal(err)
	}
	rec = do(t, h, "POST", "/api/runs", body, &token)
	if rec.Code == http.StatusConflict {
		t.Fatalf("在途 run 终结后不应再 409: %s", rec.Body)
	}
}

// TestObservabilityEndpoints metrics 与 pprof：admin 可见、viewer 403、
// 匿名 401；metrics 输出含请求计数器。
func TestObservabilityEndpoints(t *testing.T) {
	s, _ := newTestServer(t)
	h := s.Handler()
	admin := loginSession(t, s)

	hash, err := bcrypt.GenerateFromPassword([]byte("viewer-pass-1"), bcrypt.DefaultCost)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.st.CreateUser("viewer1", string(hash), "viewer"); err != nil {
		t.Fatal(err)
	}
	viewer := loginAs(t, h, "viewer1", "viewer-pass-1")

	// admin: 200 + 指标体
	if rec := do(t, h, "GET", "/metrics", nil, &admin); rec.Code != http.StatusOK ||
		!strings.Contains(rec.Body.String(), "wdp_http_requests_total") {
		t.Fatalf("admin /metrics 应 200+指标: %d %s", rec.Code, rec.Body)
	}
	// pprof 索引（GET；POST 侧同样挂门，抽一个验证）
	if rec := do(t, h, "GET", "/debug/pprof/", nil, &admin); rec.Code != http.StatusOK ||
		!strings.Contains(rec.Body.String(), "pprof") {
		t.Fatalf("admin pprof 索引应 200: %d %s", rec.Code, rec.Body)
	}
	// viewer: 403
	if rec := do(t, h, "GET", "/metrics", nil, &viewer); rec.Code != http.StatusForbidden {
		t.Fatalf("viewer /metrics 应 403: %d", rec.Code)
	}
	if rec := do(t, h, "GET", "/debug/pprof/", nil, &viewer); rec.Code != http.StatusForbidden {
		t.Fatalf("viewer pprof 应 403: %d", rec.Code)
	}
	// 匿名: 401
	if rec := do(t, h, "GET", "/metrics", nil, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("匿名 /metrics 应 401: %d", rec.Code)
	}
}
