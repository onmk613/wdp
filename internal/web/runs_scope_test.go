package web

// run:view 作用域裁剪与 exec 执行证据的回归测试：
//   - 此前 runs 列表/详情只要求全局 run:view，run_tasks 的 stdout/stderr
//     常含凭据，是主机/应用列表作用域裁剪之外的旁路泄露面；
//   - exec 只记 selector/用户，事后无法回答"谁在哪台机执行了什么命令"。

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"wdp/internal/store"
)

// TestExecRunRecordsScript exec 的执行证据：截断快照 + 完整脚本 sha256
// 入库；详情端点带出，列表不回传（50 行 × 16 KiB 会撑爆列表响应）。
func TestExecRunRecordsScript(t *testing.T) {
	s, st := newAppServer(t, 18841)
	h := s.Handler()
	admin := loginSession2(t, s, "e2e-pass-1")

	rec := do(t, h, "POST", "/api/exec", map[string]any{
		"host_ids": []int64{1}, "script": "echo evidence-ok",
	}, &admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("exec 应 200: %d %s", rec.Code, rec.Body)
	}
	var resp struct {
		RunID int64 `json:"run_id"`
	}
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.RunID == 0 {
		t.Fatal("应返回 run_id")
	}

	run, err := st.GetRun(resp.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Script != "echo evidence-ok" {
		t.Fatalf("脚本快照应入库: %q", run.Script)
	}
	if want := fmt.Sprintf("%x", sha256.Sum256([]byte("echo evidence-ok"))); run.ScriptSHA != want {
		t.Fatalf("sha256 应对质完整脚本: %q want %q", run.ScriptSHA, want)
	}

	// 超长脚本：快照截断带尾注，哈希仍是完整原文的（截断不损害证据链）
	long := strings.Repeat("x", 20<<10)
	rec = do(t, h, "POST", "/api/exec", map[string]any{
		"host_ids": []int64{1}, "script": long,
	}, &admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("超长脚本 exec 应 200: %d %s", rec.Code, rec.Body)
	}
	json.Unmarshal(rec.Body.Bytes(), &resp)
	run2, err := st.GetRun(resp.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if len(run2.Script) > (16<<10)+len("\n... (truncated)") {
		t.Fatalf("快照应截断到 16KiB 档: %d", len(run2.Script))
	}
	if !strings.Contains(run2.Script, "(truncated)") {
		t.Fatalf("截断应带尾注: %q", run2.Script)
	}
	if want := fmt.Sprintf("%x", sha256.Sum256([]byte(long))); run2.ScriptSHA != want {
		t.Fatalf("截断不得影响完整脚本哈希: %q", run2.ScriptSHA)
	}

	// 详情带出 script/script_sha256；列表不回传脚本体
	rec = do(t, h, "GET", fmt.Sprintf("/api/runs/%d", run.ID), nil, &admin)
	if !strings.Contains(rec.Body.String(), "evidence-ok") || !strings.Contains(rec.Body.String(), run.ScriptSHA) {
		t.Fatalf("详情应带出执行证据: %s", rec.Body)
	}
	rec = do(t, h, "GET", "/api/runs?limit=50", nil, &admin)
	if strings.Contains(rec.Body.String(), "echo evidence-ok") {
		t.Fatalf("列表不得回传脚本体: %s", rec.Body)
	}
}

// TestRunsScopedByRunView run:view 作用域（覆盖语义收窄）：列表/详情按
// run 实际触达主机裁剪，跨作用域主机的任务明细不外泄。
func TestRunsScopedByRunView(t *testing.T) {
	s, st := newAppServer(t, 18843)
	h := s.Handler()
	admin := loginSession2(t, s, "e2e-pass-1")
	if _, err := st.CreateHost(&store.Host{Name: "sv-b", Address: "127.0.0.1", AgentPort: 2, Pools: []string{"pool-b"}}); err != nil {
		t.Fatal(err)
	}
	hosts, _ := st.ListHosts("")
	hostID := map[string]int64{}
	for _, hh := range hosts {
		hostID[hh.Name] = hh.ID
	}

	execOK := func(name string, ids []int64) int64 {
		t.Helper()
		rec := do(t, h, "POST", "/api/exec", map[string]any{
			"host_ids": ids, "script": "echo " + name,
		}, &admin)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s exec 应 200: %d %s", name, rec.Code, rec.Body)
		}
		var resp struct {
			RunID int64 `json:"run_id"`
		}
		json.Unmarshal(rec.Body.Bytes(), &resp)
		return resp.RunID
	}
	runA := execOK("a", []int64{hostID["e2e-local"]})
	runB := execOK("b", []int64{hostID["sv-b"]})
	runAB := execOK("ab", []int64{hostID["e2e-local"], hostID["sv-b"]})

	// 跨域 run 手工补齐两台主机的任务行：断言过滤行为与执行连通性无关
	if err := st.AddRunTask(&store.RunTask{RunID: runAB, Play: "p", Task: "t", Host: "sv-b", Status: "failed", Detail: "pool-b-secret"}); err != nil {
		t.Fatal(err)
	}
	if err := st.AddRunTask(&store.RunTask{RunID: runAB, Play: "p", Task: "t", Host: "e2e-local", Status: "ok", Detail: "pool-a-ok"}); err != nil {
		t.Fatal(err)
	}

	// viewer + run:view@e2e-pool：覆盖语义取代该点的全局授予
	scoped := newUserSession(t, h, admin, "rvsc", "scoped-Pass1", "viewer")
	do(t, h, "PUT", fmt.Sprintf("/api/users/%d/scopes", userIDByName(t, st, "rvsc")), map[string]any{
		"scopes": []map[string]any{{"verb": "run:view", "kind": "pool", "value": "e2e-pool"}},
	}, &admin)

	// 列表：只看到触达 e2e-local 的 run（A、AB），看不到纯域外的 B
	rec := do(t, h, "GET", "/api/runs", nil, &scoped)
	var runs []*store.Run
	json.Unmarshal(rec.Body.Bytes(), &runs)
	seen := map[int64]bool{}
	for _, rn := range runs {
		seen[rn.ID] = true
	}
	if seen[runB] || !seen[runA] || !seen[runAB] {
		t.Fatalf("列表裁剪异常: A=%v AB=%v B=%v", seen[runA], seen[runAB], seen[runB])
	}

	// 详情：跨域 run 可见（部分触达在域内），但域外主机的任务行被滤掉
	rec = do(t, h, "GET", fmt.Sprintf("/api/runs/%d", runAB), nil, &scoped)
	var det struct {
		Tasks []*store.RunTask `json:"tasks"`
	}
	json.Unmarshal(rec.Body.Bytes(), &det)
	hasInScope := false
	for _, tk := range det.Tasks {
		if tk.Host == "sv-b" {
			t.Fatalf("作用域外主机任务行泄露: %+v", tk)
		}
		if tk.Host == "e2e-local" {
			hasInScope = true
		}
	}
	if !hasInScope {
		t.Fatal("域内主机的任务行应保留")
	}
	// 纯域外 run 详情 403
	if rec = do(t, h, "GET", fmt.Sprintf("/api/runs/%d", runB), nil, &scoped); rec.Code != http.StatusForbidden {
		t.Fatalf("域外 run 详情应 403: %d", rec.Code)
	}

	// admin（全局 run:view）不受影响
	rec = do(t, h, "GET", "/api/runs", nil, &admin)
	runs = nil
	json.Unmarshal(rec.Body.Bytes(), &runs)
	if len(runs) < 3 {
		t.Fatalf("admin 应见全部 run: %d", len(runs))
	}
}

// TestRunDeleteAdminOnly 回归：operator 不再默认持有 run:delete——操作者
// 能清掉自己（或他人）的执行痕迹，与审计留存的目标相悖（docs/18 挂账项）。
func TestRunDeleteAdminOnly(t *testing.T) {
	s, st := newAppServer(t, 18845)
	h := s.Handler()
	admin := loginSession2(t, s, "e2e-pass-1")
	rid, err := st.CreateRun(store.RunInput{Kind: "exec", Selector: "{}", User: "admin", Status: "succeeded"})
	if err != nil {
		t.Fatal(err)
	}

	op := newUserSession(t, h, admin, "delop", "op-Pass1", "operator")
	if rec := do(t, h, "DELETE", fmt.Sprintf("/api/runs/%d", rid), nil, &op); rec.Code != http.StatusForbidden {
		t.Fatalf("operator 删除执行记录应 403: %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, h, "DELETE", fmt.Sprintf("/api/runs/%d", rid), nil, &admin); rec.Code != http.StatusOK {
		t.Fatalf("admin 删除应 200: %d %s", rec.Code, rec.Body)
	}
}
