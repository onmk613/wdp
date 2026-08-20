package module

import (
	"fmt"
	"strconv"
	"strings"

	"wdp/internal/i18n"
	"wdp/internal/shellquote"
)

func init() {
	Register(&UserModule{})
}

// UserModule 管理系统用户（创建/删除/属性漂移校正）。
// 系统级变更不可回滚（与 package 模块同样视为不可逆操作，不登记回滚日志）。
type UserModule struct{}

// Name 模块名。
func (m *UserModule) Name() string { return "user" }

// Desc 模块说明。
func (m *UserModule) Desc() string {
	return i18n.T("manage system users (create/delete/attribute correction)", "管理系统用户（创建/删除/属性校正）")
}

// Params 参数文档。
func (m *UserModule) Params() []ParamDoc {
	return []ParamDoc{
		{Name: "name", Type: "string", Desc: "用户名"},
		{Name: "state", Type: "string", Default: "present", Desc: "present 创建/校正；absent 删除（含 home）"},
		{Name: "uid", Type: "int", Desc: "UID（已存在用户漂移时经 usermod -u 校正）"},
		{Name: "group", Type: "string", Desc: "主组名（漂移时经 usermod -g 校正）"},
		{Name: "groups", Type: "list", Desc: "附加组列表（漂移时经 usermod -G 整体覆盖）"},
		{Name: "shell", Type: "string", Desc: "登录 shell（漂移时经 usermod -s 校正）"},
		{Name: "home", Type: "string", Desc: "home 目录（漂移时经 usermod -d -m 迁移内容）"},
		{Name: "system", Type: "bool", Default: "false", Desc: "创建系统账号（useradd -r，仅创建时生效）"},
		{Name: "password", Type: "string", Desc: "crypt 哈希（useradd -p，仅创建时生效；已有用户改密请用 shell 模块）"},
	}
}

// Example 示例任务。
func (m *UserModule) Example() string {
	return `# 创建部署用户并加入 docker 组
- name: 创建 deploy 用户
  become: true
  user:
    name: deploy
    shell: /sbin/nologin
    home: /home/deploy
    groups: [docker]

# 修正漂移（uid/shell/组变化时才执行 usermod）
- name: 校正 app 用户 shell
  become: true
  user:
    name: app
    shell: /bin/bash

# 删除用户（含 home 目录，不可回滚）
- name: 移除离职账号
  become: true
  user:
    name: leaver
    state: absent`
}

// Run 执行用户管理。
func (m *UserModule) Run(rc *RunContext, args map[string]any, free string) *Result {
	name, ok := argStr(args, "name")
	if !ok || name == "" {
		return Fail("user 需要 name 参数")
	}
	state, _ := argStr(args, "state")
	if state == "" {
		state = "present"
	}
	switch state {
	case "present", "absent":
	default:
		return Fail("不支持的 state %q（可选: present/absent）", state)
	}
	uid, hasUID := argInt(args, "uid")
	primaryGroup, _ := argStr(args, "group")
	groups, hasGroups := argStrList(args, "groups")
	shell, _ := argStr(args, "shell")
	home, _ := argStr(args, "home")
	system, _ := argBool(args, "system")
	password, _ := argStr(args, "password")

	exists, bad := userExists(rc, name)
	if bad != nil {
		return bad
	}

	// absent：存在则删除
	if state == "absent" {
		if !exists {
			return &Result{Msg: fmt.Sprintf("用户 %s 不存在", name)}
		}
		if !rc.Become {
			return Fail("删除用户需要 become: true")
		}
		if rc.CheckMode {
			res := &Result{Changed: true, Msg: fmt.Sprintf("[check] 用户 %s 将删除（含 home）", name)}
			if rc.DiffMode {
				res.Diff = fmt.Sprintf("- %s（用户将删除）", name)
			}
			return res
		}
		if out, bad := rc.exec(fmt.Sprintf("userdel -r %s", shellquote.Quote(name))); bad != nil {
			return bad
		} else if out.Code != 0 {
			return Fail("userdel 失败: %s", firstLine(out.Stderr))
		}
		return &Result{Changed: true, Msg: fmt.Sprintf("用户 %s 已删除", name)}
	}

	// present：缺失则创建
	if !exists {
		if !rc.Become {
			return Fail("创建用户需要 become: true")
		}
		var flags []string
		if system {
			flags = append(flags, "-r")
		}
		if hasUID {
			flags = append(flags, "-u", strconv.Itoa(uid))
		}
		if primaryGroup != "" {
			flags = append(flags, "-g", shellquote.Quote(primaryGroup))
		}
		if hasGroups {
			flags = append(flags, "-G", shellquote.Quote(strings.Join(groups, ",")))
		}
		if shell != "" {
			flags = append(flags, "-s", shellquote.Quote(shell))
		}
		if home != "" {
			flags = append(flags, "-d", shellquote.Quote(home), "-m")
		}
		if password != "" {
			flags = append(flags, "-p", shellquote.Quote(password))
		}
		script := fmt.Sprintf("useradd %s %s", strings.Join(flags, " "), shellquote.Quote(name))
		if rc.CheckMode {
			res := &Result{Changed: true, Msg: fmt.Sprintf("[check] 用户 %s 将创建", name)}
			if rc.DiffMode {
				var d []string
				d = append(d, fmt.Sprintf("+ %s（新建用户%s）", name, boolTo(system, "，系统账号", "")))
				if hasUID {
					d = append(d, "+ uid "+strconv.Itoa(uid))
				}
				if primaryGroup != "" {
					d = append(d, "+ group "+primaryGroup)
				}
				if hasGroups {
					d = append(d, "+ groups "+strings.Join(groups, ","))
				}
				if shell != "" {
					d = append(d, "+ shell "+shell)
				}
				if home != "" {
					d = append(d, "+ home "+home)
				}
				res.Diff = joinLines(d)
			}
			return res
		}
		if out, bad := rc.exec(script); bad != nil {
			return bad
		} else if out.Code != 0 {
			return Fail("useradd 失败: %s", firstLine(out.Stderr))
		}
		return &Result{Changed: true, Msg: fmt.Sprintf("用户 %s 已创建", name)}
	}

	// present 且已存在：逐属性探测漂移，仅校正漂移项
	drift, diffLines, bad := userDrift(rc, name, hasUID, uid, primaryGroup, hasGroups, groups, shell, home)
	if bad != nil {
		return bad
	}
	if len(drift) == 0 {
		return &Result{Msg: fmt.Sprintf("用户 %s 已是目标状态", name)}
	}
	if !rc.Become {
		return Fail("用户 %s 属性漂移（%s），校正需要 become: true", name, strings.Join(drift, "、"))
	}
	if rc.CheckMode {
		return &Result{
			Changed: true,
			Msg:     fmt.Sprintf("[check] 用户 %s: 将调整 %s", name, strings.Join(drift, "、")),
			Diff:    joinLines(diffLines),
		}
	}
	var flags []string
	if hasUID && containsStr(drift, "uid") {
		flags = append(flags, "-u", strconv.Itoa(uid))
	}
	if primaryGroup != "" && containsStr(drift, "group") {
		flags = append(flags, "-g", shellquote.Quote(primaryGroup))
	}
	if hasGroups && containsStr(drift, "groups") {
		flags = append(flags, "-G", shellquote.Quote(strings.Join(groups, ",")))
	}
	if shell != "" && containsStr(drift, "shell") {
		flags = append(flags, "-s", shellquote.Quote(shell))
	}
	if home != "" && containsStr(drift, "home") {
		flags = append(flags, "-d", shellquote.Quote(home), "-m")
	}
	script := fmt.Sprintf("usermod %s %s", strings.Join(flags, " "), shellquote.Quote(name))
	if out, bad := rc.exec(script); bad != nil {
		return bad
	} else if out.Code != 0 {
		return Fail("usermod 失败: %s", firstLine(out.Stderr))
	}
	return &Result{Changed: true, Msg: fmt.Sprintf("用户 %s: 已调整 %s", name, strings.Join(drift, "、"))}
}

// userDrift 探测已存在用户的属性漂移，返回（漂移字段名列表、diff 行、失败）。
// 仅比较显式提供的参数；groups 与主组并集后做集合比较保证幂等。
func userDrift(rc *RunContext, name string, hasUID bool, uid int, primaryGroup string, hasGroups bool, groups []string, shell, home string) ([]string, []string, *Result) {
	var drift, diff []string
	if hasUID {
		cur, bad := userUID(rc, name)
		if bad != nil {
			return nil, nil, bad
		}
		if want := strconv.Itoa(uid); cur != want {
			drift = append(drift, "uid")
			diff = append(diff, "- uid "+cur, "+ uid "+want)
		}
	}
	curPrimary := ""
	if primaryGroup != "" || hasGroups {
		var bad *Result
		curPrimary, bad = userPrimaryGroup(rc, name)
		if bad != nil {
			return nil, nil, bad
		}
		if primaryGroup != "" && primaryGroup != curPrimary {
			drift = append(drift, "group")
			diff = append(diff, "- group "+curPrimary, "+ group "+primaryGroup)
		}
	}
	if hasGroups {
		curFull, bad := userGroups(rc, name)
		if bad != nil {
			return nil, nil, bad
		}
		// 目标全集 = 附加组 + 主组（主组不在 -G 列表也保持成员关系，集合比较才收敛）
		wantPrimary := primaryGroup
		if wantPrimary == "" {
			wantPrimary = curPrimary
		}
		want := sortUnique(append(append([]string{}, groups...), wantPrimary))
		if joinSorted(curFull) != joinSorted(want) {
			drift = append(drift, "groups")
			diff = append(diff, "- groups "+joinSorted(curFull), "+ groups "+joinSorted(want))
		}
	}
	if shell != "" || home != "" {
		curHome, curShell, bad := userPasswd(rc, name)
		if bad != nil {
			return nil, nil, bad
		}
		if shell != "" && shell != curShell {
			drift = append(drift, "shell")
			diff = append(diff, "- shell "+curShell, "+ shell "+shell)
		}
		if home != "" && home != curHome {
			drift = append(drift, "home")
			diff = append(diff, "- home "+curHome, "+ home "+home)
		}
	}
	return drift, diff, nil
}

// userExists 探测用户是否存在（id -u 退出码）。
func userExists(rc *RunContext, name string) (bool, *Result) {
	out, bad := rc.exec(fmt.Sprintf("id -u %s >/dev/null 2>&1", shellquote.Quote(name)))
	if bad != nil {
		return false, bad
	}
	return out.Code == 0, nil
}

// userUID 读取当前 UID（用户必须存在）。
func userUID(rc *RunContext, name string) (string, *Result) {
	out, bad := rc.exec(fmt.Sprintf("id -u %s", shellquote.Quote(name)))
	if bad != nil {
		return "", bad
	}
	if out.Code != 0 {
		return "", Fail("读取 %s UID 失败: %s", name, firstLine(out.Stderr))
	}
	return strings.TrimSpace(out.Stdout), nil
}

// userPrimaryGroup 读取主组名。
func userPrimaryGroup(rc *RunContext, name string) (string, *Result) {
	out, bad := rc.exec(fmt.Sprintf("id -gn %s", shellquote.Quote(name)))
	if bad != nil {
		return "", bad
	}
	if out.Code != 0 {
		return "", Fail("读取 %s 主组失败: %s", name, firstLine(out.Stderr))
	}
	return strings.TrimSpace(out.Stdout), nil
}

// userGroups 读取全部组名（含主组）并排序去重。
func userGroups(rc *RunContext, name string) ([]string, *Result) {
	out, bad := rc.exec(fmt.Sprintf("id -nG %s", shellquote.Quote(name)))
	if bad != nil {
		return nil, bad
	}
	if out.Code != 0 {
		return nil, Fail("读取 %s 组列表失败: %s", name, firstLine(out.Stderr))
	}
	return sortUnique(strings.Fields(out.Stdout)), nil
}

// userPasswd 读取 passwd 条目中的 home 与 shell（getent 字段 6/7）。
func userPasswd(rc *RunContext, name string) (string, string, *Result) {
	script := fmt.Sprintf(`getent passwd %s | cut -d: -f6,7`, shellquote.Quote(name))
	out, bad := rc.exec(script)
	if bad != nil {
		return "", "", bad
	}
	if out.Code != 0 {
		return "", "", Fail("getent passwd %s 失败（getent 不存在？）", name)
	}
	home, shell, _ := strings.Cut(strings.TrimSpace(out.Stdout), ":")
	return home, shell, nil
}

// containsStr 简易包含判断（小列表线性扫即可）。
func containsStr(items []string, s string) bool {
	for _, it := range items {
		if it == s {
			return true
		}
	}
	return false
}

// joinSorted 逗号连接（已排序列表展示用）。
func joinSorted(items []string) string {
	return strings.Join(items, ",")
}

// argInt 解析整数参数（YAML 数值可能是 int/int64/float64，或字符串数字）。
func argInt(args map[string]any, key string) (int, bool) {
	v, ok := args[key]
	if !ok || v == nil {
		return 0, false
	}
	switch x := v.(type) {
	case int:
		return x, true
	case int64:
		return int(x), true
	case float64:
		return int(x), true
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(x))
		if err != nil {
			return 0, false
		}
		return n, true
	}
	return 0, false
}
