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
		{Name: "groups", Type: "list", Desc: "附加组列表（漂移时经 usermod -G 整体覆盖；append: true 时改为 -aG 只增不删）"},
		{Name: "append", Type: "bool", Default: "false", Desc: "groups 只追加成员、不删除既有附加组（usermod -aG）"},
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
		return nil, Fail("user 需要 name 参数")
	}
	state, _ := argStr(args, "state")
	if state == "" {
		state = "present"
	}
	switch state {
	case "present", "absent":
	default:
		return nil, Fail("不支持的 state %q（可选: present/absent）", state)
	}
	u := &userReq{name: name, state: state}
	u.uid, u.hasUID = argInt(args, "uid")
	u.primaryGroup, _ = argStr(args, "group")
	u.groups, u.hasGroups = argStrList(args, "groups")
	u.appendGroups, _ = argBool(args, "append")
	u.shell, _ = argStr(args, "shell")
	u.home, _ = argStr(args, "home")
	u.system, _ = argBool(args, "system")
	u.password, _ = argStr(args, "password")
	return u, nil
}

// Run 执行用户管理：absent 删除 / present 创建缺失 / 已存在则按漂移项校正。
func (m *UserModule) Run(rc *RunContext, args map[string]any, free string) *Result {
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

// userCreate 创建缺失用户（useradd flags 组装；check 模式输出创建内容 diff）。
func userCreate(rc *RunContext, u *userReq) *Result {
	if !rc.Become {
		return Fail("创建用户需要 become: true")
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
		res := &Result{Changed: true, Msg: fmt.Sprintf("[check] 用户 %s 将创建", u.name)}
		if rc.DiffMode {
			var d []string
			d = append(d, fmt.Sprintf("+ %s（新建用户%s）", u.name, boolTo(u.system, "，系统账号", "")))
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
		return Fail("useradd 失败: %s", firstLine(out.Stderr))
	}
	return &Result{Changed: true, Msg: fmt.Sprintf("用户 %s 已创建", u.name)}
}

// userModify 校正已存在用户的属性漂移（探测漂移项，usermod 仅调整漂移项）。
func userModify(rc *RunContext, u *userReq) *Result {
	drift, diffLines, bad := userDrift(rc, u)
	if bad != nil {
		return bad
	}
	if len(drift) == 0 {
		return &Result{Msg: fmt.Sprintf("用户 %s 已是目标状态", u.name)}
	}
	if !rc.Become {
		return Fail("用户 %s 属性漂移（%s），校正需要 become: true", u.name, strings.Join(drift, "、"))
	}
	if rc.CheckMode {
		return &Result{
			Changed: true,
			Msg:     fmt.Sprintf("[check] 用户 %s: 将调整 %s", u.name, strings.Join(drift, "、")),
			Diff:    joinLines(diffLines),
		}
	}
	var flags []string
	if u.hasUID && containsStr(drift, "uid") {
		flags = append(flags, "-u", strconv.Itoa(u.uid))
	}
	if u.primaryGroup != "" && containsStr(drift, "group") {
		flags = append(flags, "-g", shellquote.Quote(u.primaryGroup))
	}
	if u.hasGroups && containsStr(drift, "groups") {
		if u.appendGroups {
			flags = append(flags, "-aG", shellquote.Quote(strings.Join(u.groups, ",")))
		} else {
			flags = append(flags, "-G", shellquote.Quote(strings.Join(u.groups, ",")))
		}
	}
	if u.shell != "" && containsStr(drift, "shell") {
		flags = append(flags, "-s", shellquote.Quote(u.shell))
	}
	if u.home != "" && containsStr(drift, "home") {
		flags = append(flags, "-d", shellquote.Quote(u.home), "-m")
	}
	script := fmt.Sprintf("usermod %s %s", strings.Join(flags, " "), shellquote.Quote(u.name))
	if out, bad := rc.exec(script); bad != nil {
		return bad
	} else if out.Code != 0 {
		return Fail("usermod 失败: %s", firstLine(out.Stderr))
	}
	return &Result{Changed: true, Msg: fmt.Sprintf("用户 %s: 已调整 %s", u.name, strings.Join(drift, "、"))}
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
