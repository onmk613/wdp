package module

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"wdp/internal/i18n"
	"wdp/internal/shellquote"
)

func init() {
	Register(&LineinfileModule{})
}

// lineinfileStates 是 lineinfile.state 的合法值域：解析校验与 Params
// 文档共用同一变量（文档长在实现旁边）。
var lineinfileStates = []string{"present", "absent"}

// LineinfileModule 确保远端文件中的某行存在/缺席/被替换：
// 下载 → 控制端变换行集 → 整体回传（幂等，check/diff/回滚齐全）。
type LineinfileModule struct{}

func (m *LineinfileModule) Name() string { return "lineinfile" }

func (m *LineinfileModule) Desc() string {
	return i18n.T("Manage a single line in a remote file (ensure present / delete / replace)", "管理远端文件中的单行（确保存在/删除/替换）")
}

func (m *LineinfileModule) Params() []ParamDoc {
	return []ParamDoc{
		{Name: "path", Type: "string", Desc: i18n.T("remote file path", "远端文件路径")},
		{Name: "line", Type: "string", Desc: i18n.T("desired line content (with state=absent, specify either line or regexp)", "期望的行内容（state=absent 时 line 与 regexp 二选一）")},
		{Name: "regexp", Type: "string", Desc: i18n.T("regex matching the target line: present replaces the first match; absent deletes all matches", "匹配目标行的正则：present 替换首个匹配；absent 删除全部匹配")},
		{Name: "state", Type: "string", Default: "present", Enum: lineinfileStates, Desc: i18n.T("present ensures the line exists / absent ensures it does not", "present 确保该行存在 / absent 确保该行不存在")},
		{Name: "insertafter", Type: "string", Default: "EOF", Desc: i18n.T("insert position when nothing matches: after the last matching line (EOF = end of file)", "无匹配时的插入位置：最后一个匹配行之后（EOF = 文件末尾）")},
		{Name: "create", Type: "bool", Default: "false", Desc: i18n.T("create the file if it does not exist (fails by default)", "文件不存在时创建（缺省直接失败）")},
		{Name: "backup", Type: "bool", Default: "false", Desc: i18n.T("back up before modifying (path.bak.<timestamp>)", "修改前先备份（path.bak.<时间戳>）")},
		{Name: "mode", Type: "mode", Default: "0644", Desc: i18n.T("mode for a newly created file (existing files keep their permissions)", "新建文件的权限位（已有文件保持原权限）")},
		{Name: "owner", Type: "string", Desc: i18n.T("owner (requires become)", "属主（需 become）")},
		{Name: "group", Type: "string", Desc: i18n.T("group (requires become)", "属组（需 become）")},
	}
}

func (m *LineinfileModule) Example() string {
	return i18n.T(`- name: Ensure the authorization line exists (create the file if missing)
  lineinfile:
    path: /etc/sudoers.d/deploy
    line: "deploy ALL=(ALL) NOPASSWD: ALL"
    create: true
    mode: "0440"

- name: Replace the first matching line (back up first)
  lineinfile:
    path: /etc/selinux/config
    regexp: "^SELINUX="
    line: "SELINUX=disabled"
    backup: true
`, `- name: 确保授权行存在（文件缺失则创建）
  lineinfile:
    path: /etc/sudoers.d/deploy
    line: "deploy ALL=(ALL) NOPASSWD: ALL"
    create: true
    mode: "0440"

- name: 替换首个匹配行（先备份）
  lineinfile:
    path: /etc/selinux/config
    regexp: "^SELINUX="
    line: "SELINUX=disabled"
    backup: true
`)
}

// lineinfileReq 是 lineinfile 解析后的参数（正则已预编译）。
type lineinfileReq struct {
	path         string
	line         string
	hasLine      bool
	state        string
	pattern      string
	afterPattern string
	create       bool
	backup       bool
	owner        string
	group        string
	mode         int64 // 仅 create 新建时生效；已有文件保持原权限
	re           *regexp.Regexp
	after        *regexp.Regexp
}

// parseLineinfileArgs 解析并校验 lineinfile 参数（path 必填、state 合法、
// present 需 line、absent 需非空 line 或 regexp、owner/group 的 become 要求），
// 并预编译 regexp/insertafter。
func parseLineinfileArgs(rc *RunContext, args map[string]any) (*lineinfileReq, *Result) {
	path, ok := argStr(args, "path")
	if !ok || path == "" {
		return nil, Fail("lineinfile requires a path parameter")
	}
	line, hasLine := argStr(args, "line")
	state, ok := parseState(args, "present", lineinfileStates...)
	if !ok {
		return nil, Fail("unsupported state %q (options: %s)", state, strings.Join(lineinfileStates, "/"))
	}
	if state != "absent" && !hasLine {
		return nil, Fail("lineinfile requires a line parameter (state=present)")
	}
	pattern, _ := argStr(args, "regexp")
	if state == "absent" {
		// line:"" 显式空串经 argStr 视为"已提供"——但空行匹配会删除文件中
		// 所有空行，语义上几乎必然是误用，与无 regexp 的 absent 一并拒绝
		if !hasLine || line == "" {
			if pattern == "" {
				return nil, Fail("state=absent requires a non-empty line or regexp (an empty line would match every blank line)")
			}
		}
	}
	afterPattern, _ := argStr(args, "insertafter")
	q := &lineinfileReq{
		path:         path,
		line:         line,
		hasLine:      hasLine,
		state:        state,
		pattern:      pattern,
		afterPattern: afterPattern,
	}
	q.create, _ = argBool(args, "create")
	q.backup, _ = argBool(args, "backup")
	q.owner, _ = argStr(args, "owner")
	q.group, _ = argStr(args, "group")
	q.mode = 0o644 // 仅 create 新建时生效；已有文件保持原权限
	if mv, ok := argMode(args, "mode"); ok {
		q.mode = int64(mv.Perm())
	}
	if bad := requireBecomeForOwner(rc, q.owner, q.group, path); bad != nil {
		return nil, bad
	}
	if pattern != "" {
		r, err := regexp.Compile(pattern)
		if err != nil {
			return nil, Fail("unable to parse regexp: %v", err)
		}
		q.re = r
	}
	if afterPattern != "" && afterPattern != "EOF" {
		r, err := regexp.Compile(afterPattern)
		if err != nil {
			return nil, Fail("unable to parse insertafter: %v", err)
		}
		q.after = r
	}
	return q, nil
}

// readRemoteFile 读取远端文件内容。返回 (内容, 是否存在, 终态结果)；
// 终态结果非 nil 时直接透传（非普通文件报错、缺文件且 absent 的"无需
// 变更"、缺文件且未开 create 的报错、下载超限报错等）。
// 存在性判定用显式探测：下载失败的语义是错误（权限/断连），
// 不能与"文件不存在"混为一谈——absent 时混同会吞错报"无需变更"。
func readRemoteFile(rc *RunContext, q *lineinfileReq) (string, bool, *Result) {
	probe := fmt.Sprintf(`p=%s
[ -e "$p" ] || exit 3
[ -f "$p" ] || exit 4`, shellquote.Quote(q.path))
	pout, pbad := rc.exec(probe)
	if pbad != nil {
		return "", false, pbad
	}
	switch pout.Code {
	case 4:
		return "", false, Fail("remote path %s exists and is not a regular file", q.path)
	case 0:
		// 下载封顶：lineinfile 需要全文才能做行变换，超限只能 fail-loud
		// （截断后回写会把远端文件改坏），不能像 diff 那样降级提示
		buf := &cappedBuffer{max: uploadLimit(rc)}
		if err := rc.Conn.DownloadFile(rc.Ctx, q.path, buf); err != nil {
			return "", false, Fail("failed to read %s: %v", q.path, err)
		}
		if buf.truncated() {
			return "", false, Fail("remote file %s exceeds the %d MiB limit ([transfer].max_upload_mb)", q.path, uploadLimit(rc)>>20)
		}
		return buf.buf.String(), true, nil
	default:
		if q.state == "absent" {
			return "", false, &Result{Msg: fmt.Sprintf("%s does not exist, no change needed for state=absent", q.path)}
		}
		if !q.create {
			return "", false, Fail("file does not exist: %s (use create: true to create it)", q.path)
		}
		return "", false, nil
	}
}

// Run 执行行级变更：解析 → 探测读取 → 变换 → check 预估或实跑回写
// （骨架与 user 模块一致）。
func (m *LineinfileModule) Run(rc *RunContext, args map[string]any, _ string) *Result {
	q, bad := parseLineinfileArgs(rc, args)
	if bad != nil {
		return bad
	}
	oldContent, exists, bad := readRemoteFile(rc, q)
	if bad != nil {
		return bad
	}

	newContent := lineTransform(oldContent, q.line, q.re, q.after, q.state == "absent")
	changed := !exists || newContent != oldContent

	// check 模式：只读对比返回变更预估（--diff 产出内容级差异），不回写
	if rc.CheckMode {
		res := &Result{Changed: changed, Msg: fmt.Sprintf("[check] %s will %s", q.path, changeLabel(changed))}
		if changed && rc.DiffMode {
			res.Diff = diffText(oldContent, newContent, "remote "+q.path, "target "+q.path)
		}
		return res
	}
	return applyLineChange(rc, q, newContent, exists, changed)
}

// applyLineChange 实跑回写：可选备份 + 回滚登记 + 整体上传（已有文件保持
// 原权限，新建文件用 mode），收尾经 fixAttrs 校正属主漂移。
func applyLineChange(rc *RunContext, q *lineinfileReq, newContent string, exists, changed bool) *Result {
	if changed {
		if exists && q.backup {
			if bad := backupRemote(rc, q.path); bad != nil {
				return bad
			}
		}
		// 变更前登记回滚动作（auto_rollback）：已存在 → 快照恢复；新建 → 回滚时删除
		if rc.Rollback != nil {
			if exists {
				rc.Rollback.Snapshot(rc, q.path)
			} else {
				rc.Rollback.RecordRemove(q.path)
			}
		}
		// 已有文件保持原权限；新建文件用 mode（缺省 0644）
		uploadMode := q.mode
		if exists {
			if cur, ok, bad := remoteMode(rc, q.path); bad != nil {
				return bad
			} else if ok {
				uploadMode = cur
			}
		}
		if err := uploadBytes(rc, q.path, []byte(newContent), uploadMode, true); err != nil {
			return Fail("upload failed: %v", err)
		}
	}
	// 属主漂移才校正（与 copy 的幂等收尾一致：探测驱动，变更计入 changed；
	// mode 传 nil——已有文件保持原权限，lineinfile 收尾不动权限）
	_, fixedOwner, fbad := fixAttrs(rc, q.path, nil, q.owner, q.group)
	if fbad != nil {
		return fbad
	}
	if fixedOwner {
		changed = true
	}

	msg := fmt.Sprintf("%s is already in the desired state", q.path)
	if changed {
		msg = fmt.Sprintf("%s updated", q.path)
	}
	return &Result{Changed: changed, Msg: msg}
}

// lineTransform 按规则变换文件行集，返回新内容（变化与否由调用方字节比较判定）。
//   - present + regexp：替换首个匹配行为 line；无匹配按 insertafter 插入（缺省 EOF）
//   - present 无 regexp：整行精确匹配已存在则不动；否则按 insertafter 插入
//   - absent：删除全部匹配行（regexp 优先，否则整行精确匹配）
func lineTransform(old, line string, re, after *regexp.Regexp, absent bool) string {
	lines, trailing := splitFileLines(old)
	var out []string
	if absent {
		for _, l := range lines {
			match := re != nil && re.MatchString(l)
			if re == nil {
				match = l == line
			}
			if !match {
				out = append(out, l)
			}
		}
		joined := strings.Join(out, "\n")
		if len(out) > 0 && trailing {
			joined += "\n"
		}
		return joined
	}

	out = lines
	idx := -1 // 首个匹配行下标；-2 表示精确行已存在
	if re != nil {
		for i, l := range lines {
			if re.MatchString(l) {
				idx = i
				break
			}
		}
	} else {
		if slices.Contains(lines, line) {
			idx = -2
		}
	}
	switch {
	case idx == -2:
		// 已存在精确行：保持原样（不补行尾换行，保证幂等）
	case idx >= 0:
		out[idx] = line
	default: // 未命中：插入（insertafter 最后一个匹配行之后，缺省 EOF）
		pos := len(out)
		if after != nil {
			for i, o := range slices.Backward(out) {
				if after.MatchString(o) {
					pos = i + 1
					break
				}
			}
		}
		out = append(out, "")
		copy(out[pos+1:], out[pos:])
		out[pos] = line
		trailing = true // 插入的行补行尾换行
	}
	joined := strings.Join(out, "\n")
	// 尾换行仅两种来源：原文件本就有，或本次插入了新行。替换命中行
	// （idx>=0）不给原本无尾换行的文件追加换行——超出任务范围的字节
	// 变更对 sudoers/cron 等固定格式下游有实际风险。
	if len(out) > 0 && trailing {
		joined += "\n"
	}
	return joined
}

// splitFileLines 按行切分内容，返回行集与原内容是否带结尾换行。
func splitFileLines(s string) ([]string, bool) {
	if s == "" {
		return nil, false
	}
	trailing := strings.HasSuffix(s, "\n")
	if trailing {
		s = strings.TrimSuffix(s, "\n")
	}
	return strings.Split(s, "\n"), trailing
}
