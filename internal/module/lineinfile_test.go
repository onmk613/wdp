package module

import (
	"strings"
	"testing"
)

func TestLineinfileAppendIdempotent(t *testing.T) {
	rc, fake := newTestRC(t)
	fake.Files["/etc/app.conf"] = []byte("a=1\nb=2\n")
	mod := &LineinfileModule{}

	r1 := mod.Run(rc, map[string]any{"path": "/etc/app.conf", "line": "c=3"}, "")
	if r1.Failed {
		t.Fatalf("追加失败: %s", r1.Msg)
	}
	if !r1.Changed {
		t.Fatal("追加新行应 changed")
	}
	if got, _ := fake.File("/etc/app.conf"); got != "a=1\nb=2\nc=3\n" {
		t.Fatalf("内容 %q", got)
	}

	// 幂等：精确行已存在 → 不变
	r2 := mod.Run(rc, map[string]any{"path": "/etc/app.conf", "line": "c=3"}, "")
	if r2.Failed || r2.Changed {
		t.Fatalf("行已存在不应 changed: %+v", r2)
	}
	if got, _ := fake.File("/etc/app.conf"); got != "a=1\nb=2\nc=3\n" {
		t.Fatalf("幂等破坏内容: %q", got)
	}
}

func TestLineinfileRegexpReplaceFirst(t *testing.T) {
	rc, fake := newTestRC(t)
	fake.Files["/etc/app.conf"] = []byte("key=1\nother\nkey=2\n")
	mod := &LineinfileModule{}

	r := mod.Run(rc, map[string]any{"path": "/etc/app.conf", "regexp": "^key=", "line": "key=9"}, "")
	if r.Failed || !r.Changed {
		t.Fatalf("regexp 替换: %+v", r)
	}
	if got, _ := fake.File("/etc/app.conf"); got != "key=9\nother\nkey=2\n" {
		t.Fatalf("应只替换首个匹配: %q", got)
	}
	// 幂等：首个匹配行已是目标行
	r = mod.Run(rc, map[string]any{"path": "/etc/app.conf", "regexp": "^key=", "line": "key=9"}, "")
	if r.Failed || r.Changed {
		t.Fatalf("幂等: %+v", r)
	}
}

func TestLineinfileInsertAfter(t *testing.T) {
	rc, fake := newTestRC(t)
	fake.Files["/etc/rc.local"] = []byte("start\nexit 0\n")
	mod := &LineinfileModule{}

	r := mod.Run(rc, map[string]any{
		"path": "/etc/rc.local", "line": "/usr/local/bin/agent.sh", "insertafter": "^start",
	}, "")
	if r.Failed || !r.Changed {
		t.Fatalf("insertafter: %+v", r)
	}
	if got, _ := fake.File("/etc/rc.local"); got != "start\n/usr/local/bin/agent.sh\nexit 0\n" {
		t.Fatalf("应插入到最后一个匹配行之后: %q", got)
	}
}

func TestLineinfileAbsent(t *testing.T) {
	rc, fake := newTestRC(t)
	fake.Files["/etc/app.conf"] = []byte("keep\nx1\nx2\nkeep2\n")
	mod := &LineinfileModule{}

	// regexp 删除全部匹配行
	r := mod.Run(rc, map[string]any{"path": "/etc/app.conf", "regexp": "^x", "state": "absent"}, "")
	if r.Failed || !r.Changed {
		t.Fatalf("absent+regexp: %+v", r)
	}
	if got, _ := fake.File("/etc/app.conf"); got != "keep\nkeep2\n" {
		t.Fatalf("应删除全部匹配: %q", got)
	}
	// 幂等
	r = mod.Run(rc, map[string]any{"path": "/etc/app.conf", "regexp": "^x", "state": "absent"}, "")
	if r.Failed || r.Changed {
		t.Fatalf("absent 幂等: %+v", r)
	}
	// 精确行删除
	r = mod.Run(rc, map[string]any{"path": "/etc/app.conf", "line": "keep2", "state": "absent"}, "")
	if r.Failed || !r.Changed {
		t.Fatalf("absent+line: %+v", r)
	}
	if got, _ := fake.File("/etc/app.conf"); got != "keep\n" {
		t.Fatalf("精确删除: %q", got)
	}
}

func TestLineinfileCreate(t *testing.T) {
	rc, fake := newTestRC(t)
	mod := &LineinfileModule{}

	r := mod.Run(rc, map[string]any{"path": "/etc/new.conf", "line": "first=1", "create": true}, "")
	if r.Failed || !r.Changed {
		t.Fatalf("create: %+v", r)
	}
	if got, _ := fake.File("/etc/new.conf"); got != "first=1\n" {
		t.Fatalf("新建内容 %q", got)
	}
	if fake.Modes["/etc/new.conf"].Perm() != 0o644 {
		t.Fatalf("新建缺省权限: %v", fake.Modes["/etc/new.conf"].Perm())
	}

	// 缺 create 且文件不存在 → 失败
	if r := mod.Run(rc, map[string]any{"path": "/etc/gone.conf", "line": "x"}, ""); !r.Failed {
		t.Fatal("文件不存在且未开 create 应失败")
	}
	// absent 且文件不存在 → 直接成功不变更（带 regexp 满足参数约束）
	if r := mod.Run(rc, map[string]any{"path": "/etc/gone.conf", "regexp": ".", "state": "absent"}, ""); r.Failed || r.Changed {
		t.Fatalf("absent 缺文件: %+v", r)
	}
}

func TestLineinfilePreserveModeAndBackup(t *testing.T) {
	rc, fake := newTestRC(t)
	fake.Files["/etc/app.conf"] = []byte("a\n")
	fake.Modes["/etc/app.conf"] = 0o600
	mod := &LineinfileModule{}

	r := mod.Run(rc, map[string]any{"path": "/etc/app.conf", "line": "b", "backup": true, "mode": "0666"}, "")
	if r.Failed || !r.Changed {
		t.Fatalf("备份写入: %+v", r)
	}
	// 已有文件保持原权限（mode 仅 create 时生效）
	if fake.Modes["/etc/app.conf"].Perm() != 0o600 {
		t.Fatalf("应保持原权限 0600: %v", fake.Modes["/etc/app.conf"].Perm())
	}
	// Fake 不模拟 cp，改验证备份命令确已下发
	backupIssued := false
	for _, req := range fake.ExecLog {
		if strings.Contains(req.Script, "cp -a") && strings.Contains(req.Script, "/etc/app.conf.bak.") {
			backupIssued = true
		}
	}
	if !backupIssued {
		t.Fatal("backup 应下发备份命令")
	}
}

func TestLineinfileCheckMode(t *testing.T) {
	rc, fake := newTestRC(t)
	fake.Files["/etc/app.conf"] = []byte("a\n")
	rc.CheckMode = true
	rc.DiffMode = true
	mod := &LineinfileModule{}

	r := mod.Run(rc, map[string]any{"path": "/etc/app.conf", "line": "b"}, "")
	if r.Failed {
		t.Fatalf("check 失败: %s", r.Msg)
	}
	if !r.Changed || !strings.Contains(r.Msg, "[check]") {
		t.Fatalf("check 应预估变更: %+v", r)
	}
	if r.Diff == "" {
		t.Fatal("diff 模式应产出内容差异")
	}
	if got, _ := fake.File("/etc/app.conf"); got != "a\n" {
		t.Fatalf("check 不应写远端: %q", got)
	}
	// 已是期望状态
	r = mod.Run(rc, map[string]any{"path": "/etc/app.conf", "line": "a"}, "")
	if r.Failed || r.Changed {
		t.Fatalf("check 幂等: %+v", r)
	}
}

func TestLineinfileArgValidation(t *testing.T) {
	rc, _ := newTestRC(t)
	mod := &LineinfileModule{}
	if r := mod.Run(rc, map[string]any{"line": "x"}, ""); !r.Failed {
		t.Fatal("缺 path 应失败")
	}
	if r := mod.Run(rc, map[string]any{"path": "/x"}, ""); !r.Failed {
		t.Fatal("present 缺 line 应失败")
	}
	if r := mod.Run(rc, map[string]any{"path": "/x", "state": "absent"}, ""); !r.Failed {
		t.Fatal("absent 缺 line/regexp 应失败")
	}
	if r := mod.Run(rc, map[string]any{"path": "/x", "line": "a", "regexp": "["}, ""); !r.Failed {
		t.Fatal("非法 regexp 应失败")
	}
	if r := mod.Run(rc, map[string]any{"path": "/x", "line": "a", "insertafter": "("}, ""); !r.Failed {
		t.Fatal("非法 insertafter 应失败")
	}
	if r := mod.Run(rc, map[string]any{"path": "/x", "line": "a", "state": "weird"}, ""); !r.Failed {
		t.Fatal("非法 state 应失败")
	}
	if r := mod.Run(rc, map[string]any{"path": "/x", "line": "a", "owner": "root"}, ""); !r.Failed {
		t.Fatal("owner 未提权应失败")
	}
}
