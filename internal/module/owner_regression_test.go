package module

import "testing"

// 回归：只设 owner 不设 group 时不得漂移——远端 group 为主组名（非空），
// 旧比较 curGroup != ""（group 未给出）恒真，每轮 changed + 每轮 chown。
func TestPutFileOwnerOnlyNoDrift(t *testing.T) {
	rc, _ := ownerFake(t, "app app") // 远端 owner=app、group=主组 app
	rc.Become = true
	rc.CheckMode = true
	changed, res := putFile(rc, []byte("k=v\n"), "/etc/app.conf", 0o644, false, true, "app", "")
	if res != nil && res.Failed {
		t.Fatalf("owner-only 不应失败: %+v", res)
	}
	if changed {
		t.Fatal("owner 已收敛（group 未管理）时不应预估变更")
	}
}

// 对照：owner 确实漂移时仍应发现（只比较显式给出的一方，不放松判定）。
func TestPutFileOwnerOnlyStillDetectsDrift(t *testing.T) {
	rc, _ := ownerFake(t, "root app")
	rc.Become = true
	rc.CheckMode = true
	changed, _ := putFile(rc, []byte("k=v\n"), "/etc/app.conf", 0o644, false, true, "app", "")
	if !changed {
		t.Fatal("owner 漂移（root≠app）应预估变更")
	}
}
