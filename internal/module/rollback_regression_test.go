package module

// 回滚链路回归：
//   - 快照失败必须上报（静默吞掉会让 auto_rollback"报成功却什么都没保住"）；
//   - 快照/恢复的路径归一：带尾斜杠的 dest 曾让 cp -a 变成"拷入"，
//     现场变成 path/<basename>，原内容不在原位却报成功。

import (
	"strings"
	"testing"

	"wdp/internal/conn"
)

// TestSnapshotReportsFailure 快照命令失败时回调必须触发，且不登记动作。
func TestSnapshotReportsFailure(t *testing.T) {
	rc, fake := newTestRC(t)
	fake.ExecFn = func(conn.ExecRequest) (conn.ExecResult, error) {
		return conn.ExecResult{Code: 1, Stderr: "cp: cannot stat: Permission denied"}, nil
	}
	var recorded []RollbackAction
	var failures []string
	rc.Rollback = &RollbackCtx{
		Dir:    "/var/.wdp-rollback",
		Record: func(a RollbackAction) { recorded = append(recorded, a) },
		OnSnapshotFailure: func(path, reason string) {
			failures = append(failures, path+"|"+reason)
		},
	}
	rc.Rollback.Snapshot(rc, "/etc/app.conf")

	if len(recorded) != 0 {
		t.Fatalf("快照失败不应登记 restore 动作: %+v", recorded)
	}
	if len(failures) != 1 || !strings.HasPrefix(failures[0], "/etc/app.conf|") {
		t.Fatalf("快照失败必须上报: %+v", failures)
	}
	if !strings.Contains(failures[0], "Permission denied") {
		t.Fatalf("上报应带原因: %+v", failures)
	}
}

// TestSnapshotRejectsRelativePath 相对路径无法在 shadow 区定位，应上报
// 而不是静默跳过。
func TestSnapshotRejectsRelativePath(t *testing.T) {
	rc, _ := newTestRC(t)
	var failures []string
	rc.Rollback = &RollbackCtx{
		Dir:               "/var/.wdp-rollback",
		Record:            func(RollbackAction) {},
		OnSnapshotFailure: func(path, reason string) { failures = append(failures, path+"|"+reason) },
	}
	rc.Rollback.Snapshot(rc, "etc/app.conf")
	if len(failures) != 1 {
		t.Fatalf("相对路径应上报失败: %+v", failures)
	}
}

// TestSnapshotTrailingSlashNormalized 带尾斜杠的 dest 归一化后再快照：
// 否则 pathDirOf(shadow) 会退化成 shadow 本身，mkdir -p 预建目标后
// cp -a 变成"拷入"，快照内容错层。
func TestSnapshotTrailingSlashNormalized(t *testing.T) {
	rc, fake := newTestRC(t)
	var script string
	fake.ExecFn = func(req conn.ExecRequest) (conn.ExecResult, error) {
		if strings.Contains(req.Script, "cp -a") {
			script = req.Script
		}
		return conn.ExecResult{Code: 0}, nil
	}
	var recorded []RollbackAction
	rc.Rollback = &RollbackCtx{
		Dir:    "/var/.wdp-rollback",
		Record: func(a RollbackAction) { recorded = append(recorded, a) },
	}
	rc.Rollback.Snapshot(rc, "/srv/data/")

	if strings.Contains(script, "/srv/data/ ") || strings.Contains(script, "'/srv/data/'") {
		t.Fatalf("快照命令应使用归一化后的路径: %s", script)
	}
	if len(recorded) != 1 || recorded[0].Path != "/srv/data" {
		t.Fatalf("登记的恢复路径应为归一化路径: %+v", recorded)
	}
	if !strings.HasPrefix(recorded[0].Shadow, "/var/.wdp-rollback/srv/data") {
		t.Fatalf("shadow 路径异常: %+v", recorded[0])
	}
}
