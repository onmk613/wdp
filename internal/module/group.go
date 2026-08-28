package module

import (
	"fmt"
	"strconv"
	"strings"

	"wdp/internal/shellquote"
)

func init() {
	Register(&GroupModule{})
}

// GroupModule 管理系统组（创建/删除/GID 校正）。
// 系统级变更不可回滚（与 package/user 模块同样视为不可逆操作，不登记回滚日志）。
type GroupModule struct{}

// Name 模块名。
func (m *GroupModule) Name() string { return "group" }

// Desc 模块说明。
func (m *GroupModule) Desc() string {
	return "manage system groups (create/delete/GID correction)"
}

// Params 参数文档。
func (m *GroupModule) Params() []ParamDoc {
	return []ParamDoc{
		{Name: "name", Type: "string", Desc: "group name"},
		{Name: "state", Type: "string", Default: "present", Desc: "present creates/fixes drift; absent removes"},
		{Name: "gid", Type: "int", Desc: "GID (drift on existing groups is fixed via groupmod -g)"},
		{Name: "system", Type: "bool", Default: "false", Desc: "create as a system group (groupadd -r, only at creation)"},
	}
}

// Example 示例任务。
func (m *GroupModule) Example() string {
	return `# create a deploy group
- name: create the deploy group
  become: true
  group:
    name: deploy
    gid: 2000

# fix GID drift (groupmod only runs when changed)
- name: fix the app group GID
  become: true
  group:
    name: app
    gid: 2010

# remove a group (not rollback-able)
- name: remove an obsolete group
  become: true
  group:
    name: legacy
    state: absent`
}

// Run 执行组管理。
func (m *GroupModule) Run(rc *RunContext, args map[string]any, _ string) *Result {
	name, ok := argStr(args, "name")
	if !ok || name == "" {
		return Fail("%s", "group requires a name parameter")
	}
	state, ok := parseState(args, "present", "present", "absent")
	if !ok {
		return Fail("unsupported state %q (options: present/absent)", state)
	}
	gid, hasGID := argInt(args, "gid")
	system, _ := argBool(args, "system")

	exists, bad := groupExists(rc, name)
	if bad != nil {
		return bad
	}

	// absent：存在则删除
	if state == "absent" {
		if !exists {
			return &Result{Msg: fmt.Sprintf("group %s does not exist", name)}
		}
		if !rc.Become {
			return Fail("%s", "deleting a group requires become: true")
		}
		if rc.CheckMode {
			res := &Result{Changed: true, Msg: fmt.Sprintf("[check] group %s would be removed", name)}
			if rc.DiffMode {
				res.Diff = fmt.Sprintf("- %s (group will be deleted)", name)
			}
			return res
		}
		if out, bad := rc.exec(fmt.Sprintf("groupdel %s", shellquote.Quote(name))); bad != nil {
			return bad
		} else if out.Code != 0 {
			return Fail("groupdel failed: %s", firstLine(out.Stderr))
		}
		return &Result{Changed: true, Msg: fmt.Sprintf("group %s deleted", name)}
	}

	// present：缺失则创建
	if !exists {
		if !rc.Become {
			return Fail("%s", "creating a group requires become: true")
		}
		var flags []string
		if system {
			flags = append(flags, "-r")
		}
		if hasGID {
			flags = append(flags, "-g", strconv.Itoa(gid))
		}
		script := fmt.Sprintf("groupadd %s %s", strings.Join(flags, " "), shellquote.Quote(name))
		if rc.CheckMode {
			res := &Result{Changed: true, Msg: fmt.Sprintf("[check] group %s would be created", name)}
			if rc.DiffMode {
				var d []string
				d = append(d, fmt.Sprintf("+ %s (new group%s)", name, boolTo(system, ", system group", "")))
				if hasGID {
					d = append(d, "+ gid "+strconv.Itoa(gid))
				}
				res.Diff = joinLines(d)
			}
			return res
		}
		if out, bad := rc.exec(script); bad != nil {
			return bad
		} else if out.Code != 0 {
			return Fail("groupadd failed: %s", firstLine(out.Stderr))
		}
		return &Result{Changed: true, Msg: fmt.Sprintf("group %s created", name)}
	}

	// present 且已存在：仅校正 GID 漂移
	if hasGID {
		cur, bad := groupGID(rc, name)
		if bad != nil {
			return bad
		}
		if want := strconv.Itoa(gid); cur != want {
			if !rc.Become {
				return Fail("group %s GID drift (%s → %s), correcting requires become: true", name, cur, want)
			}
			if rc.CheckMode {
				return &Result{
					Changed: true,
					Msg:     fmt.Sprintf("[check] group %s: would adjust gid (%s -> %s)", name, cur, want),
					Diff:    joinLines([]string{"- gid " + cur, "+ gid " + want}),
				}
			}
			script := fmt.Sprintf("groupmod -g %d %s", gid, shellquote.Quote(name))
			if out, bad := rc.exec(script); bad != nil {
				return bad
			} else if out.Code != 0 {
				return Fail("groupmod failed: %s", firstLine(out.Stderr))
			}
			return &Result{Changed: true, Msg: fmt.Sprintf("group %s: GID adjusted to %d", name, gid)}
		}
	}
	return &Result{Msg: fmt.Sprintf("group %s is already in the target state", name)}
}

// groupExists 探测组是否存在（getent group 退出码）。
func groupExists(rc *RunContext, name string) (bool, *Result) {
	out, bad := rc.exec(fmt.Sprintf("getent group %s >/dev/null 2>&1", shellquote.Quote(name)))
	if bad != nil {
		return false, bad
	}
	return out.Code == 0, nil
}

// groupGID 读取当前 GID（组必须存在，getent 字段 3）。
func groupGID(rc *RunContext, name string) (string, *Result) {
	script := fmt.Sprintf(`getent group %s | cut -d: -f3`, shellquote.Quote(name))
	out, bad := rc.exec(script)
	if bad != nil {
		return "", bad
	}
	if out.Code != 0 || strings.TrimSpace(out.Stdout) == "" {
		return "", Fail("failed to read group %s GID", name)
	}
	return strings.TrimSpace(out.Stdout), nil
}
