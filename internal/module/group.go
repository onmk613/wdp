package module

import (
	"fmt"
	"strconv"
	"strings"

	"wdp/internal/i18n"
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
	return i18n.T("manage system groups (create/delete/GID correction)", "管理系统组（创建/删除/GID 校正）")
}

// Params 参数文档。
func (m *GroupModule) Params() []ParamDoc {
	return []ParamDoc{
		{Name: "name", Type: "string", Desc: "组名"},
		{Name: "state", Type: "string", Default: "present", Desc: "present 创建/校正；absent 删除"},
		{Name: "gid", Type: "int", Desc: "GID（已存在组漂移时经 groupmod -g 校正）"},
		{Name: "system", Type: "bool", Default: "false", Desc: "创建系统组（groupadd -r，仅创建时生效）"},
	}
}

// Example 示例任务。
func (m *GroupModule) Example() string {
	return `# 创建部署组
- name: 创建 deploy 组
  become: true
  group:
    name: deploy
    gid: 2000

# 修正 GID 漂移（变化时才执行 groupmod）
- name: 校正 app 组 GID
  become: true
  group:
    name: app
    gid: 2010

# 删除组（不可回滚）
- name: 移除废弃组
  become: true
  group:
    name: legacy
    state: absent`
}

// Run 执行组管理。
func (m *GroupModule) Run(rc *RunContext, args map[string]any, free string) *Result {
	name, ok := argStr(args, "name")
	if !ok || name == "" {
		return Fail("%s", i18n.T("group requires a name parameter", "group 需要 name 参数"))
	}
	state, ok := parseState(args, "present", "present", "absent")
	if !ok {
		return Fail(i18n.T("unsupported state %q (options: present/absent)", "不支持的 state %q（可选: present/absent）"), state)
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
			return &Result{Msg: fmt.Sprintf(i18n.T("group %s does not exist", "组 %s 不存在"), name)}
		}
		if !rc.Become {
			return Fail("%s", i18n.T("deleting a group requires become: true", "删除组需要 become: true"))
		}
		if rc.CheckMode {
			res := &Result{Changed: true, Msg: fmt.Sprintf("[check] 组 %s 将删除", name)}
			if rc.DiffMode {
				res.Diff = fmt.Sprintf(i18n.T("- %s (group will be deleted)", "- %s（组将删除）"), name)
			}
			return res
		}
		if out, bad := rc.exec(fmt.Sprintf("groupdel %s", shellquote.Quote(name))); bad != nil {
			return bad
		} else if out.Code != 0 {
			return Fail(i18n.T("groupdel failed: %s", "groupdel 失败: %s"), firstLine(out.Stderr))
		}
		return &Result{Changed: true, Msg: fmt.Sprintf(i18n.T("group %s deleted", "组 %s 已删除"), name)}
	}

	// present：缺失则创建
	if !exists {
		if !rc.Become {
			return Fail("%s", i18n.T("creating a group requires become: true", "创建组需要 become: true"))
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
			res := &Result{Changed: true, Msg: fmt.Sprintf("[check] 组 %s 将创建", name)}
			if rc.DiffMode {
				var d []string
				d = append(d, fmt.Sprintf(i18n.T("+ %s (new group%s)", "+ %s（新建组%s）"), name, boolTo(system, i18n.T(", system group", "，系统组"), "")))
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
			return Fail(i18n.T("groupadd failed: %s", "groupadd 失败: %s"), firstLine(out.Stderr))
		}
		return &Result{Changed: true, Msg: fmt.Sprintf(i18n.T("group %s created", "组 %s 已创建"), name)}
	}

	// present 且已存在：仅校正 GID 漂移
	if hasGID {
		cur, bad := groupGID(rc, name)
		if bad != nil {
			return bad
		}
		if want := strconv.Itoa(gid); cur != want {
			if !rc.Become {
				return Fail(i18n.T("group %s GID drift (%s → %s), correcting requires become: true", "组 %s GID 漂移（%s → %s），校正需要 become: true"), name, cur, want)
			}
			if rc.CheckMode {
				return &Result{
					Changed: true,
					Msg:     fmt.Sprintf("[check] 组 %s: 将调整 gid（%s → %s）", name, cur, want),
					Diff:    joinLines([]string{"- gid " + cur, "+ gid " + want}),
				}
			}
			script := fmt.Sprintf("groupmod -g %d %s", gid, shellquote.Quote(name))
			if out, bad := rc.exec(script); bad != nil {
				return bad
			} else if out.Code != 0 {
				return Fail(i18n.T("groupmod failed: %s", "groupmod 失败: %s"), firstLine(out.Stderr))
			}
			return &Result{Changed: true, Msg: fmt.Sprintf(i18n.T("group %s: GID adjusted to %d", "组 %s: GID 已调整为 %d"), name, gid)}
		}
	}
	return &Result{Msg: fmt.Sprintf(i18n.T("group %s is already in the target state", "组 %s 已是目标状态"), name)}
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
		return "", Fail(i18n.T("failed to read group %s GID", "读取组 %s GID 失败"), name)
	}
	return strings.TrimSpace(out.Stdout), nil
}
