package module

import (
	"fmt"
	"strings"

	"wdp/internal/i18n"
	"wdp/internal/shellquote"
)

func init() {
	Register(&FileModule{})
}

// FileModule 管理远端文件/目录/链接的状态与属性。
type FileModule struct{}

// Name 模块名。
func (m *FileModule) Name() string { return "file" }

// Desc 模块说明。
func (m *FileModule) Desc() string {
	return i18n.T("manage file/directory/symlink state and attributes", "管理文件/目录/链接状态与属性")
}

// Run 管理远端路径状态与属性：状态收敛（directory/touch/link/absent）
// + 属性漂移校正（mode/owner/group）。类型冲突显式报错；
// check 模式全量预估，--diff 输出属性 before→after。
func (m *FileModule) Run(rc *RunContext, args map[string]any, free string) *Result {
	path, ok := argStr(args, "path")
	if !ok || path == "" {
		return Fail("file 需要 path 参数")
	}
	state, _ := argStr(args, "state")
	switch state {
	case "", "file", "directory", "link", "touch", "absent":
	default:
		return Fail("不支持的 state %q（可选: file/directory/link/touch/absent）", state)
	}
	mode, hasMode := argMode(args, "mode")
	owner, _ := argStr(args, "owner")
	group, _ := argStr(args, "group")
	src, _ := argStr(args, "src")
	if (owner != "" || group != "") && !rc.Become {
		return Fail("设置 owner/group 需要 become: true（%s）", path)
	}
	if state == "link" && src == "" {
		return Fail("state=link 需要 src 指定链接目标")
	}

	kind, bad := probePath(rc, path)
	if bad != nil {
		return bad
	}

	// absent：删除存在的路径（快照登记回滚）
	if state == "absent" {
		if kind == "missing" {
			return &Result{Msg: fmt.Sprintf("%s 不存在", path)}
		}
		if rc.CheckMode {
			return &Result{Changed: true, Msg: fmt.Sprintf("[check] 将删除 %s（%s）", path, kind)}
		}
		if rc.Rollback != nil {
			rc.Rollback.Snapshot(rc, path)
		}
		if out, bad := rc.exec(fmt.Sprintf("rm -rf -- %s", shellquote.Quote(path))); bad != nil {
			return bad
		} else if out.Code != 0 {
			return Fail("删除失败: %s", firstLine(out.Stderr))
		}
		return &Result{Changed: true, Msg: fmt.Sprintf("已删除 %s（%s）", path, kind)}
	}

	// 类型冲突显式报错（file/directory/link 互斥）
	if conflict := typeConflict(state, kind, path); conflict != "" {
		return Fail("%s", conflict)
	}
	if state == "file" && kind == "missing" {
		return Fail("文件不存在: %s（state=file 只校验不创建）", path)
	}

	changed := false
	var logs, diffLines []string
	addChange := func(log string, diff ...string) {
		changed = true
		logs = append(logs, log)
		diffLines = append(diffLines, diff...)
	}

	// 状态收敛（check 模式只预估不执行）
	switch {
	case state == "directory" && kind == "missing":
		if rc.CheckMode {
			addChange("将创建目录")
		} else {
			if rc.Rollback != nil {
				rc.Rollback.RecordRemove(path)
			}
			if out, bad := rc.exec(fmt.Sprintf("mkdir -p -- %s", shellquote.Quote(path))); bad != nil {
				return bad
			} else if out.Code != 0 {
				return Fail("创建目录失败: %s", firstLine(out.Stderr))
			}
			addChange("已创建目录")
		}
	case state == "touch" && kind == "missing":
		if rc.CheckMode {
			addChange("将创建文件")
		} else {
			if rc.Rollback != nil {
				rc.Rollback.RecordRemove(path)
			}
			if out, bad := rc.exec(fmt.Sprintf("touch -- %s", shellquote.Quote(path))); bad != nil {
				return bad
			} else if out.Code != 0 {
				return Fail("创建文件失败: %s", firstLine(out.Stderr))
			}
			addChange("已创建文件")
		}
	case state == "link":
		cur := ""
		if kind == "link" {
			cur = linkTarget(rc, path)
		}
		if cur != src {
			if rc.CheckMode {
				addChange(fmt.Sprintf("将链接 → %s", src))
			} else {
				if rc.Rollback != nil {
					if kind == "missing" {
						rc.Rollback.RecordRemove(path)
					} else {
						rc.Rollback.Snapshot(rc, path)
					}
				}
				if bad := mklink(rc, path, src, kind != "missing"); bad != nil {
					return bad
				}
				addChange(fmt.Sprintf("已链接 → %s", src))
			}
		}
	}

	// 属性漂移（mode/owner/group）：路径存在（或将新建）时校正
	attrsExist := kind != "missing" || changed
	if !attrsExist {
		// 无 state 且路径缺失：仅属性校正语义下无对象，按无变更处理
		return &Result{Msg: fmt.Sprintf("%s 不存在，无属性可校正", path)}
	}
	if hasMode {
		wantMode := int64(mode.Perm())
		if rc.CheckMode {
			if cur, ok, mbad := remoteMode(rc, path); mbad != nil {
				return mbad
			} else if ok && cur != wantMode {
				addChange(fmt.Sprintf("权限 → %04o", wantMode),
					fmt.Sprintf("- mode: %04o", cur), fmt.Sprintf("+ mode: %04o", wantMode))
			}
		} else if fixed, bad := chmodIfDiffers(rc, path, wantMode); bad != nil {
			return bad
		} else if fixed {
			addChange(fmt.Sprintf("权限 → %04o", wantMode))
		}
	}
	if owner != "" || group != "" {
		curOwner, curGroup, ok, bad := remoteOwnerGroup(rc, path)
		if bad != nil {
			return bad
		}
		if !ok || curOwner != owner || curGroup != group {
			if rc.CheckMode {
				addChange(fmt.Sprintf("属主 → %s:%s", owner, group),
					fmt.Sprintf("- owner: %s:%s", curOwner, curGroup),
					fmt.Sprintf("+ owner: %s:%s", owner, group))
			} else if bad := chownPath(rc, path, owner, group); bad != nil {
				return bad
			} else {
				addChange(fmt.Sprintf("属主 → %s:%s", owner, group))
			}
		}
	}

	if rc.CheckMode && changed {
		return &Result{Changed: true, Msg: "[check] " + joinCN(logs), Diff: joinLines(diffLines)}
	}
	if !changed {
		return &Result{Msg: fmt.Sprintf("%s %s", path, changeLabel(false))}
	}
	return &Result{Changed: true, Msg: fmt.Sprintf("%s %s", path, joinCN(logs)), Diff: joinLines(diffLines)}
}

// typeConflict 返回状态与现存路径类型的冲突描述（空串 = 无冲突）：
// directory/file/link 对已存在但类型不符的路径显式报错。
func typeConflict(state, kind, path string) string {
	if kind == "missing" || state == "" || state == "touch" || state == "absent" || kind == state {
		return ""
	}
	return fmt.Sprintf("类型冲突：%s 已存在且为%s（state=%s）", path, kindCN(kind), state)
}

func kindCN(kind string) string {
	switch kind {
	case "file":
		return "普通文件"
	case "directory":
		return "目录"
	case "link":
		return "符号链接"
	}
	return kind
}

// probePath 探测路径类型：missing/file/directory/link。
func probePath(rc *RunContext, path string) (string, *Result) {
	script := fmt.Sprintf(`p=%s
if [ -L "$p" ]; then echo link
elif [ -d "$p" ]; then echo directory
elif [ -e "$p" ]; then echo file
else echo missing
fi`, shellquote.Quote(path))
	out, bad := rc.exec(script)
	if bad != nil {
		return "", bad
	}
	return strings.TrimSpace(out.Stdout), nil
}

// linkTarget 读取符号链接目标（非链接或读取失败返回空串）。
func linkTarget(rc *RunContext, path string) string {
	out, bad := rc.exec(fmt.Sprintf("readlink -- %s", shellquote.Quote(path)))
	if bad != nil || out.Code != 0 {
		return ""
	}
	return strings.TrimSpace(out.Stdout)
}

// remoteOwnerGroup 读取远端路径属主/属组（GNU stat 优先，BSD stat 兜底）。
func remoteOwnerGroup(rc *RunContext, path string) (string, string, bool, *Result) {
	script := fmt.Sprintf(`p=%s
[ -e "$p" ] || exit 3
stat -c '%%U %%G' "$p" 2>/dev/null || stat -f '%%Su %%Sg' "$p" 2>/dev/null
exit $?`, shellquote.Quote(path))
	out, bad := rc.exec(script)
	if bad != nil {
		return "", "", false, bad
	}
	if out.Code != 0 {
		return "", "", false, nil
	}
	fields := strings.Fields(out.Stdout)
	if len(fields) < 2 {
		return "", "", false, nil
	}
	return fields[0], fields[1], true, nil
}

func mklink(rc *RunContext, path, target string, force bool) *Result {
	flag := "-s"
	if force {
		flag = "-sfn"
	}
	out, bad := rc.exec(fmt.Sprintf("ln %s -- %s %s", flag, shellquote.Quote(target), shellquote.Quote(path)))
	if bad != nil {
		return bad
	}
	if out.Code != 0 {
		return Fail("创建链接失败: %s", firstLine(out.Stderr))
	}
	return nil
}

func changeLabel(would bool) string {
	if would {
		return "发生变更"
	}
	return "保持不变"
}

// Params 参数文档。
func (m *FileModule) Params() []ParamDoc {
	return []ParamDoc{
		{Name: "path", Type: "string", Desc: "远端路径（必需）"},
		{Name: "state", Type: "string", Desc: "file/directory/link/touch/absent"},
		{Name: "mode", Type: "mode", Desc: "权限，如 0755"},
		{Name: "owner", Type: "string", Desc: "属主（需 become）"},
		{Name: "group", Type: "string", Desc: "属组（需 become）"},
		{Name: "src", Type: "string", Desc: "state=link 时的链接目标"},
	}
}

// Example 示例任务。
func (m *FileModule) Example() string {
	return `- name: 应用目录与软链
  file:
    path: "{{ .global.workdir }}"
    state: directory
    mode: "0755"

- name: 当前版本软链
  file:
    src: "{{ .global.workdir }}/releases/v1"
    dest: "{{ .global.workdir }}/current"
    state: link
`
}
