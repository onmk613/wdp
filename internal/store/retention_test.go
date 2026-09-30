package store

// 保留策略与删除级联清理的回归测试：runs/audit/drafts 的时间清理通道，
// 以及 DeleteHost/DeleteApp 的孤儿行清理（此前告警与草稿会永久残留）。

import (
	"testing"
	"time"
)

// ageRun 把 run 的 started_at 拨回过去（模拟历史行）。
func ageRun(t *testing.T, s *Store, id int64, ts string) {
	t.Helper()
	if _, err := s.exec(`UPDATE runs SET started_at = ? WHERE id = ?`, ts, id); err != nil {
		t.Fatal(err)
	}
}

// TestPruneRunsRespectsStatusAndCutoff 只删早于 cutoff 的已终结 run；
// queued/running（哪怕早于 cutoff）与近期 run 恒不删，任务明细随行级联。
func TestPruneRunsRespectsStatusAndCutoff(t *testing.T) {
	s := openTest(t)
	old := "2020-01-01T00:00:00Z"
	cutoff := time.Now().UTC().Format(time.RFC3339)

	mk := func(status string, aged bool) int64 {
		t.Helper()
		id, err := s.CreateRun(RunInput{Kind: "exec", Selector: "{}", User: "u", Status: status})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.AddRunTask(&RunTask{RunID: id, Host: "h", Status: "ok"}); err != nil {
			t.Fatal(err)
		}
		if aged {
			ageRun(t, s, id, old)
		}
		return id
	}
	oldDone := mk("failed", true)
	oldRunning := mk("running", true)
	recentDone := mk("succeeded", false)

	n, err := s.PruneRuns(cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("应只删 1 条（旧且已终结）: %d", n)
	}
	for _, id := range []int64{oldDone} {
		if _, err := s.GetRun(id); err != ErrNotFound {
			t.Fatalf("旧终结 run %d 应被删: %v", id, err)
		}
		var cnt int
		s.queryRow(`SELECT COUNT(*) FROM run_tasks WHERE run_id = ?`, id).Scan(&cnt)
		if cnt != 0 {
			t.Fatalf("run %d 的任务明细应级联删除", id)
		}
	}
	for _, id := range []int64{oldRunning, recentDone} {
		if _, err := s.GetRun(id); err != nil {
			t.Fatalf("run %d 不应被删: %v", id, err)
		}
	}
}

// TestPruneAuditLogsAndDrafts 审计与草稿按 updated_at/created_at 清理。
func TestPruneAuditLogsAndDrafts(t *testing.T) {
	s := openTest(t)
	old := "2020-01-01T00:00:00Z"
	cutoff := time.Now().UTC().Format(time.RFC3339)

	if err := s.CreateAuditLog(&AuditLog{User: "u", Action: "a", Object: "o"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.exec(`INSERT INTO audit_logs (user, action, object, name, detail, ip, created_at) VALUES ('u','a','o','','','', ?)`, old); err != nil {
		t.Fatal(err)
	}
	n, err := s.PruneAuditLogs(cutoff)
	if err != nil || n != 1 {
		t.Fatalf("审计应删 1 条: n=%d err=%v", n, err)
	}
	var cnt int
	s.queryRow(`SELECT COUNT(*) FROM audit_logs`).Scan(&cnt)
	if cnt != 1 {
		t.Fatalf("近期审计应保留: %d", cnt)
	}

	if err := s.PutAppDraft(&AppDraft{UserID: 1, AppKey: "new:x", Payload: "{}"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.exec(`INSERT INTO app_drafts (user_id, app_key, base_version, payload, updated_at) VALUES (2, 'new:y', '', '{}', ?)`, old); err != nil {
		t.Fatal(err)
	}
	n, err = s.PruneAppDrafts(cutoff)
	if err != nil || n != 1 {
		t.Fatalf("草稿应删 1 条: n=%d err=%v", n, err)
	}
	s.queryRow(`SELECT COUNT(*) FROM app_drafts`).Scan(&cnt)
	if cnt != 1 {
		t.Fatalf("近期草稿应保留: %d", cnt)
	}
}

// TestDeleteHostCleansAlertsAndMetrics 回归：已删主机的告警以空 HostName
// 永久留在 /api/alerts、指标桶持续占库——删除需同事务清掉。
func TestDeleteHostCleansAlertsAndMetrics(t *testing.T) {
	s := openTest(t)
	h, err := s.CreateHost(&Host{Name: "gone", Address: "127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetHostAlert(h, "cpu", "warn", "high", 90); err != nil {
		t.Fatal(err)
	}
	if _, err := s.exec(`INSERT INTO metrics_5m (host_id, metric, labels, bucket, n, vsum, vmax) VALUES (?, 'cpu', '', 0, 1, 1, 1)`, h); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteHost(h); err != nil {
		t.Fatal(err)
	}
	var alerts, metrics int
	s.queryRow(`SELECT COUNT(*) FROM host_alerts WHERE host_id = ?`, h).Scan(&alerts)
	s.queryRow(`SELECT COUNT(*) FROM metrics_5m WHERE host_id = ?`, h).Scan(&metrics)
	if alerts != 0 || metrics != 0 {
		t.Fatalf("删主机应清告警与指标: alerts=%d metrics=%d", alerts, metrics)
	}
}

// TestDeleteAppKeepsOrphanDrafts 应用删除后草稿**保留**：孤儿草稿是特性
// （前端 AppGone 标记 + 草稿箱救回编辑内容），体积与敏感残留由
// PruneAppDrafts 的 TTL 收口，不靠删应用级联。
func TestDeleteAppKeepsOrphanDrafts(t *testing.T) {
	s := openTest(t)
	id, err := s.CreateApp("old-app", "", "{}", nil, nil, "1.0.0", "/dev/null", "sha", 1, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.PutAppDraft(&AppDraft{UserID: 1, AppKey: "1", Payload: "{}"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteApp(id); err != nil {
		t.Fatal(err)
	}
	var cnt int
	s.queryRow(`SELECT COUNT(*) FROM app_drafts WHERE app_key = '1'`).Scan(&cnt)
	if cnt != 1 {
		t.Fatal("孤儿草稿应保留（AppGone 恢复流程依赖它）")
	}
}
