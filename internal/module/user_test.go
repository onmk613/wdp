package module

import (
	"strings"
	"testing"

	"wdp/internal/connection"
)

// fakeUser 模拟一条 passwd/组记录。
type fakeUser struct {
	uid      string
	primary  string
	groups   []string // 含主组的全部组
	home     string
	shell    string
	password string
}

// userShell 模拟 id/getent/useradd/usermod/userdel。
type userShell struct {
	users map[string]*fakeUser
	runs  []string // useradd/usermod/userdel 命令记录
}

func newUserRC(t *testing.T) (*RunContext, *userShell) {
	t.Helper()
	rc, _ := newTestRC(t)
	sh := &userShell{users: map[string]*fakeUser{}}
	fake := rc.Conn.(*connection.Fake)
	fake.ExecFn = func(req connection.ExecRequest) (connection.ExecResult, error) {
		s := req.Script
		switch {
		case strings.Contains(s, "userdel"):
			name := firstQuoted(s)
			delete(sh.users, name)
			sh.runs = append(sh.runs, s)
			return connection.ExecResult{Code: 0}, nil
		case strings.Contains(s, "useradd"), strings.Contains(s, "usermod"):
			flags, name := parseUserCmd(s)
			if strings.Contains(s, "useradd") {
				primary := flags["g"]
				if primary == "" {
					primary = name
				}
				groups := append([]string{}, primary)
				if csv := flags["G"]; csv != "" {
					groups = append(groups, strings.Split(csv, ",")...)
				}
				home := flags["d"]
				if home == "" {
					home = "/home/" + name
				}
				sh.users[name] = &fakeUser{
					uid: orDefault(flags["u"], "1000"), primary: primary, groups: sortUnique(groups),
					home: home, shell: orDefault(flags["s"], "/bin/sh"), password: flags["p"],
				}
			} else {
				u := sh.users[name]
				if u == nil {
					return connection.ExecResult{Code: 6, Stderr: "usermod: user does not exist"}, nil
				}
				if v := flags["u"]; v != "" {
					u.uid = v
				}
				if v := flags["g"]; v != "" {
					u.primary = v
				}
				if v := flags["G"]; v != "" {
					u.groups = sortUnique(append(strings.Split(v, ","), u.primary))
				}
				if v := flags["s"]; v != "" {
					u.shell = v
				}
				if v := flags["d"]; v != "" {
					u.home = v
				}
			}
			sh.runs = append(sh.runs, s)
			return connection.ExecResult{Code: 0}, nil
		case strings.Contains(s, "getent passwd"):
			u := sh.users[firstQuoted(s)]
			if u == nil {
				return connection.ExecResult{Code: 2}, nil
			}
			return connection.ExecResult{Code: 0, Stdout: u.home + ":" + u.shell + "\n"}, nil
		case strings.Contains(s, "id -gn"):
			u := sh.users[firstQuoted(s)]
			if u == nil {
				return connection.ExecResult{Code: 1}, nil
			}
			return connection.ExecResult{Code: 0, Stdout: u.primary + "\n"}, nil
		case strings.Contains(s, "id -nG"):
			u := sh.users[firstQuoted(s)]
			if u == nil {
				return connection.ExecResult{Code: 1}, nil
			}
			return connection.ExecResult{Code: 0, Stdout: strings.Join(u.groups, " ") + "\n"}, nil
		case strings.Contains(s, "id -u") && strings.Contains(s, ">/dev/null"):
			if sh.users[firstQuoted(s)] == nil {
				return connection.ExecResult{Code: 1}, nil
			}
			return connection.ExecResult{Code: 0}, nil
		case strings.Contains(s, "id -u"):
			u := sh.users[firstQuoted(s)]
			if u == nil {
				return connection.ExecResult{Code: 1}, nil
			}
			return connection.ExecResult{Code: 0, Stdout: u.uid + "\n"}, nil
		default:
			return connection.ExecResult{Code: 0}, nil
		}
	}
	return rc, sh
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// parseUserCmd 解析 useradd/usermod 命令行，返回（选项表、用户名）。
func parseUserCmd(script string) (map[string]string, string) {
	valOpts := map[string]bool{"u": true, "g": true, "G": true, "s": true, "d": true, "p": true}
	flags := map[string]string{}
	name := ""
	fields := strings.Fields(script)
	for i := 1; i < len(fields); i++ {
		f := fields[i]
		if strings.HasPrefix(f, "-") && len(f) >= 2 {
			key := f[1:]
			if valOpts[key] && i+1 < len(fields) {
				i++
				flags[key] = strings.Trim(fields[i], "'")
			} else {
				flags[key] = ""
			}
		} else {
			name = strings.Trim(f, "'")
		}
	}
	return flags, name
}

func TestUserCreate(t *testing.T) {
	rc, sh := newUserRC(t)
	rc.Become = true
	mod := &UserModule{}

	r := mod.Run(rc, map[string]any{
		"name": "deploy", "shell": "/sbin/nologin", "home": "/home/deploy",
		"groups": []any{"docker", "wheel"}, "uid": 1500, "group": "deploy", "system": true,
	}, "")
	if r.Failed {
		t.Fatalf("创建失败: %s", r.Msg)
	}
	if !r.Changed {
		t.Fatal("创建应 changed")
	}
	want := "useradd -r -u 1500 -g 'deploy' -G 'docker,wheel' -s '/sbin/nologin' -d '/home/deploy' -m 'deploy'"
	if len(sh.runs) != 1 || sh.runs[0] != want {
		t.Fatalf("useradd 命令:\n got %q\nwant %q", sh.runs[0], want)
	}
	// 再跑一次：无漂移（uid/shell/home/组均与库一致）→ 不变更
	r = mod.Run(rc, map[string]any{
		"name": "deploy", "shell": "/sbin/nologin", "home": "/home/deploy",
		"groups": []any{"docker", "wheel"}, "uid": 1500,
	}, "")
	if r.Failed || r.Changed {
		t.Fatalf("幂等: %+v", r)
	}
	if len(sh.runs) != 1 {
		t.Fatalf("不应再次变更: %v", sh.runs)
	}
}

func TestUserCreateRequiresBecome(t *testing.T) {
	rc, _ := newUserRC(t)
	mod := &UserModule{}
	r := mod.Run(rc, map[string]any{"name": "deploy"}, "")
	if !r.Failed || !strings.Contains(r.Msg, "become") {
		t.Fatalf("缺 become 应失败: %+v", r)
	}
	// 已存在且无漂移时不需要 become
	rc2, sh := newUserRC(t)
	sh.users["deploy"] = &fakeUser{uid: "1000", primary: "deploy", groups: []string{"deploy"}, home: "/home/deploy", shell: "/bin/sh"}
	r2 := mod.Run(rc2, map[string]any{"name": "deploy"}, "")
	if r2.Failed || r2.Changed {
		t.Fatalf("无漂移不应需要 become: %+v", r2)
	}
}

func TestUserDriftUsermod(t *testing.T) {
	rc, sh := newUserRC(t)
	rc.Become = true
	sh.users["app"] = &fakeUser{uid: "1000", primary: "app", groups: []string{"app"}, home: "/home/app", shell: "/bin/sh"}
	mod := &UserModule{}

	// shell + groups 漂移，uid 一致 → usermod 只带漂移字段
	r := mod.Run(rc, map[string]any{"name": "app", "shell": "/bin/bash", "groups": "docker", "uid": 1000}, "")
	if r.Failed || !r.Changed {
		t.Fatalf("漂移校正: %+v", r)
	}
	want := "usermod -G 'docker' -s '/bin/bash' 'app'"
	if len(sh.runs) != 1 || sh.runs[0] != want {
		t.Fatalf("usermod 命令:\n got %q\nwant %q", sh.runs[0], want)
	}
	// 库已应用变更 → 幂等
	r = mod.Run(rc, map[string]any{"name": "app", "shell": "/bin/bash", "groups": "docker", "uid": 1000}, "")
	if r.Failed || r.Changed {
		t.Fatalf("幂等: %+v", r)
	}

	// uid + home 漂移 → usermod -u -d -m
	sh.users["app"].uid = "1001"
	sh.users["app"].home = "/var/app"
	r = mod.Run(rc, map[string]any{"name": "app", "uid": 1000, "home": "/opt/app"}, "")
	if r.Failed || !r.Changed {
		t.Fatalf("uid/home 漂移: %+v", r)
	}
	last := sh.runs[len(sh.runs)-1]
	if last != "usermod -u 1000 -d '/opt/app' -m 'app'" {
		t.Fatalf("usermod 命令: %q", last)
	}
}

func TestUserAbsent(t *testing.T) {
	rc, sh := newUserRC(t)
	rc.Become = true
	sh.users["leaver"] = &fakeUser{uid: "1001", primary: "leaver", groups: []string{"leaver"}, home: "/home/leaver", shell: "/bin/sh"}
	mod := &UserModule{}

	r := mod.Run(rc, map[string]any{"name": "leaver", "state": "absent"}, "")
	if r.Failed || !r.Changed {
		t.Fatalf("删除: %+v", r)
	}
	if len(sh.runs) != 1 || sh.runs[0] != "userdel -r 'leaver'" {
		t.Fatalf("userdel: %v", sh.runs)
	}
	// 再删：已不存在
	r = mod.Run(rc, map[string]any{"name": "leaver", "state": "absent"}, "")
	if r.Failed || r.Changed {
		t.Fatalf("幂等删除: %+v", r)
	}
	// 缺 become：删除应失败
	rc2, sh2 := newUserRC(t)
	sh2.users["x"] = &fakeUser{uid: "1", primary: "x", groups: []string{"x"}, home: "/home/x", shell: "/bin/sh"}
	r2 := (&UserModule{}).Run(rc2, map[string]any{"name": "x", "state": "absent"}, "")
	if !r2.Failed || !strings.Contains(r2.Msg, "become") {
		t.Fatalf("删除缺 become 应失败: %+v", r2)
	}
}

func TestUserCheckMode(t *testing.T) {
	rc, sh := newUserRC(t)
	rc.Become = true
	rc.CheckMode = true
	rc.DiffMode = true
	mod := &UserModule{}

	// 创建预估
	r := mod.Run(rc, map[string]any{"name": "deploy", "uid": 1500, "shell": "/sbin/nologin"}, "")
	if r.Failed || !r.Changed || !strings.Contains(r.Msg, "[check] 用户 deploy 将创建") {
		t.Fatalf("创建预估: %+v", r)
	}
	if !strings.Contains(r.Diff, "+ uid 1500") || !strings.Contains(r.Diff, "+ shell /sbin/nologin") {
		t.Fatalf("创建 diff: %q", r.Diff)
	}

	// 漂移预估：报告逐属性，不执行 usermod
	sh.users["app"] = &fakeUser{uid: "1000", primary: "app", groups: []string{"app"}, home: "/home/app", shell: "/bin/sh"}
	r = mod.Run(rc, map[string]any{"name": "app", "shell": "/bin/bash"}, "")
	if r.Failed || !r.Changed || !strings.Contains(r.Msg, "将调整 shell") {
		t.Fatalf("漂移预估: %+v", r)
	}
	if !strings.Contains(r.Diff, "- shell /bin/sh") || !strings.Contains(r.Diff, "+ shell /bin/bash") {
		t.Fatalf("漂移 diff: %q", r.Diff)
	}
	for _, c := range sh.runs {
		if strings.Contains(c, "useradd") || strings.Contains(c, "usermod") {
			t.Fatalf("check 不应变更: %v", sh.runs)
		}
	}

	// 删除预估
	r = mod.Run(rc, map[string]any{"name": "app", "state": "absent"}, "")
	if r.Failed || !r.Changed || !strings.Contains(r.Msg, "将删除") {
		t.Fatalf("删除预估: %+v", r)
	}
	if !strings.Contains(r.Diff, "- app") {
		t.Fatalf("删除 diff: %q", r.Diff)
	}
}

func TestUserValidation(t *testing.T) {
	rc, _ := newUserRC(t)
	mod := &UserModule{}
	if r := mod.Run(rc, nil, ""); !r.Failed {
		t.Fatal("缺 name 应失败")
	}
	if r := mod.Run(rc, map[string]any{"name": "a", "state": "weird"}, ""); !r.Failed || !strings.Contains(r.Msg, "state") {
		t.Fatalf("非法 state: %+v", r)
	}
}
