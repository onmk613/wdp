package module

import "testing"

// 回归：dest 是既有目录时必须显式失败，而不是当作"不存在"继续走
// 上传 + RecordRemove——自动回滚会 rm -rf 掉这个既有目录。
func TestPutFileRejectsDirectoryDest(t *testing.T) {
	rc, _ := newTestRC(t)
	if out, bad := rc.exec("mkdir -p '/data'"); bad != nil || out.Code != 0 {
		t.Fatalf("预置目录失败: %v %+v", bad, out)
	}
	mod := &CopyModule{}
	r := mod.Run(rc, map[string]any{"content": "x", "dest": "/data"}, "")
	if !r.Failed {
		t.Fatalf("dest 为既有目录应失败: %+v", r)
	}
}
