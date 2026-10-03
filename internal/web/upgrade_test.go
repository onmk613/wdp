package web

// agent 远程升级：后台 run 化后的受理语义（立即返回 run_id）、run 落库
// 元数据、步骤明细（run_tasks）、终态推进与失败/取消路径；闸门占用 409、
// 同版本幂等短路（真实 mTLS loopback agent）、替换脚本形态
// （/proc/$PPID/exe 定位 + 原子 mv + systemd 分支）。
// force 全流程会真实改写目标机文件，不在单测执行——由冒烟环境人工验证。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"wdp/internal/agent"
	"wdp/internal/buildinfo"
	"wdp/internal/store"
)

// waitRunState 轮询等 run 到达期望状态之一并返回（后台 goroutine 异步
// 推进，测试侧只能观察落库结果；10s 上限远超本文件全部路径——离线探活
// 毫秒级失败、同版本短路一次探活即回）。
func waitRunState(t *testing.T, st *store.Store, id int64, want ...string) *store.Run {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		r, err := st.GetRun(id)
		if err != nil {
			t.Fatalf("get run %d: %v", id, err)
		}
		for _, w := range want {
			if r.Status == w {
				return r
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %d 状态停在 %q（期望 %v）", id, r.Status, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestUpgradeAlreadyLatest 同版本幂等短路：受理立即返回 run_id，run 很快
// 以 succeeded 收口（摘要"已是最新"），只落探活一步明细。
func TestUpgradeAlreadyLatest(t *testing.T) {
	s, st := newAppServerMTLS(t, 18764)
	h := s.Handler()
	token := loginSession2(t, s, "e2e-pass-1")

	// 注入定版构建信息：测试二进制默认 Commit=="none"（未注入 ldflags），
	// 属「未定版本」形态——版本串不反映内容，幂等短路被有意跳过（见
	// upgradeAgent）。注入后 loopback agent（同进程，实时取值）与 server
	// 报同一定版串，短路生效
	oldCommit := buildinfo.Commit
	buildinfo.Commit = "c0ffee0"
	defer func() { buildinfo.Commit = oldCommit }()

	hosts, err := st.ListHosts("")
	if err != nil || len(hosts) != 1 {
		t.Fatalf("loopback 主机应已建账: %v %d", err, len(hosts))
	}
	hid := hosts[0].ID

	// loopback agent 与 server 同二进制：非 force 应幂等短路。
	// 受理是立即返回的（不等升级完成）——只断言 run_id 已落库可用
	rec := do(t, h, "POST", fmt.Sprintf("/api/hosts/%d/upgrade", hid), map[string]any{}, &token)
	if rec.Code != http.StatusOK {
		t.Fatalf("受理应 200: %d %s", rec.Code, rec.Body)
	}
	var resp struct {
		RunID int64 `json:"run_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp.RunID == 0 {
		t.Fatalf("受理响应应带 run_id: %s", rec.Body)
	}

	run := waitRunState(t, st, resp.RunID, "succeeded", "failed")
	if run.Status != "succeeded" {
		t.Fatalf("同版本应幂等成功: %s %s", run.Status, run.Summary)
	}
	if !strings.Contains(run.Summary, "已是最新") || !strings.Contains(run.Summary, buildinfo.BuildVersion()) {
		t.Fatalf("摘要应报告已是最新: %s", run.Summary)
	}
	if agent.BuildVersion() != buildinfo.BuildVersion() {
		t.Fatal("agent 与 server 版本口径应一致")
	}
	// run 元数据：kind/目标主机/目标版本/发起人（受理请求的会话用户）
	if run.Kind != upgradeRunKind || run.AppName != hosts[0].Name || run.User != "admin" {
		t.Fatalf("run 元数据异常: %+v", run)
	}
	if run.Version != agent.BuildVersion() {
		t.Fatalf("run.Version 应记目标版本: %+v", run)
	}
	if !strings.Contains(run.Selector, fmt.Sprintf(`[%d]`, hid)) {
		t.Fatalf("selector 应记触达主机（run:view 裁剪依据）: %s", run.Selector)
	}
	// 短路只应有探活一步，无 upload/replace 明细
	tasks, err := st.RunTasks(resp.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 || tasks[0].Task != "probe" || tasks[0].Status != "ok" {
		t.Fatalf("短路应只落 probe 一步: %+v", tasks)
	}
	if !strings.Contains(tasks[0].Detail, buildinfo.BuildVersion()) {
		t.Fatalf("probe 明细应含目标版本: %+v", tasks[0])
	}
}

// TestUpgradeOfflineRun 离线主机：受理成功但 run 以 failed 收口（不可达），
// probe 步骤明细落库；详情端点可回看（断连恢复的数据面）。
func TestUpgradeOfflineRun(t *testing.T) {
	s, st := newTestServer(t)
	h := s.Handler()
	token := loginSession(t, s)
	if _, err := st.CreateHost(&store.Host{Name: "up-off", Address: "127.0.0.1", AgentPort: 1}); err != nil {
		t.Fatal(err)
	}

	rec := do(t, h, "POST", "/api/hosts/1/upgrade", map[string]any{}, &token)
	if rec.Code != http.StatusOK {
		t.Fatalf("离线也应受理（后台判失败）: %d %s", rec.Code, rec.Body)
	}
	var resp struct {
		RunID int64 `json:"run_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp.RunID == 0 {
		t.Fatalf("受理响应应带 run_id: %s", rec.Body)
	}
	run := waitRunState(t, st, resp.RunID, "succeeded", "failed")
	if run.Status != "failed" || !strings.Contains(run.Summary, "不可达") {
		t.Fatalf("离线应 failed 且摘要含原因: %s %s", run.Status, run.Summary)
	}
	tasks, err := st.RunTasks(resp.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 || tasks[0].Task != "probe" || tasks[0].Status != "failed" || !strings.Contains(tasks[0].Detail, "不可达") {
		t.Fatalf("离线应落失败的 probe 明细: %+v", tasks)
	}
	// 详情端点回看（run:view 元数据 + tasks 明细）
	rec = do(t, h, "GET", fmt.Sprintf("/api/runs/%d", resp.RunID), nil, &token)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), upgradeRunKind) {
		t.Fatalf("run 详情应可回看: %d %s", rec.Code, rec.Body)
	}
	// 空参数校验
	if rec := do(t, h, "POST", "/api/hosts/upgrade", map[string]any{"ids": []int64{}}, &token); rec.Code != http.StatusBadRequest {
		t.Fatalf("空 ids 应 400: %d %s", rec.Code, rec.Body)
	}
}

// TestUpgradeBatchRuns 批量：每台一条独立 run（重复 id 去重），响应带
// run↔主机映射；后台并发推进到各自终态；runs 列表按 kind=upgrade 可查
// （前端轮询数据源）。
func TestUpgradeBatchRuns(t *testing.T) {
	s, st := newTestServer(t)
	h := s.Handler()
	token := loginSession(t, s)
	for _, n := range []string{"up-b1", "up-b2", "up-b3"} {
		if _, err := st.CreateHost(&store.Host{Name: n, Address: "127.0.0.1", AgentPort: 1}); err != nil {
			t.Fatal(err)
		}
	}
	rec := do(t, h, "POST", "/api/hosts/upgrade", map[string]any{"ids": []int64{1, 2, 3, 2}}, &token)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("批量受理应 202: %d %s", rec.Code, rec.Body)
	}
	var resp struct {
		RunIDs []int64 `json:"run_ids"`
		Hosts  []struct {
			RunID  int64  `json:"run_id"`
			HostID int64  `json:"host_id"`
			Name   string `json:"name"`
		} `json:"hosts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.RunIDs) != 3 || len(resp.Hosts) != 3 {
		t.Fatalf("重复 id 应去重为 3 台: %s", rec.Body)
	}
	for i, m := range resp.Hosts {
		if m.RunID != resp.RunIDs[i] || m.HostID != int64(i)+1 || m.Name == "" {
			t.Fatalf("run↔主机映射异常: %+v", resp.Hosts)
		}
	}
	for _, id := range resp.RunIDs {
		run := waitRunState(t, st, id, "succeeded", "failed", "cancelled")
		if run.Status != "failed" || !strings.Contains(run.Summary, "不可达") {
			t.Fatalf("离线主机的 run 应 failed: %s %s", run.Status, run.Summary)
		}
		tasks, err := st.RunTasks(id)
		if err != nil || len(tasks) == 0 || tasks[0].Task != "probe" {
			t.Fatalf("每台应有 probe 明细: %v %+v", err, tasks)
		}
	}
	// runs 列表按 kind 过滤（前端轮询升级进度的数据源）
	rec = do(t, h, "GET", "/api/runs?kind=upgrade&limit=10", nil, &token)
	if rec.Code != http.StatusOK {
		t.Fatalf("runs 列表应 200: %d %s", rec.Code, rec.Body)
	}
	var runs []*store.Run
	if err := json.Unmarshal(rec.Body.Bytes(), &runs); err != nil {
		t.Fatal(err)
	}
	if len(runs) != 3 {
		t.Fatalf("kind=upgrade 应恰有 3 条: %s", rec.Body)
	}
	for _, r := range runs {
		if r.Kind != upgradeRunKind || r.User != "admin" {
			t.Fatalf("升级 run 元数据异常: %+v", r)
		}
	}
}

// TestUpgradeGateBusy 闸门占用期间单台升级受理应 409 快速失败（同步闸门
// 语义保留；批量路径改为后台排队，不在此列）。
func TestUpgradeGateBusy(t *testing.T) {
	s, st := newTestServer(t)
	h := s.Handler()
	token := loginSession(t, s)
	if _, err := st.CreateHost(&store.Host{Name: "up-busy", Address: "127.0.0.1", AgentPort: 1}); err != nil {
		t.Fatal(err)
	}
	release := s.gate.Acquire([]int64{1})
	defer release()
	if rec := do(t, h, "POST", "/api/hosts/1/upgrade", map[string]any{}, &token); rec.Code != http.StatusConflict {
		t.Fatalf("闸门占用期间应 409: %d %s", rec.Code, rec.Body)
	}
}

// TestUpgradeCancelQueued 排队期取消：闸门全占住 → 批量 run 停在 queued
// （后台排队）；取消其中一台后释放闸门——被取消的停在 cancelled（未执行
// 任何步骤），其余照常推进到各自终态。
func TestUpgradeCancelQueued(t *testing.T) {
	s, st := newTestServer(t)
	h := s.Handler()
	token := loginSession(t, s)
	ids := make([]int64, 0, 3)
	for _, n := range []string{"up-c1", "up-c2", "up-c3"} {
		id, err := st.CreateHost(&store.Host{Name: n, Address: "127.0.0.1", AgentPort: 1})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	release := s.gate.Acquire(ids)
	rec := do(t, h, "POST", "/api/hosts/upgrade", map[string]any{"ids": ids}, &token)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("批量受理应 202: %d %s", rec.Code, rec.Body)
	}
	var resp struct {
		RunIDs []int64 `json:"run_ids"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || len(resp.RunIDs) != 3 {
		t.Fatalf("受理响应异常: %s", rec.Body)
	}
	// 取消第 1 台：取消注册在后台 worker 起点完成，极端竞态下（取消先于
	// 注册到达）run 不在注册表会 409——重试到注册可见为止
	deadline := time.Now().Add(3 * time.Second)
	for {
		rec = do(t, h, "POST", fmt.Sprintf("/api/runs/%d/cancel", resp.RunIDs[0]), map[string]any{}, &token)
		if rec.Code == http.StatusOK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("排队中的 run 应可取消: %d %s", rec.Code, rec.Body)
		}
		time.Sleep(20 * time.Millisecond)
	}
	release()
	// 被取消的：cancelled 且无任何步骤明细（未执行任何步骤）
	run := waitRunState(t, st, resp.RunIDs[0], "succeeded", "failed", "cancelled")
	if run.Status != "cancelled" || !strings.Contains(run.Summary, "用户取消") {
		t.Fatalf("排队取消应 cancelled: %s %s", run.Status, run.Summary)
	}
	if tasks, err := st.RunTasks(resp.RunIDs[0]); err != nil || len(tasks) != 0 {
		t.Fatalf("排队取消不应有步骤明细: %v %+v", err, tasks)
	}
	// 其余两台照常推进（离线 → failed），不受同批取消影响
	for _, id := range resp.RunIDs[1:] {
		r := waitRunState(t, st, id, "succeeded", "failed", "cancelled")
		if r.Status != "failed" {
			t.Fatalf("未取消的 run 应照常推进: %s %s", r.Status, r.Summary)
		}
	}
}

func TestUpgradeScript(t *testing.T) {
	sc := upgradeScript("/usr/local/bin/.wdp-upgrade-abc123", "")
	for _, want := range []string{
		`readlink /proc/$PPID/exe`,                          // 旧 agent：探测二进制
		`mv -f '/usr/local/bin/.wdp-upgrade-abc123' "$DST"`, // 原子替换（单引号字面量）
		`systemctl restart wdp-agent`,                       // systemd 重启
		`WDP_NO_SYSTEMD`,                                    // 非 systemd 分支
	} {
		if !strings.Contains(sc, want) {
			t.Fatalf("升级脚本缺 %q:\n%s", want, sc)
		}
	}
}

// TestUpgradeScriptKnownPath 已知 bin_path：直接使用，不做探测。
func TestUpgradeScriptKnownPath(t *testing.T) {
	sc := upgradeScript("/tmp/x/.wdp-upgrade-1", "/tmp/x/wdp")
	if strings.Contains(sc, "readlink") || !strings.Contains(sc, `DST='/tmp/x/wdp'`) {
		t.Fatalf("已知路径应直用:\n%s", sc)
	}
}

// TestUpgradeScriptUnsafeDst dst 来自 agent 上报（不可信）：含引号/命令
// 替换语法时必须是单引号字面量，不得闭合注入。
func TestUpgradeScriptUnsafeDst(t *testing.T) {
	sc := upgradeScript("/opt/$(.wdp-upgrade-1", `/opt/x';reboot;'/wdp`)
	if !strings.Contains(sc, `DST='/opt/x'\'';reboot;'\''/wdp'`) {
		t.Fatalf("不可信 dst 应安全引用:\n%s", sc)
	}
	// tmp 与 dst 同目录：路径里的命令替换语法同样只作字面量
	if !strings.Contains(sc, `mv -f '/opt/$(.wdp-upgrade-1' "$DST"`) {
		t.Fatalf("tmp 路径应安全引用:\n%s", sc)
	}
}
