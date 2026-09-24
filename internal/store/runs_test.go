package store

import (
	"errors"
	"testing"
)

// TestCreateRunsExclusiveConflict：已有 queued/running 时创建被拒并携带
// 冲突方明细；终态后可再次创建。事务内检查+插入是真正的互斥——先查后
// 插分开的旧实现存在并发窗口（两个请求同时通过检查同时创建）。
func TestCreateRunsExclusiveConflict(t *testing.T) {
	s := openTest(t)
	app1 := createTestApp(t, s, "app1")
	if _, err := s.CreateRunsExclusive([]RunInput{{Kind: "app", AppID: app1, AppName: "app1", Status: "queued"}}); err != nil {
		t.Fatal(err)
	}
	// 同应用再建 → 冲突
	_, err := s.CreateRunsExclusive([]RunInput{{Kind: "app", AppID: app1, AppName: "app1", Status: "queued"}})
	var cf *RunConflictError
	if !errors.As(err, &cf) {
		t.Fatalf("应返回 RunConflictError，实际 %v", err)
	}
	if len(cf.Active) != 1 || cf.Active[0].AppName != "app1" {
		t.Fatalf("冲突明细缺失: %+v", cf.Active)
	}
	// 其它应用不受影响
	app2 := createTestApp(t, s, "app2")
	ids, err := s.CreateRunsExclusive([]RunInput{{Kind: "app", AppID: app2, AppName: "app2", Status: "queued"}})
	if err != nil || len(ids) != 1 {
		t.Fatalf("无冲突应用应创建成功: %v", err)
	}
	// 冲突方终态后可再建
	if err := s.FinishRun(1, "failed", "x"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateRunsExclusive([]RunInput{{Kind: "app", AppID: app1, AppName: "app1", Status: "queued"}}); err != nil {
		t.Fatalf("终态后应可创建: %v", err)
	}
}

// TestCreateRunsExclusiveAtomic：多条目插入遇冲突时整批不落库。
func TestCreateRunsExclusiveAtomic(t *testing.T) {
	s := openTest(t)
	app1 := createTestApp(t, s, "a1")
	app2 := createTestApp(t, s, "a2")
	if _, err := s.CreateRunsExclusive([]RunInput{{Kind: "app", AppID: app1, AppName: "a1", Status: "running"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateRunsExclusive([]RunInput{
		{Kind: "app", AppID: app2, AppName: "a2", Status: "queued"},
		{Kind: "app", AppID: app1, AppName: "a1", Status: "queued"}, // 冲突项在后
	}); err == nil {
		t.Fatal("含冲突项的批次应整体拒绝")
	}
	// app2 的行不得残留（事务回滚）
	if active, err := s.ActiveRunsByApp([]int64{app2}); err != nil || len(active) != 0 {
		t.Fatalf("回滚后不应残留半批记录: %v %v", active, err)
	}
}

// createTestApp 建一个应用（含首个版本）返回 ID。
func createTestApp(t *testing.T, s *Store, name string) int64 {
	t.Helper()
	id, err := s.CreateApp(name, "", "{}", nil, nil, "1.0.0", "/tmp/x.tgz", "deadbeef", 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	return id
}
