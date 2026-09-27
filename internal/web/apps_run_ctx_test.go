package web

// apps_run 的 ctx 生命周期回归：排队超时只封顶"等闸门"，不得封顶执行
// 本身。此前排队 ctx 直接传给 RunOneApp——多主机/多相位部署超过
// runQueueTimeout（30min）即被 ctx 过期拦腰打断，留下半完成态。

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"wdp/internal/store"
)

// uploadSlowChart 上传一个含 sleep 任务的 chart，返回应用。
func uploadSlowChart(t *testing.T, h http.Handler, cookie string, name, task string) *store.App {
	t.Helper()
	tasks := "- name: e2e\n  hosts: all\n  tasks:\n    - name: t\n      shell: '" + task + "'\n"
	body := &bytes.Buffer{}
	mw := newMultipartWriter(body, nil, "tgz", name+".tgz", buildChartTgzFull(t, name, "1.0.0", "", tasks, "{}"))
	req := httptest.NewRequest("POST", "/api/apps/upload", body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	addCookie(req, cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("上传 %s 应 201: %d %s", name, rec.Code, rec.Body)
	}
	var app store.App
	if err := json.Unmarshal(rec.Body.Bytes(), &app); err != nil || app.ID == 0 {
		t.Fatalf("解析应用失败: %v %s", err, rec.Body)
	}
	return &app
}

// startRun 发起执行并返回 run_ids。
func startRun(t *testing.T, h http.Handler, cookie string, appID int64) []int64 {
	t.Helper()
	rbody := `{"items":[{"app_id":` + fmt.Sprint(appID) + `}],"selector":{"kind":"pool","value":"e2e-pool"}}`
	req := httptest.NewRequest("POST", "/api/runs", strings.NewReader(rbody))
	req.Header.Set("Content-Type", "application/json")
	addCookie(req, cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("执行应 202: %d %s", rec.Code, rec.Body)
	}
	var started struct {
		RunIDs []int64 `json:"run_ids"`
	}
	json.Unmarshal(rec.Body.Bytes(), &started)
	if len(started.RunIDs) == 0 {
		t.Fatalf("run_ids 异常: %s", rec.Body)
	}
	return started.RunIDs
}

// waitRun 轮询 run 到终态。
func waitRun(t *testing.T, st *store.Store, id int64) *store.Run {
	t.Helper()
	var run *store.Run
	for i := 0; i < 150; i++ {
		run, _ = st.GetRun(id)
		if run != nil && run.Status != "running" && run.Status != "queued" {
			return run
		}
		time.Sleep(100 * time.Millisecond)
	}
	if run == nil {
		t.Fatal("run 未创建")
	}
	t.Fatalf("run 未在超时内到终态: %+v", run)
	return nil
}

// TestRunQueueTimeoutNotCappingExecution 收窄排队超时到 300ms、跑一个 2s
// 任务：闸门空闲时立即取得，执行换挂独立 ctx 后应照常成功（旧实现会在
// 300ms 处 ctx 过期、run 失败）。
func TestRunQueueTimeoutNotCappingExecution(t *testing.T) {
	old := runQueueTimeout
	runQueueTimeout = 300 * time.Millisecond
	t.Cleanup(func() { runQueueTimeout = old })

	s, st := newAppServer(t, 18773)
	h := s.Handler()
	admin := loginSession2(t, s, "e2e-pass-1")
	app := uploadSlowChart(t, h, admin, "slowapp", "sleep 2; echo slow-ok")

	run := waitRun(t, st, startRun(t, h, admin, app.ID)[0])
	if run.Status != "succeeded" {
		ts, _ := st.RunTasks(run.ID)
		for _, x := range ts {
			t.Logf("TASK %s/%s@%s status=%s detail=%s", x.Play, x.Task, x.Host, x.Status, x.Detail)
		}
		t.Fatalf("执行不应被排队超时打断: %+v", run)
	}
}

// TestRunQueueTimeoutFailsQueuedRun 闸门被占时排队超时把 run 置 failed
// （排队上限语义保持：不无限堆积）。
func TestRunQueueTimeoutFailsQueuedRun(t *testing.T) {
	old := runQueueTimeout
	runQueueTimeout = 300 * time.Millisecond
	t.Cleanup(func() { runQueueTimeout = old })

	s, st := newAppServer(t, 18774)
	h := s.Handler()
	admin := loginSession2(t, s, "e2e-pass-1")
	app := uploadSlowChart(t, h, admin, "gateapp", "echo ok")

	// 预占 e2e-local 的执行闸门：run 只能排队，300ms 后应判失败
	hostsAll, _ := st.ListHosts("")
	var hid int64
	for _, hh := range hostsAll {
		if hh.Name == "e2e-local" {
			hid = hh.ID
		}
	}
	release, ok := s.gate.TryAcquire([]int64{hid}, time.Second)
	if !ok {
		t.Fatal("预占闸门失败")
	}
	defer release()

	run := waitRun(t, st, startRun(t, h, admin, app.ID)[0])
	if run.Status != "failed" || !strings.Contains(run.Summary, "排队超时") {
		t.Fatalf("排队超时应置 failed: %+v", run)
	}
}
