package module

import (
	"fmt"
	"io/fs"
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

// RollbackCapability 变更经快照登记可自动回滚，且 absent 即逆操作。
func (m *FileModule) RollbackCapability() RollbackCapability { return RollbackFull }

// Desc 模块说明。
func (m *FileModule) Desc() string {
	return i18n.T("manage file/directory/symlink state and attributes", "管理文件/目录/链接状态与属性")
}

// fileReq 是 file 模块解析后的参数。
type fileReq struct {
	path         string
	state        string
	src          string // state=link 的链接目标
	owner, group string
	mode         fs.FileMode
	hasMode      bool
}

// parseFileArgs 解析并校验 file 模块参数（path/dest 别名、state 合法性、
// owner/group 的 become 要求、link 的 src 要求）。
func parseFileArgs(rc *RunContext, args map[string]any) (*fileReq, *Result) {
	path, ok := argStr(args, "path")
	if !ok || path == "" {
		// dest 别名：与 copy/template/get_url/unarchive 保持一致的键名
		if alias, aok := argStr(args, "dest"); aok && alias != "" {
			path = alias
			ok = true
		}
	}
	if !ok || path == "" {
		return nil, Fail("%s", i18n.T("file requires a path parameter (or dest alias)", "file 需要 path 参数（或 dest 别名）"))
	}
	state, _ := argStr(args, "state")
	switch state {
	case "", "file", "directory", "link", "touch", "absent":
	default:
		return nil, Fail(i18n.T("unsupported state %q (options: file/directory/link/touch/absent)", "不支持的 state %q（可选: file/directory/link/touch/absent）"), state)
	}
	fr := &fileReq{path: path, state: state}
	fr.mode, fr.hasMode = argMode(args, "mode")
	fr.owner, _ = argStr(args, "owner")
	fr.group, _ = argStr(args, "group")
	fr.src, _ = argStr(args, "src")
	if (fr.owner != "" || fr.group != "") && !rc.Become {
		return nil, Fail(i18n.T("setting owner/group requires become: true (%s)", "设置 owner/group 需要 become: true（%s）"), fr.path)
	}
	if fr.state == "link" && fr.src == "" {
		return nil, Fail("%s", i18n.T("state=link requires src to specify the link target", "state=link 需要 src 指定链接目标"))
	}
	return fr, nil
}

// fileChanges 聚合 file 模块状态收敛与属性校正的变更（日志与 diff 行）。
type fileChanges struct {
	changed   bool
	logs      []string
	diffLines []string
}

func (c *fileChanges) add(log string, diff ...string) {
	c.changed = true
	c.logs = append(c.logs, log)
	c.diffLines = append(c.diffLines, diff...)
}

// result 产出模块结果（check 模式标注预估，无变更按 ok 处理）。
func (c *fileChanges) result(rc *RunContext, path string) *Result {
	if rc.CheckMode && c.changed {
		return &Result{Changed: true, Msg: "[check] " + joinCN(c.logs), Diff: joinLines(c.diffLines)}
	}
	if !c.changed {
		return &Result{Msg: fmt.Sprintf("%s %s", path, changeLabel(false))}
	}
	return &Result{Changed: true, Msg: fmt.Sprintf("%s %s", path, joinCN(c.logs)), Diff: joinLines(c.diffLines)}
}

// Run 管理远端路径状态与属性：状态收敛（directory/touch/link/absent）
// + 属性漂移校正（mode/owner/group）。类型冲突显式报错；
// check 模式全量预估，--diff 输出属性 before→after。
func (m *FileModule) Run(rc *RunContext, args map[string]any, free string) *Result {
	fr, bad := parseFileArgs(rc, args)
	if bad != nil {
		return bad
	}
	kind, bad := probePath(rc, fr.path)
	if bad != nil {
		return bad
	}

	// absent：删除存在的路径（快照登记回滚）
	if fr.state == "absent" {
		return fileAbsent(rc, fr.path, kind)
	}

	// 类型冲突显式报错（file/directory/link 互斥）
	if conflict := typeConflict(fr.state, kind, fr.path); conflict != "" {
		return Fail("%s", conflict)
	}
	if fr.state == "file" && kind == "missing" {
		return Fail(i18n.T("file does not exist: %s (state=file only validates, it does not create)", "文件不存在: %s（state=file 只校验不创建）"), fr.path)
	}

	ch := &fileChanges{}
	if bad := convergeFileState(rc, fr, kind, ch); bad != nil {
		return bad
	}
	if bad := fixFileAttrs(rc, fr, kind, ch); bad != nil {
		return bad
	}
	return ch.result(rc, fr.path)
}

// fileAbsent 删除存在的路径（快照登记回滚）。
func fileAbsent(rc *RunContext, path, kind string) *Result {
	if kind == "missing" {
		return &Result{Msg: fmt.Sprintf(i18n.T("%s does not exist", "%s 不存在"), path)}
	}
	if rc.CheckMode {
		return &Result{Changed: true, Msg: fmt.Sprintf(i18n.T("[check] will delete %s (%s)", "[check] 将删除 %s（%s）"), path, kind)}
	}
	if rc.Rollback != nil {
		rc.Rollback.Snapshot(rc, path)
	}
	if out, bad := rc.exec(fmt.Sprintf("rm -rf -- %s", shellquote.Quote(path))); bad != nil {
		return bad
	} else if out.Code != 0 {
		return Fail(i18n.T("delete failed: %s", "删除失败: %s"), firstLine(out.Stderr))
	}
	return &Result{Changed: true, Msg: fmt.Sprintf(i18n.T("deleted %s (%s)", "已删除 %s（%s）"), path, kind)}
}

// convergeFileState 状态收敛（directory/touch/link；check 模式只预估不执行）。
func convergeFileState(rc *RunContext, fr *fileReq, kind string, ch *fileChanges) *Result {
	switch {
	case fr.state == "directory" && kind == "missing":
		if rc.CheckMode {
			ch.add(i18n.T("will create directory", "将创建目录"))
		} else {
			if rc.Rollback != nil {
				rc.Rollback.RecordRemove(fr.path)
			}
			if out, bad := rc.exec(fmt.Sprintf("mkdir -p -- %s", shellquote.Quote(fr.path))); bad != nil {
				return bad
			} else if out.Code != 0 {
				return Fail(i18n.T("failed to create directory: %s", "创建目录失败: %s"), firstLine(out.Stderr))
			}
			ch.add(i18n.T("created directory", "已创建目录"))
		}
	case fr.state == "touch" && kind == "missing":
		if rc.CheckMode {
			ch.add(i18n.T("will create file", "将创建文件"))
		} else {
			if rc.Rollback != nil {
				rc.Rollback.RecordRemove(fr.path)
			}
			if out, bad := rc.exec(fmt.Sprintf("touch -- %s", shellquote.Quote(fr.path))); bad != nil {
				return bad
			} else if out.Code != 0 {
				return Fail(i18n.T("failed to create file: %s", "创建文件失败: %s"), firstLine(out.Stderr))
			}
			ch.add(i18n.T("created file", "已创建文件"))
		}
	case fr.state == "touch":
		// 已存在的文件/目录：touch 语义是刷新时间戳（此前静默跳过不更新，
		// 依赖 mtime 的下游如 make/监控感知不到）
		if rc.CheckMode {
			ch.add(i18n.T("will update timestamp", "将更新时间戳"))
		} else {
			if out, bad := rc.exec(fmt.Sprintf("touch -- %s", shellquote.Quote(fr.path))); bad != nil {
				return bad
			} else if out.Code != 0 {
				return Fail(i18n.T("failed to touch: %s", "touch 失败: %s"), firstLine(out.Stderr))
			}
			ch.add(i18n.T("timestamp updated", "时间戳已更新"))
		}
	case fr.state == "link":
		cur := ""
		if kind == "link" {
			cur = linkTarget(rc, fr.path)
		}
		if cur != fr.src {
			if rc.CheckMode {
				ch.add(fmt.Sprintf(i18n.T("will link → %s", "将链接 → %s"), fr.src))
			} else {
				if rc.Rollback != nil {
					if kind == "missing" {
						rc.Rollback.RecordRemove(fr.path)
					} else {
						rc.Rollback.Snapshot(rc, fr.path)
					}
				}
				if bad := mklink(rc, fr.path, fr.src, kind != "missing"); bad != nil {
					return bad
				}
				ch.add(fmt.Sprintf(i18n.T("linked → %s", "已链接 → %s"), fr.src))
			}
		}
	}
	return nil
}

// fixFileAttrs 校正属性漂移（mode/owner/group）：路径存在（或将新建）时校正，
// 路径缺失且无状态动作时无对象可校正。
func fixFileAttrs(rc *RunContext, fr *fileReq, kind string, ch *fileChanges) *Result {
	if kind == "missing" && !ch.changed {
		// 无 state 且路径缺失：仅属性校正语义下无对象，按无变更处理
		return &Result{Msg: fmt.Sprintf(i18n.T("%s does not exist, no attributes to correct", "%s 不存在，无属性可校正"), fr.path)}
	}
	if fr.hasMode {
		wantMode := int64(fr.mode.Perm())
		if rc.CheckMode {
			if cur, ok, mbad := remoteMode(rc, fr.path); mbad != nil {
				return mbad
			} else if ok && cur != wantMode {
				ch.add(fmt.Sprintf(i18n.T("permission → %04o", "权限 → %04o"), wantMode),
					fmt.Sprintf("- mode: %04o", cur), fmt.Sprintf("+ mode: %04o", wantMode))
			}
		} else if fixed, bad := chmodIfDiffers(rc, fr.path, wantMode); bad != nil {
			return bad
		} else if fixed {
			ch.add(fmt.Sprintf(i18n.T("permission → %04o", "权限 → %04o"), wantMode))
		}
	}
	if fr.owner != "" || fr.group != "" {
		curOwner, curGroup, ok, bad := remoteOwnerGroup(rc, fr.path)
		if bad != nil {
			return bad
		}
		if !ok || curOwner != fr.owner || curGroup != fr.group {
			if rc.CheckMode {
				ch.add(fmt.Sprintf(i18n.T("owner → %s:%s", "属主 → %s:%s"), fr.owner, fr.group),
					fmt.Sprintf("- owner: %s:%s", curOwner, curGroup),
					fmt.Sprintf("+ owner: %s:%s", fr.owner, fr.group))
			} else if bad := chownPath(rc, fr.path, fr.owner, fr.group); bad != nil {
				return bad
			} else {
				ch.add(fmt.Sprintf(i18n.T("owner → %s:%s", "属主 → %s:%s"), fr.owner, fr.group))
			}
		}
	}
	return nil
}

// typeConflict 返回状态与现存路径类型的冲突描述（空串 = 无冲突）：
// directory/file/link 对已存在但类型不符的路径显式报错。
func typeConflict(state, kind, path string) string {
	if kind == "missing" || state == "" || state == "touch" || state == "absent" || kind == state {
		return ""
	}
	return fmt.Sprintf(i18n.T("type conflict: %s already exists and is %s (state=%s)", "类型冲突：%s 已存在且为%s（state=%s）"), path, kindCN(kind), state)
}

func kindCN(kind string) string {
	switch kind {
	case "file":
		return i18n.T("regular file", "普通文件")
	case "directory":
		return i18n.T("directory", "目录")
	case "link":
		return i18n.T("symbolic link", "符号链接")
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
		return Fail(i18n.T("failed to create link: %s", "创建链接失败: %s"), firstLine(out.Stderr))
	}
	return nil
}

func changeLabel(would bool) string {
	if would {
		return i18n.T("changed", "发生变更")
	}
	return i18n.T("unchanged", "保持不变")
}

// Params 参数文档。
func (m *FileModule) Params() []ParamDoc {
	return []ParamDoc{
		{Name: "path", Type: "string", Desc: "远端路径（必需；dest 为等价别名）"},
		{Name: "dest", Type: "string", Desc: "path 的别名（与 copy/template 等模块键名一致）"},
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
    path: "{{ .global.workdir }}/current"
    state: link
`
}
