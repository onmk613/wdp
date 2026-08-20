package module

import (
	"strings"
	"testing"

	"wdp/internal/connection"
)

// groupShell 模拟 getent/groupadd/groupmod/groupdel。
type groupShell struct {
	gids map[string]string // 组名 → GID
	runs []string          // 变更命令记录
}

func newGroupRC(t *testing.T) (*RunContext, *groupShell) {
	t.Helper()
	rc, _ := newTestRC(t)
	sh := &groupShell{gids: map[string]string{}}
	fake := rc.Conn.(*connection.Fake)
	fake.ExecFn = func(req connection.ExecRequest) (connection.ExecResult, error) {
		s := req.Script
		switch {
		case strings.Contains(s, "groupdel"):
			delete(sh.gids, firstQuoted(s))
			sh.runs = append(sh.runs, s)
			return connection.ExecResult{Code: 0}, nil
		case strings.Contains(s, "groupmod"):
			fields := strings.Fields(s)
			gid, name := fields[2], strings.Trim(fields[3], "'")
			sh.gids[name] = gid
			sh.runs = append(sh.runs, s)
			return connection.ExecResult{Code: 0}, nil
		case strings.Contains(s, "groupadd"):
			fields := strings.Fields(s)
			gid, name := "1000", strings.Trim(fields[len(fields)-1], "'")
			for i, f := range fields {
				if f == "-g" && i+1 < len(fields) {
					gid = fields[i+1]
				}
			}
			sh.gids[name] = gid
			sh.runs = append(sh.runs, s)
			return connection.ExecResult{Code: 0}, nil
		case strings.Contains(s, "getent group") && strings.Contains(s, "cut -d: -f3"):
			gid, ok := sh.gids[firstQuoted(s)]
			if !ok {
				return connection.ExecResult{Code: 2}, nil
			}
			return connection.ExecResult{Code: 0, Stdout: gid + "\n"}, nil
		case strings.Contains(s, "getent group") && strings.Contains(s, ">/dev/null"):
			if _, ok := sh.gids[firstQuoted(s)]; !ok {
				return connection.ExecResult{Code: 2}, nil
			}
			return connection.ExecResult{Code: 0}, nil
		default:
			return connection.ExecResult{Code: 0}, nil
		}
	}
	return rc, sh
}

func TestGroupCreate(t *testing.T) {
	rc, sh := newGroupRC(t)
	rc.Become = true
	mod := &GroupModule{}

	r := mod.Run(rc, map[string]any{"name": "deploy", "gid": 2000}, "")
	if r.Failed {
		t.Fatalf("创建失败: %s", r.Msg)
	}
	if !r.Changed || len(sh.runs) != 1 || sh.runs[0] != "groupadd -g 2000 'deploy'" {
		t.Fatalf("groupadd: %+v runs=%v", r, sh.runs)
	}
	// 幂等：同名同 GID 不再变更
	r = mod.Run(rc, map[string]any{"name": "deploy", "gid": 2000}, "")
	if r.Failed || r.Changed {
		t.Fatalf("幂等: %+v", r)
	}
	if len(sh.runs) != 1 {
		t.Fatalf("不应再次变更: %v", sh.runs)
	}
	// 系统组
	r = mod.Run(rc, map[string]any{"name": "sysgrp", "system": true}, "")
	if r.Failed || sh.runs[len(sh.runs)-1] != "groupadd -r 'sysgrp'" {
		t.Fatalf("系统组: %+v runs=%v", r, sh.runs)
	}
}

func TestGroupGidDrift(t *testing.T) {
	rc, sh := newGroupRC(t)
	rc.Become = true
	sh.gids["app"] = "1005"
	mod := &GroupModule{}

	r := mod.Run(rc, map[string]any{"name": "app", "gid": 2010}, "")
	if r.Failed || !r.Changed {
		t.Fatalf("GID 漂移: %+v", r)
	}
	if len(sh.runs) != 1 || sh.runs[0] != "groupmod -g 2010 'app'" {
		t.Fatalf("groupmod: %v", sh.runs)
	}
	// 库已应用 → 幂等
	r = mod.Run(rc, map[string]any{"name": "app", "gid": 2010}, "")
	if r.Failed || r.Changed {
		t.Fatalf("幂等: %+v", r)
	}
}

func TestGroupAbsent(t *testing.T) {
	rc, sh := newGroupRC(t)
	rc.Become = true
	sh.gids["legacy"] = "999"
	mod := &GroupModule{}

	r := mod.Run(rc, map[string]any{"name": "legacy", "state": "absent"}, "")
	if r.Failed || !r.Changed || len(sh.runs) != 1 || sh.runs[0] != "groupdel 'legacy'" {
		t.Fatalf("删除: %+v runs=%v", r, sh.runs)
	}
	// 幂等
	r = mod.Run(rc, map[string]any{"name": "legacy", "state": "absent"}, "")
	if r.Failed || r.Changed {
		t.Fatalf("幂等删除: %+v", r)
	}
	// 缺 become：变更应失败
	sh.gids["x"] = "1"
	rc2, sh2 := newGroupRC(t)
	sh2.gids["x"] = "1"
	r2 := mod.Run(rc2, map[string]any{"name": "x", "state": "absent"}, "")
	if !r2.Failed || !strings.Contains(r2.Msg, "become") {
		t.Fatalf("缺 become 应失败: %+v", r2)
	}
	rc3, _ := newGroupRC(t)
	r3 := mod.Run(rc3, map[string]any{"name": "y"}, "")
	if !r3.Failed || !strings.Contains(r3.Msg, "become") {
		t.Fatalf("创建缺 become 应失败: %+v", r3)
	}
}

func TestGroupCheckMode(t *testing.T) {
	rc, sh := newGroupRC(t)
	rc.Become = true
	rc.CheckMode = true
	rc.DiffMode = true
	mod := &GroupModule{}

	// 创建预估
	r := mod.Run(rc, map[string]any{"name": "deploy", "gid": 2000}, "")
	if r.Failed || !r.Changed || !strings.Contains(r.Msg, "[check] 组 deploy 将创建") {
		t.Fatalf("创建预估: %+v", r)
	}
	if !strings.Contains(r.Diff, "+ gid 2000") {
		t.Fatalf("创建 diff: %q", r.Diff)
	}
	// GID 漂移预估
	sh.gids["app"] = "1005"
	r = mod.Run(rc, map[string]any{"name": "app", "gid": 2010}, "")
	if r.Failed || !r.Changed || !strings.Contains(r.Msg, "将调整 gid") {
		t.Fatalf("漂移预估: %+v", r)
	}
	if !strings.Contains(r.Diff, "- gid 1005") || !strings.Contains(r.Diff, "+ gid 2010") {
		t.Fatalf("漂移 diff: %q", r.Diff)
	}
	// 删除预估
	r = mod.Run(rc, map[string]any{"name": "app", "state": "absent"}, "")
	if r.Failed || !r.Changed || !strings.Contains(r.Msg, "将删除") {
		t.Fatalf("删除预估: %+v", r)
	}
	if len(sh.runs) != 0 {
		t.Fatalf("check 不应变更: %v", sh.runs)
	}
	// 已是目标状态
	sh.gids["ok"] = "3000"
	r = mod.Run(rc, map[string]any{"name": "ok", "gid": 3000}, "")
	if r.Failed || r.Changed {
		t.Fatalf("无漂移: %+v", r)
	}
}

func TestGroupValidation(t *testing.T) {
	rc, _ := newGroupRC(t)
	mod := &GroupModule{}
	if r := mod.Run(rc, nil, ""); !r.Failed {
		t.Fatal("缺 name 应失败")
	}
	if r := mod.Run(rc, map[string]any{"name": "g", "state": "weird"}, ""); !r.Failed || !strings.Contains(r.Msg, "state") {
		t.Fatalf("非法 state: %+v", r)
	}
}
