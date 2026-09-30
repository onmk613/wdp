package module

import (
	"fmt"
	"io/fs"
	"slices"
	"strings"

	"wdp/internal/i18n"
	"wdp/internal/shellquote"
)

func init() {
	Register(&FileModule{})
}

// fileStates 是 file.state 的合法值域：解析校验与 Params 文档共用同一
// 变量（文档长在实现旁边），空串合法 = 仅属性校正语义，不进枚举。
var fileStates = []string{"file", "directory", "link", "touch", "absent"}

// FileModule 管理远端文件/目录/链接的状态与属性。
type FileModule struct{}

func (m *FileModule) Name() string { return "file" }

// RollbackCapability 变更经快照登记可自动回滚，且 absent 即逆操作。
func (m *FileModule) RollbackCapability() RollbackCapability { return RollbackFull }

func (m *FileModule) Desc() string {
	return i18n.T("Manage the state and attributes of remote files, directories, and symlinks", "管理远端文件/目录/符号链接的状态与属性")
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

// parseFileArgs 解析并校验 file 模块参数（path 必填、state 合法性、
// owner/group 的 become 要求、link 的 src 要求）。
func parseFileArgs(rc *RunContext, args map[string]any) (*fileReq, *Result) {
	path, ok := argStr(args, "path")
	if !ok || path == "" {
		return nil, Fail("file requires a path parameter")
	}
	state, _ := argStr(args, "state")
	if state != "" && !slices.Contains(fileStates, state) {
		return nil, Fail("unsupported state %q (options: %s)", state, strings.Join(fileStates, "/"))
	}
	fr := &fileReq{path: path, state: state}
	fr.mode, fr.hasMode = argMode(args, "mode")
	fr.owner, _ = argStr(args, "owner")
	fr.group, _ = argStr(args, "group")
	fr.src, _ = argStr(args, "src")
	if bad := requireBecomeForOwner(rc, fr.owner, fr.group, fr.path); bad != nil {
		return nil, bad
	}
	if fr.state == "link" && fr.src == "" {
		return nil, Fail("state=link requires src to specify the link target")
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

// toResult 产出模块结果（check 模式标注预估，无变更按 ok 处理）。
// Diff 仅在 --diff 下填充（与其他模块一致，见 Result.Diff 契约）。
func (c *fileChanges) toResult(rc *RunContext, path string) *Result {
	if rc.CheckMode && c.changed {
		res := &Result{Changed: true, Msg: "[check] " + strings.Join(c.logs, ", ")}
		if rc.DiffMode {
			res.Diff = strings.Join(c.diffLines, "\n")
		}
		return res
	}
	if !c.changed {
		return &Result{Msg: fmt.Sprintf("%s %s", path, changeLabel(false))}
	}
	res := &Result{Changed: true, Msg: fmt.Sprintf("%s %s", path, strings.Join(c.logs, ", "))}
	if rc.DiffMode {
		res.Diff = strings.Join(c.diffLines, "\n")
	}
	return res
}

// Run 管理远端路径状态与属性：状态收敛（directory/touch/link/absent）
// + 属性漂移校正（mode/owner/group）。类型冲突显式报错；
// check 模式全量预估，--diff 输出属性 before→after。
func (m *FileModule) Run(rc *RunContext, args map[string]any, _ string) *Result {
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
		return Fail("file does not exist: %s (state=file only validates, it does not create)", fr.path)
	}

	ch := &fileChanges{}
	if bad := convergeFileState(rc, fr, kind, ch); bad != nil {
		return bad
	}
	if bad := fixFileAttrs(rc, fr, kind, ch); bad != nil {
		return bad
	}
	return ch.toResult(rc, fr.path)
}

// fileAbsent 删除存在的路径（快照登记回滚）。
func fileAbsent(rc *RunContext, path, kind string) *Result {
	if kind == "missing" {
		return &Result{Msg: fmt.Sprintf("%s does not exist", path)}
	}
	if rc.CheckMode {
		res := &Result{Changed: true, Msg: fmt.Sprintf("[check] will delete %s (%s)", path, kind)}
		if rc.DiffMode { // 与 user/group 的 absent 预估同口径：diff 报告将被删除的路径
			res.Diff = fmt.Sprintf("- %s (will be deleted)", path)
		}
		return res
	}
	if rc.Rollback != nil {
		rc.Rollback.Snapshot(rc, path)
	}
	if out, bad := rc.exec(fmt.Sprintf("rm -rf -- %s", shellquote.Quote(path))); bad != nil {
		return bad
	} else if out.Code != 0 {
		return Fail("delete failed: %s", firstLine(out.Stderr))
	}
	return &Result{Changed: true, Msg: fmt.Sprintf("deleted %s (%s)", path, kind)}
}

// convergeFileState 状态收敛（directory/touch/link；check 模式只预估不执行）。
func convergeFileState(rc *RunContext, fr *fileReq, kind string, ch *fileChanges) *Result {
	switch {
	case fr.state == "directory" && kind == "missing":
		if rc.CheckMode {
			ch.add("will create directory")
		} else {
			if rc.Rollback != nil {
				rc.Rollback.RecordRemove(fr.path)
			}
			if out, bad := rc.exec(fmt.Sprintf("mkdir -p -- %s", shellquote.Quote(fr.path))); bad != nil {
				return bad
			} else if out.Code != 0 {
				return Fail("failed to create directory: %s", firstLine(out.Stderr))
			}
			ch.add("created directory")
		}
	case fr.state == "touch" && kind == "missing":
		if rc.CheckMode {
			ch.add("will create file")
		} else {
			if rc.Rollback != nil {
				rc.Rollback.RecordRemove(fr.path)
			}
			if out, bad := rc.exec(fmt.Sprintf("touch -- %s", shellquote.Quote(fr.path))); bad != nil {
				return bad
			} else if out.Code != 0 {
				return Fail("failed to create file: %s", firstLine(out.Stderr))
			}
			ch.add("created file")
		}
	case fr.state == "touch":
		// 已存在的文件/目录：touch 语义是刷新时间戳（此前静默跳过不更新，
		// 依赖 mtime 的下游如 make/监控感知不到）
		if rc.CheckMode {
			ch.add("will update timestamp")
		} else {
			if out, bad := rc.exec(fmt.Sprintf("touch -- %s", shellquote.Quote(fr.path))); bad != nil {
				return bad
			} else if out.Code != 0 {
				return Fail("failed to touch: %s", firstLine(out.Stderr))
			}
			ch.add("timestamp updated")
		}
	case fr.state == "link":
		cur := ""
		if kind == "link" {
			cur = linkTarget(rc, fr.path)
		}
		if cur != fr.src {
			if rc.CheckMode {
				ch.add(fmt.Sprintf("will link → %s", fr.src))
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
				ch.add(fmt.Sprintf("linked → %s", fr.src))
			}
		}
	}
	return nil
}

// fixFileAttrs 校正属性漂移（mode/owner/group）：路径存在（或将新建）时校正，
// 路径缺失且无状态动作时无对象可校正。
// 实跑走共用的 fixAttrs（探测属主 → chmod → chown）；check 预估是 file
// 自有分支——带 before→after diff 行展示，与 putFile/get_url 的预估口径
// 不同（那边只输出汇总消息），故不并入 fixAttrs。
func fixFileAttrs(rc *RunContext, fr *fileReq, kind string, ch *fileChanges) *Result {
	if kind == "missing" && !ch.changed {
		// 无 state 且路径缺失：仅属性校正语义下无对象，按无变更处理
		return &Result{Msg: fmt.Sprintf("%s does not exist, no attributes to correct", fr.path)}
	}
	if rc.CheckMode {
		return checkFileAttrs(rc, fr, ch)
	}
	var mode *fs.FileMode
	if fr.hasMode {
		mode = modePtr(fr.mode.Perm())
	}
	fixedMode, fixedOwner, bad := fixAttrs(rc, fr.path, mode, fr.owner, fr.group)
	if bad != nil {
		return bad
	}
	if fixedMode {
		ch.add(fmt.Sprintf("permission → %04o", int64(fr.mode.Perm())))
	}
	if fixedOwner {
		ch.add(fmt.Sprintf("owner → %s:%s", fr.owner, fr.group))
	}
	return nil
}

// checkFileAttrs 是 fixFileAttrs 的 check 预估分支：只读探测 mode/属主
// 漂移并产出 diff 行（- 旧值/+ 新值），不执行任何校正。
func checkFileAttrs(rc *RunContext, fr *fileReq, ch *fileChanges) *Result {
	if fr.hasMode {
		wantMode := int64(fr.mode.Perm())
		if cur, ok, mbad := remoteMode(rc, fr.path); mbad != nil {
			return mbad
		} else if ok && cur != wantMode {
			ch.add(fmt.Sprintf("permission → %04o", wantMode),
				fmt.Sprintf("- mode: %04o", cur), fmt.Sprintf("+ mode: %04o", wantMode))
		}
	}
	if fr.owner != "" || fr.group != "" {
		curOwner, curGroup, ok, bad := remoteOwnerGroup(rc, fr.path)
		if bad != nil {
			return bad
		}
		if !ok || (fr.owner != "" && curOwner != fr.owner) || (fr.group != "" && curGroup != fr.group) {
			ch.add(fmt.Sprintf("owner → %s:%s", fr.owner, fr.group),
				fmt.Sprintf("- owner: %s:%s", curOwner, curGroup),
				fmt.Sprintf("+ owner: %s:%s", fr.owner, fr.group))
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
	return fmt.Sprintf("type conflict: %s already exists and is %s (state=%s)", path, kindLabel(kind), state)
}

// kindLabel 将探测到的路径类型映射为可读标签（kindCN 的更名：返回的
// 一直是英文标签，命名与实现保持一致）。
func kindLabel(kind string) string {
	switch kind {
	case "file":
		return "regular file"
	case "directory":
		return "directory"
	case "link":
		return "symbolic link"
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
		return Fail("failed to create link: %s", firstLine(out.Stderr))
	}
	return nil
}

func changeLabel(would bool) string {
	if would {
		return "changed"
	}
	return "unchanged"
}

// Params 参数文档。顺序即使用顺序：path → state 为主用法，link 场景的
// src 紧随 state，其余按使用频率——补全候选与 snippet 骨架按此序展示。
func (m *FileModule) Params() []ParamDoc {
	return []ParamDoc{
		{Name: "path", Type: "string", Desc: i18n.T("remote target path (required)", "远端目标路径（必填）")},
		{Name: "state", Type: "string", Enum: fileStates, Desc: i18n.T("desired state; empty = only correct attributes of an existing path", "目标状态；留空 = 仅对已存在路径做属性校正")},
		{Name: "src", Type: "string", Desc: i18n.T("link target (required when state=link)", "链接目标（state=link 时必填）")},
		{Name: "mode", Type: "mode", Desc: i18n.T("mode, e.g. 0755", "权限位，如 0755")},
		{Name: "owner", Type: "string", Desc: i18n.T("owner (requires become)", "属主（需 become）")},
		{Name: "group", Type: "string", Desc: i18n.T("group (requires become)", "属组（需 become）")},
	}
}

func (m *FileModule) Example() string {
	return i18n.T(`- name: Set up the application directory and symlink
  file:
    path: "{{ .global.workdir }}"
    state: directory
    mode: "0755"

- name: Symlink the current version
  file:
    src: "{{ .global.workdir }}/releases/v1"
    path: "{{ .global.workdir }}/current"
    state: link
`, `- name: 应用目录与符号链接
  file:
    path: "{{ .global.workdir }}"
    state: directory
    mode: "0755"

- name: 当前版本软链
  file:
    src: "{{ .global.workdir }}/releases/v1"
    path: "{{ .global.workdir }}/current"
    state: link
`)
}
