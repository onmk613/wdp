package module

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

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
	return "manage system users (create/delete/attribute correction)"
}

// Params 参数文档。
func (m *UserModule) Params() []ParamDoc {
	return []ParamDoc{
		{Name: "name", Type: "string", Desc: "user name"},
		{Name: "state", Type: "string", Default: "present", Desc: "present creates/fixes drift; absent removes (including home)"},
		{Name: "uid", Type: "int", Desc: "UID (drift on existing users is fixed via usermod -u)"},
		{Name: "group", Type: "string", Desc: "primary group (drift fixed via usermod -g)"},
		{Name: "groups", Type: "list", Desc: "supplementary groups (drift replaced via usermod -G; append: true uses -aG to add only)"},
		{Name: "append", Type: "bool", Default: "false", Desc: "append to groups only, never remove existing membership (usermod -aG)"},
		{Name: "shell", Type: "string", Desc: "login shell (drift fixed via usermod -s)"},
		{Name: "home", Type: "string", Desc: "home directory (drift migrated via usermod -d -m)"},
		{Name: "system", Type: "bool", Default: "false", Desc: "create as a system account (useradd -r, only at creation)"},
		{Name: "password", Type: "string", Desc: "crypt hash (useradd -p, only at creation; change an existing user's password via the shell module)"},
	}
}

// Example 示例任务。
func (m *UserModule) Example() string {
	return `# create a deploy user and add it to the docker group
- name: create the deploy user
  become: true
  user:
    name: deploy
    shell: /sbin/nologin
    home: /home/deploy
    groups: [docker]

# fix drift (usermod only runs when uid/shell/groups changed)
- name: fix the app user shell
  become: true
  user:
    name: app
    shell: /bin/bash

# remove a user (including home; not rollback-able)
- name: remove a departed account
  become: true
  user:
    name: leaver
    state: absent`
}

// userReq 是 user 模块解析后的参数。
type userReq struct {
	name         string
	state        string
	primaryGroup string
	shell        string
	home         string
	password     string
	uid          int
	groups       []string
	hasUID       bool
	hasGroups    bool
	appendGroups bool
	system       bool
}

// parseUserArgs 解析并校验 user 模块参数。
func parseUserArgs(args map[string]any) (*userReq, *Result) {
	name, ok := argStr(args, "name")
	if !ok || name == "" {
		return nil, Fail("%s", "user requires a name parameter")
	}
	state, ok := parseState(args, "present", "present", "absent")
	if !ok {
		return nil, Fail("unsupported state %q (options: present/absent)", state)
	}
	u := &userReq{name: name, state: state}
	u.uid, u.hasUID = argInt(args, "uid")
	u.primaryGroup, _ = argStr(args, "group")
	u.groups, u.hasGroups = argStrList(args, "groups")
	u.appendGroups, _ = argBool(args, "append")
	// groups: [] + 替换模式 = "清空附加组"：usermod -G 需要至少一个组名，
	// 空列表会拼出非法的 -G ''（远端报 group '' does not exist）。
	// fail-loud：要求显式写出保留组（通常为主组）。
	if u.hasGroups && len(u.groups) == 0 && !u.appendGroups {
		return nil, Fail("'groups: []' with append unset would clear all supplementary groups; list the groups to keep explicitly (usually the primary group), or use append: true")
	}
	u.shell, _ = argStr(args, "shell")
	u.home, _ = argStr(args, "home")
	u.system, _ = argBool(args, "system")
	u.password, _ = argStr(args, "password")
	return u, nil
}

// Run 执行用户管理：absent 删除 / present 创建缺失 / 已存在则按漂移项校正。
func (m *UserModule) Run(rc *RunContext, args map[string]any, _ string) *Result {
	u, bad := parseUserArgs(args)
	if bad != nil {
		return bad
	}

	exists, bad := userExists(rc, u.name)
	if bad != nil {
		return bad
	}
	if u.state == "absent" {
		return userAbsent(rc, u.name, exists)
	}
	if !exists {
		return userCreate(rc, u)
	}
	return userModify(rc, u)
}

// userAbsent 删除存在的用户（含 home）。
func userAbsent(rc *RunContext, name string, exists bool) *Result {
	if !exists {
		return &Result{Msg: fmt.Sprintf("user %s does not exist", name)}
	}
	if !rc.Become {
		return Fail("%s", "deleting a user requires become: true")
	}
	if rc.CheckMode {
		res := &Result{Changed: true, Msg: fmt.Sprintf("[check] user %s would be removed (including home)", name)}
		if rc.DiffMode {
			res.Diff = fmt.Sprintf("- %s (user will be deleted)", name)
		}
		return res
	}
	if out, bad := rc.exec(fmt.Sprintf("userdel -r %s", shellquote.Quote(name))); bad != nil {
		return bad
	} else if out.Code != 0 {
		return Fail("userdel failed: %s", firstLine(out.Stderr))
	}
	return &Result{Changed: true, Msg: fmt.Sprintf("user %s deleted", name)}
}

// userCreate 创建缺失用户（useradd flags 组装；check 模式输出创建内容 diff）。
func userCreate(rc *RunContext, u *userReq) *Result {
	if !rc.Become {
		return Fail("%s", "creating a user requires become: true")
	}
	var flags []string
	if u.system {
		flags = append(flags, "-r")
	}
	if u.hasUID {
		flags = append(flags, "-u", strconv.Itoa(u.uid))
	}
	if u.primaryGroup != "" {
		flags = append(flags, "-g", shellquote.Quote(u.primaryGroup))
	}
	if u.hasGroups {
		flags = append(flags, "-G", shellquote.Quote(strings.Join(u.groups, ",")))
	}
	if u.shell != "" {
		flags = append(flags, "-s", shellquote.Quote(u.shell))
	}
	if u.home != "" {
		flags = append(flags, "-d", shellquote.Quote(u.home), "-m")
	}
	if u.password != "" {
		flags = append(flags, "-p", shellquote.Quote(u.password))
	}
	script := fmt.Sprintf("useradd %s %s", strings.Join(flags, " "), shellquote.Quote(u.name))
	if rc.CheckMode {
		res := &Result{Changed: true, Msg: fmt.Sprintf("[check] user %s would be created", u.name)}
		if rc.DiffMode {
			var d []string
			d = append(d, fmt.Sprintf("+ %s (new user%s)", u.name, boolTo(u.system, ", system account", "")))
			if u.hasUID {
				d = append(d, "+ uid "+strconv.Itoa(u.uid))
			}
			if u.primaryGroup != "" {
				d = append(d, "+ group "+u.primaryGroup)
			}
			if u.hasGroups {
				d = append(d, "+ groups "+strings.Join(u.groups, ","))
			}
			if u.shell != "" {
				d = append(d, "+ shell "+u.shell)
			}
			if u.home != "" {
				d = append(d, "+ home "+u.home)
			}
			res.Diff = joinLines(d)
		}
		return res
	}
	if out, bad := rc.exec(script); bad != nil {
		return bad
	} else if out.Code != 0 {
		return Fail("useradd failed: %s", firstLine(out.Stderr))
	}
	return &Result{Changed: true, Msg: fmt.Sprintf("user %s created", u.name)}
}

// userModify 校正已存在用户的属性漂移（探测漂移项，usermod 仅调整漂移项）。
func userModify(rc *RunContext, u *userReq) *Result {
	drift, diffLines, bad := userDrift(rc, u)
	if bad != nil {
		return bad
	}
	if len(drift) == 0 {
		return &Result{Msg: fmt.Sprintf("user %s is already in the target state", u.name)}
	}
	if !rc.Become {
		return Fail("user %s attribute drift (%s), correcting requires become: true", u.name, strings.Join(drift, ", "))
	}
	if rc.CheckMode {
		res := &Result{
			Changed: true,
			Msg:     fmt.Sprintf("[check] user %s: would adjust %s", u.name, strings.Join(drift, "、")),
		}
		if rc.DiffMode { // 与其他模块一致：Diff 仅在 --diff 下填充
			res.Diff = joinLines(diffLines)
		}
		return res
	}
	var flags []string
	if u.hasUID && slices.Contains(drift, "uid") {
		flags = append(flags, "-u", strconv.Itoa(u.uid))
	}
	if u.primaryGroup != "" && slices.Contains(drift, "group") {
		flags = append(flags, "-g", shellquote.Quote(u.primaryGroup))
	}
	if u.hasGroups && slices.Contains(drift, "groups") {
		if u.appendGroups {
			flags = append(flags, "-aG", shellquote.Quote(strings.Join(u.groups, ",")))
		} else {
			flags = append(flags, "-G", shellquote.Quote(strings.Join(u.groups, ",")))
		}
	}
	if u.shell != "" && slices.Contains(drift, "shell") {
		flags = append(flags, "-s", shellquote.Quote(u.shell))
	}
	if u.home != "" && slices.Contains(drift, "home") {
		flags = append(flags, "-d", shellquote.Quote(u.home), "-m")
	}
	script := fmt.Sprintf("usermod %s %s", strings.Join(flags, " "), shellquote.Quote(u.name))
	if out, bad := rc.exec(script); bad != nil {
		return bad
	} else if out.Code != 0 {
		return Fail("usermod failed: %s", firstLine(out.Stderr))
	}
	return &Result{Changed: true, Msg: fmt.Sprintf("user %s: adjusted %s", u.name, strings.Join(drift, ", "))}
}

// userDrift 探测已存在用户的属性漂移，返回（漂移字段名列表、diff 行、失败）。
// 仅比较显式提供的参数；groups 与主组并集后做集合比较保证幂等。
// append=true 时 groups 语义为"确保成员"（不删除既有附加组，走 usermod -aG），
// 避免整体覆盖把既有 wheel/sudo 之类附加组悄悄移除。
func userDrift(rc *RunContext, u *userReq) ([]string, []string, *Result) {
	name := u.name
	var drift, diff []string
	if u.hasUID {
		cur, bad := userUID(rc, name)
		if bad != nil {
			return nil, nil, bad
		}
		if want := strconv.Itoa(u.uid); cur != want {
			drift = append(drift, "uid")
			diff = append(diff, "- uid "+cur, "+ uid "+want)
		}
	}
	curPrimary := ""
	if u.primaryGroup != "" || u.hasGroups {
		var bad *Result
		curPrimary, bad = userPrimaryGroup(rc, name)
		if bad != nil {
			return nil, nil, bad
		}
		if u.primaryGroup != "" && u.primaryGroup != curPrimary {
			drift = append(drift, "group")
			diff = append(diff, "- group "+curPrimary, "+ group "+u.primaryGroup)
		}
	}
	if u.hasGroups {
		curFull, bad := userGroups(rc, name)
		if bad != nil {
			return nil, nil, bad
		}
		wantPrimary := u.primaryGroup
		if wantPrimary == "" {
			wantPrimary = curPrimary
		}
		var want []string
		if u.appendGroups {
			// 确保成员：目标 = 当前全集 ∪ 附加组（主组恒保留，既有组不丢）
			want = sortUnique(append(append([]string{}, curFull...), u.groups...))
		} else {
			// 整体覆盖：目标全集 = 附加组 + 主组
			want = sortUnique(append(append([]string{}, u.groups...), wantPrimary))
		}
		if joinSorted(curFull) != joinSorted(want) {
			drift = append(drift, "groups")
			diff = append(diff, "- groups "+joinSorted(curFull), "+ groups "+joinSorted(want))
		}
	}
	if u.shell != "" || u.home != "" {
		curHome, curShell, bad := userPasswd(rc, name)
		if bad != nil {
			return nil, nil, bad
		}
		if u.shell != "" && u.shell != curShell {
			drift = append(drift, "shell")
			diff = append(diff, "- shell "+curShell, "+ shell "+u.shell)
		}
		if u.home != "" && u.home != curHome {
			drift = append(drift, "home")
			diff = append(diff, "- home "+curHome, "+ home "+u.home)
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
		return "", Fail("failed to read %s UID: %s", name, firstLine(out.Stderr))
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
		return "", Fail("failed to read %s primary group: %s", name, firstLine(out.Stderr))
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
		return nil, Fail("failed to read %s group list: %s", name, firstLine(out.Stderr))
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
		return "", "", Fail("getent passwd %s failed (is getent missing?)", name)
	}
	home, shell, _ := strings.Cut(strings.TrimSpace(out.Stdout), ":")
	return home, shell, nil
}

// containsStr 简易包含判断（小列表线性扫即可）。
// joinSorted 逗号连接（已排序列表展示用）。
func joinSorted(items []string) string {
	return strings.Join(items, ",")
}
