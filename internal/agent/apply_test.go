package agent

// §7.7 自治执行的功能验证：journal 载体、异步提交/进度/中止、幂等与
// 续跑、自更新拒绝、空闲抑制。本地回环真实执行（selfexec 路径）。

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"wdp/internal/plan"
)

func enc(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

// selfPlan 构造一个最小自包含计划：本机 shell 任务序列。
func selfPlan(t *testing.T, host string, tasks []string) *plan.Plan {
	t.Helper()
	rts := make([]*plan.ResolvedTask, len(tasks))
	for i, script := range tasks {
		rts[i] = &plan.ResolvedTask{
			Idx: i + 1, Label: fmt.Sprintf("task-%d", i), Module: "shell", FreeForm: script,
		}
	}
	p := &plan.Plan{
		SchemaVer: plan.SchemaVer,
		Chart:     "selftest", Version: "1.0.0", Phase: "deploy",
		Values: map[string]any{},
		Files: map[string]string{
			"chart.yaml":  enc("name: selftest\nversion: \"1.0.0\"\n"),
			"values.yaml": enc("{}\n"),
			"deploy.yaml": enc("- hosts: all\n  tasks:\n    - shell: 'true'\n"),
		},
		Hosts: []*plan.HostPlan{{
			PlayIdx: 0, Host: host,
			Conn:   plan.HostConn{Conn: "selfexec"},
			Values: map[string]any{},
			Vars:   map[string]any{"inventory_hostname": host},
			Play:   plan.PlayMeta{Hosts: "all"},
			Tasks:  rts,
		}},
	}
	p.FillID()
	return p
}

// startTestAgent 起一个回环 agent（无认证），返回实例与基址。
func startTestAgent(t *testing.T) (*Server, string) {
	t.Helper()
	s := New("")
	s.SetRunsDir(filepath.Join(t.TempDir(), "runs"))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = s.serve(ln) }()
	base := "http://" + ln.Addr().String()
	t.Cleanup(func() {
		resp, err := http.Post(base+"/shutdown", "", nil)
		if err == nil {
			resp.Body.Close()
		}
	})
	return s, base
}

// submitPlan 提交计划（JSON 直传；handler 同时接受 gzip）。返回状态码、
// 响应体文本与解析后的响应。
func submitPlan(t *testing.T, base string, req PlanSubmitRequest) (int, string, PlanSubmitResponse) {
	t.Helper()
	body, _ := json.Marshal(req)
	resp, err := http.Post(base+"/plan", "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	var out PlanSubmitResponse
	if resp.StatusCode == http.StatusOK {
		_ = json.Unmarshal(raw, &out)
	}
	return resp.StatusCode, string(raw), out
}

// planStatus 查询进度。
func planStatus(t *testing.T, base, runID string, since int64) (*http.Response, PlanStatusResponse) {
	t.Helper()
	resp, err := http.Get(base + "/plan/status?run_id=" + runID + fmt.Sprintf("&since_seq=%d", since))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out PlanStatusResponse
	if resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
	}
	return resp, out
}

// waitRunState 轮询直到终态（超时报错）。
func waitRunState(t *testing.T, base, runID string, terminal ...string) PlanStatusResponse {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		_, st := planStatus(t, base, runID, 0)
		for _, want := range terminal {
			if st.State == want {
				return st
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("run %s 未达终态 %v", runID, terminal)
	return PlanStatusResponse{}
}

// TestJournalAppendAndReplay journal 单元：追加写、seq 递增、CompletedIdx 重放。
func TestJournalAppendAndReplay(t *testing.T) {
	dir := t.TempDir()
	j, err := OpenJournal(filepath.Join(dir, "journal.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	for i, st := range []string{"changed", "ok", "failed", "skipped"} {
		if err := j.Append(JournalEntry{Host: "h1", Idx: i + 1, Label: fmt.Sprintf("t%d", i), State: st}); err != nil {
			t.Fatal(err)
		}
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	// 重开续写：seq 不回退
	j2, err := OpenJournal(filepath.Join(dir, "journal.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	if err := j2.Append(JournalEntry{Host: "h1", Idx: 5, Label: "t4", State: "ok"}); err != nil {
		t.Fatal(err)
	}
	_ = j2.Close()

	entries, err := ReadJournal(filepath.Join(dir, "journal.ndjson"), 0)
	if err != nil || len(entries) != 5 {
		t.Fatalf("entries: %v %v", len(entries), err)
	}
	if entries[4].Seq != 5 || entries[0].Seq != 1 {
		t.Fatalf("seq 递增异常: %+v", entries)
	}
	// 增量游标（seq > 3：4 与 5 两条）
	inc, _ := ReadJournal(filepath.Join(dir, "journal.ndjson"), 3)
	if len(inc) != 2 || inc[0].Seq != 4 || inc[1].Seq != 5 {
		t.Fatalf("增量读取: %+v", inc)
	}
	// 重放：changed/ok 完成，failed/skipped 未完成
	done, err := CompletedIdx(filepath.Join(dir, "journal.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	if !done["h1"][1] || !done["h1"][2] || done["h1"][3] || done["h1"][4] {
		t.Fatalf("重放结果异常: %+v", done)
	}
}

// TestPlanSubmitRunAndStatus G1 核心路径：提交后异步收敛（控制端此刻可断开），
// journal 记录任务级进度，state 落终态。
func TestPlanSubmitRunAndStatus(t *testing.T) {
	_, base := startTestAgent(t)
	marker := filepath.Join(t.TempDir(), "done.flag")
	p := selfPlan(t, "self", []string{"mkdir -p " + filepath.Dir(marker) + " && echo ok > " + marker})

	code, _, out := submitPlan(t, base, PlanSubmitRequest{RunID: "run-1", Plan: p, LocalHost: "self"})
	if code != http.StatusOK || !out.Accepted || out.State != "running" {
		t.Fatalf("提交被拒绝: %d %+v", code, out)
	}
	st := waitRunState(t, base, "run-1", "done", "failed")
	if st.State != "done" {
		t.Fatalf("应 done: %+v", st)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("任务应真实执行: %v", err)
	}
	// journal 有任务级记录
	found := false
	for _, e := range st.Journal {
		if e.Idx == 1 && e.Host == "self" && e.State == "changed" {
			found = true
		}
	}
	if !found {
		t.Fatalf("journal 应含 idx0 changed: %+v", st.Journal)
	}
}

// TestPlanSubmitIdempotent 同一 plan 重复提交：返回同一 run，不启动第二个收敛。
func TestPlanSubmitIdempotent(t *testing.T) {
	_, base := startTestAgent(t)
	p := selfPlan(t, "self", []string{"sleep 1"})
	if code, _, out := submitPlan(t, base, PlanSubmitRequest{RunID: "run-idem", Plan: p, LocalHost: "self"}); code != 200 || !out.Accepted {
		t.Fatalf("首次提交: %d %+v", code, out)
	}
	// 收敛进行中重复提交（不同 run_id，同一 plan）：命中幂等（plan → run 映射）
	code, _, out := submitPlan(t, base, PlanSubmitRequest{RunID: "run-idem-2", Plan: p, LocalHost: "self"})
	if code != http.StatusOK || out.RunID != "run-idem" {
		t.Fatalf("重复提交应幂等返回原 run: %d %+v", code, out)
	}
	waitRunState(t, base, "run-idem", "done")

	// 不同 plan_id 复用同一 run_id：明确拒绝
	p2 := selfPlan(t, "self", []string{"true"})
	code2, body2, _ := submitPlan(t, base, PlanSubmitRequest{RunID: "run-idem", Plan: p2, LocalHost: "self"})
	if code2 != http.StatusConflict || !strings.Contains(body2, "refusing to reuse") {
		t.Fatalf("不同 plan 复用 run_id 应 409: %d %s", code2, body2)
	}
}

// TestPlanResumeFromJournal 断点续跑：journal 已记 idx0 完成 → resume 跳过
// idx0（记 skipped）并从 idx1 继续。
func TestPlanResumeFromJournal(t *testing.T) {
	s, base := startTestAgent(t)
	dirA := t.TempDir()
	dirB := t.TempDir()
	p := selfPlan(t, "self", []string{
		"echo a > " + filepath.Join(dirA, "a"),
		"echo b > " + filepath.Join(dirB, "b"),
	})
	// selfPlan 的 idx 从 1 起：任务 1（写 a）预置为已完成，任务 2（写 b）待执行
	// 预置 run 目录：plan.id 一致 + journal 记 idx0 已 changed
	runDir := filepath.Join(s.runsRoot(), "run-resume")
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "plan.id"), []byte(p.PlanID), 0o600); err != nil {
		t.Fatal(err)
	}
	jline := `{"seq":1,"host":"self","idx":1,"label":"task-0","state":"changed","changed":true,"at":"2026-01-01T00:00:00Z"}`
	if err := os.WriteFile(filepath.Join(runDir, "journal.ndjson"), []byte(jline+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	code, _, out := submitPlan(t, base, PlanSubmitRequest{RunID: "run-resume", Plan: p, LocalHost: "self", Resume: true})
	if code != http.StatusOK || out.ResumedFromIdx["self"] != 1 {
		t.Fatalf("resume 提交异常: %d %+v", code, out)
	}
	st := waitRunState(t, base, "run-resume", "done", "failed")
	if st.State != "done" {
		t.Fatalf("resume 应 done: %+v", st)
	}
	// 任务 1 本轮记 skipped；任务 2 真实执行
	var idx1, idx2 string
	for _, e := range st.Journal {
		if e.Seq <= 1 {
			continue // 预置的历史条目
		}
		if e.Idx == 1 {
			idx1 = e.State
		}
		if e.Idx == 2 {
			idx2 = e.State
		}
	}
	if idx1 != "skipped" {
		t.Fatalf("任务 1 应 skipped（已完成的任务不重做）: %q", idx1)
	}
	if idx2 != "changed" {
		t.Fatalf("任务 2 应执行: %q", idx2)
	}
	if _, err := os.Stat(filepath.Join(dirA, "a")); !os.IsNotExist(err) {
		t.Fatal("任务 1 不应重做（文件不应被重新创建）")
	}
	if _, err := os.Stat(filepath.Join(dirB, "b")); err != nil {
		t.Fatalf("任务 2 应执行: %v", err)
	}
}

// TestPlanResumePlanChanged 换了 plan 的 resume：明确拒绝，不静默重跑。
func TestPlanResumePlanChanged(t *testing.T) {
	s, base := startTestAgent(t)
	p := selfPlan(t, "self", []string{"true"})
	runDir := filepath.Join(s.runsRoot(), "run-changed")
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "plan.id"), []byte("0000000000000000000000000000000000000000000000000000000000000000"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, body, _ := submitPlan(t, base, PlanSubmitRequest{RunID: "run-changed", Plan: p, LocalHost: "self", Resume: true})
	if code != http.StatusConflict || !strings.Contains(body, "resume refused") {
		t.Fatalf("plan 变更的 resume 应拒绝: %d %s", code, body)
	}
}

// TestPlanSelfUpdateRejected plan 目标是 agent 自身二进制：拒绝执行。
func TestPlanSelfUpdateRejected(t *testing.T) {
	s, base := startTestAgent(t)
	selfBin, _ := os.Executable()
	p := selfPlan(t, "self", nil)
	p.Hosts[0].Tasks = []*plan.ResolvedTask{{
		Idx: 0, Label: "upgrade-agent", Module: "copy",
		Args: map[string]any{"src": "x", "dest": selfBin},
	}}
	p.FillID()
	code, body, _ := submitPlan(t, base, PlanSubmitRequest{RunID: "run-selfupd", Plan: p, LocalHost: "self"})
	if code != http.StatusBadRequest || !strings.Contains(body, "self-update") {
		t.Fatalf("自更新应拒绝: %d %s", code, body)
	}
	_ = s
}

// TestPlanCancel 中止：running 的 run 被取消后落 cancelled。
func TestPlanCancel(t *testing.T) {
	_, base := startTestAgent(t)
	p := selfPlan(t, "self", []string{"sleep 30"})
	submitPlan(t, base, PlanSubmitRequest{RunID: "run-cancel", Plan: p, LocalHost: "self"})
	// 等收敛真正跑起来再取消
	time.Sleep(300 * time.Millisecond)
	resp, err := http.Post(base+"/plan/cancel", "application/json", strings.NewReader(`{"run_id":"run-cancel"}`))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("cancel: %v %d", err, resp.StatusCode)
	}
	resp.Body.Close()
	st := waitRunState(t, base, "run-cancel", "cancelled", "done")
	if st.State != "cancelled" {
		t.Fatalf("应 cancelled: %+v", st)
	}
}

// TestIdleSuppressedDuringPlan §7.5 必查项：自治执行期间没有请求到达，
// agent 不被空闲计时器杀掉。
func TestIdleSuppressedDuringPlan(t *testing.T) {
	s := New("")
	s.SetRunsDir(filepath.Join(t.TempDir(), "runs"))
	s.SetIdleTimeout(300 * time.Millisecond)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = s.serve(ln) }()
	base := "http://" + ln.Addr().String()
	defer http.Post(base+"/shutdown", "", nil)

	p := selfPlan(t, "self", []string{"sleep 1"})
	submitPlan(t, base, PlanSubmitRequest{RunID: "run-idle", Plan: p, LocalHost: "self"})
	st := waitRunState(t, base, "run-idle", "done", "failed", "cancelled")
	if st.State != "done" {
		t.Fatalf("长于空闲周期的收敛应完成而非被杀: %+v", st)
	}
	// 服务器仍在服务（未因空闲退出）
	if resp, err := http.Get(base + "/health"); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("agent 应存活: %v", err)
	} else {
		resp.Body.Close()
	}
}

// TestPlanIntegrityRejected 内容寻址复核：篡改的 plan 被拒绝。
func TestPlanIntegrityRejected(t *testing.T) {
	_, base := startTestAgent(t)
	p := selfPlan(t, "self", []string{"true"})
	p.Values["tampered"] = true // 不重算 PlanID
	code, body, _ := submitPlan(t, base, PlanSubmitRequest{RunID: "run-bad", Plan: p, LocalHost: "self"})
	if code != http.StatusBadRequest || !strings.Contains(body, "mismatch") {
		t.Fatalf("篡改 plan 应拒绝: %d %s", code, body)
	}
}

// TestPlanRejectsMaliciousIDs run_id 直接拼进运行目录路径、plan_id 参与
// 错误消息截断：非法/过短的取值必须被 400 拒绝，而不是路径穿越或 panic。
func TestPlanRejectsMaliciousIDs(t *testing.T) {
	_, base := startTestAgent(t)
	p := selfPlan(t, "self", []string{"true"})

	for _, bad := range []string{"../../etc", "a/b", ".", "..", strings.Repeat("x", 65)} {
		code, body, _ := submitPlan(t, base, PlanSubmitRequest{RunID: bad, Plan: p, LocalHost: "self"})
		if code != http.StatusBadRequest {
			t.Fatalf("run_id %q 应被拒绝: %d %s", bad, code, body)
		}
	}

	// plan_id 过短：ComputeID 不匹配 → 走错误分支，截断展示不得越界 panic
	short := *p
	short.PlanID = "x"
	code, body, _ := submitPlan(t, base, PlanSubmitRequest{RunID: "run-short", Plan: &short, LocalHost: "self"})
	if code != http.StatusBadRequest || !strings.Contains(body, "mismatch") {
		t.Fatalf("过短 plan_id 应返回 400 而非 panic: %d %s", code, body)
	}

	// 读取路径同样拒绝越界 run_id
	resp, err := http.Get(base + "/plan/status?run_id=../../etc")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status 越界 run_id 应被拒绝: %d", resp.StatusCode)
	}
}
