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

// groupStates 是 group.state 的合法值域：解析校验与 Params 文档共用同一
// 变量（文档长在实现旁边）。
var groupStates = []string{"present", "absent"}

// GroupModule 管理系统组（创建/删除/GID 校正）。
// 系统级变更不可回滚（与 package/user 模块同样视为不可逆操作，不登记回滚日志）。
type GroupModule struct{}

func (m *GroupModule) Name() string { return "group" }

func (m *GroupModule) Desc() string {
	return "manage system groups (create/delete/GID correction)"
}

func (m *GroupModule) Params() []ParamDoc {
	return []ParamDoc{
		{Name: "name", Type: "string", Desc: "group name"},
		{Name: "state", Type: "string", Default: "present", Enum: groupStates, Desc: "present creates/fixes drift; absent removes"},
		{Name: "gid", Type: "int", Desc: "GID (drift on existing groups is fixed via groupmod -g)"},
		{Name: "system", Type: "bool", Default: "false", Desc: "create as a system group (groupadd -r, only at creation)"},
	}
}

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

// groupReq 是 group 解析后的参数。
type groupReq struct {
	name   string
	state  string
	gid    int
	hasGID bool
	system bool
}

// parseGroupArgs 解析并校验 group 参数（name 必填、state 合法）。
func parseGroupArgs(rc *RunContext, args map[string]any) (*groupReq, *Result) {
	name, ok := argStr(args, "name")
	if !ok || name == "" {
		return nil, Fail("group requires a name parameter")
	}
	state, ok := parseState(args, "present", groupStates...)
	if !ok {
		return nil, Fail("unsupported state %q (options: %s)", state, strings.Join(groupStates, "/"))
	}
	g := &groupReq{name: name, state: state}
	g.gid, g.hasGID = argInt(args, "gid")
	g.system, _ = argBool(args, "system")
	return g, nil
}

// Run 执行组管理：解析 → 探测存在性 → absent 删除 / present 创建缺失 /
// 已存在则仅校正 GID 漂移（骨架与 user 模块一致）。
func (m *GroupModule) Run(rc *RunContext, args map[string]any, _ string) *Result {
	g, bad := parseGroupArgs(rc, args)
	if bad != nil {
		return bad
	}

	exists, bad := groupExists(rc, g.name)
	if bad != nil {
		return bad
	}
	if g.state == "absent" {
		return groupAbsent(rc, g.name, exists)
	}
	if !exists {
		return groupCreate(rc, g)
	}
	return groupConverge(rc, g)
}

// groupAbsent 删除存在的组。
func groupAbsent(rc *RunContext, name string, exists bool) *Result {
	if !exists {
		return &Result{Msg: fmt.Sprintf("group %s does not exist", name)}
	}
	if !rc.Become {
		return Fail("deleting a group requires become: true")
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

// groupCreate 创建缺失的组（groupadd flags 组装；check 模式输出创建内容 diff）。
func groupCreate(rc *RunContext, g *groupReq) *Result {
	if !rc.Become {
		return Fail("creating a group requires become: true")
	}
	var flags []string
	if g.system {
		flags = append(flags, "-r")
	}
	if g.hasGID {
		flags = append(flags, "-g", strconv.Itoa(g.gid))
	}
	script := fmt.Sprintf("groupadd %s %s", strings.Join(flags, " "), shellquote.Quote(g.name))
	if rc.CheckMode {
		res := &Result{Changed: true, Msg: fmt.Sprintf("[check] group %s would be created", g.name)}
		if rc.DiffMode {
			var d []string
			d = append(d, fmt.Sprintf("+ %s (new group%s)", g.name, boolTo(g.system, ", system group", "")))
			if g.hasGID {
				d = append(d, "+ gid "+strconv.Itoa(g.gid))
			}
			res.Diff = strings.Join(d, "\n")
		}
		return res
	}
	if out, bad := rc.exec(script); bad != nil {
		return bad
	} else if out.Code != 0 {
		return Fail("groupadd failed: %s", firstLine(out.Stderr))
	}
	return &Result{Changed: true, Msg: fmt.Sprintf("group %s created", g.name)}
}

// groupConverge 校正已存在组的 GID 漂移（groupmod 仅在漂移时执行）；
// 未给 gid 或无漂移时为已收敛。
func groupConverge(rc *RunContext, g *groupReq) *Result {
	if g.hasGID {
		cur, bad := groupGID(rc, g.name)
		if bad != nil {
			return bad
		}
		if want := strconv.Itoa(g.gid); cur != want {
			if !rc.Become {
				return Fail("group %s GID drift (%s → %s), correcting requires become: true", g.name, cur, want)
			}
			if rc.CheckMode {
				res := &Result{
					Changed: true,
					Msg:     fmt.Sprintf("[check] group %s: would adjust gid (%s -> %s)", g.name, cur, want),
				}
				if rc.DiffMode { // 与其他模块一致：Diff 仅在 --diff 下填充
					res.Diff = strings.Join([]string{"- gid " + cur, "+ gid " + want}, "\n")
				}
				return res
			}
			script := fmt.Sprintf("groupmod -g %d %s", g.gid, shellquote.Quote(g.name))
			if out, bad := rc.exec(script); bad != nil {
				return bad
			} else if out.Code != 0 {
				return Fail("groupmod failed: %s", firstLine(out.Stderr))
			}
			return &Result{Changed: true, Msg: fmt.Sprintf("group %s: GID adjusted to %d", g.name, g.gid)}
		}
	}
	return &Result{Msg: fmt.Sprintf("group %s is already in the target state", g.name)}
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
