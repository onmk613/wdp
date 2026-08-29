package module

import (
	"bytes"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"wdp/internal/shellquote"
)

func init() {
	Register(&LineinfileModule{})
}

// LineinfileModule 确保远端文件中的某行存在/缺席/被替换：
// 下载 → 控制端变换行集 → 整体回传（幂等，check/diff/回滚齐全）。
type LineinfileModule struct{}

// Name 模块名。
func (m *LineinfileModule) Name() string { return "lineinfile" }

// Desc 模块说明。
func (m *LineinfileModule) Desc() string {
	return "manage single lines in remote files (present/absent/replace)"
}

// Params 参数文档。
func (m *LineinfileModule) Params() []ParamDoc {
	return []ParamDoc{
		{Name: "path", Type: "string", Desc: "remote file path"},
		{Name: "line", Type: "string", Desc: "desired line content (for state=absent, either line or regexp)"},
		{Name: "regexp", Type: "string", Desc: "regex matching target lines: present replaces the first match; absent removes all matches"},
		{Name: "state", Type: "string", Default: "present", Desc: "present ensures the line exists / absent ensures it is gone"},
		{Name: "insertafter", Type: "string", Default: "EOF", Desc: "insertion point when no match: after the last matched line (EOF means end of file)"},
		{Name: "create", Type: "bool", Default: "false", Desc: "create the file when missing (default is to fail)"},
		{Name: "backup", Type: "bool", Default: "false", Desc: "back up before modifying (path.bak.<timestamp>)"},
		{Name: "mode", Type: "mode", Default: "0644", Desc: "mode for newly created files (existing files keep their mode)"},
		{Name: "owner", Type: "string", Desc: "owner (requires become: true)"},
		{Name: "group", Type: "string", Desc: "group (requires become: true)"},
	}
}

// Example 示例任务。
func (m *LineinfileModule) Example() string {
	return `- name: ensure the grant line exists (create the file if missing)
  lineinfile:
    path: /etc/sudoers.d/deploy
    line: "deploy ALL=(ALL) NOPASSWD: ALL"
    create: true
    mode: "0440"

- name: replace the first matching line (back up first)
  lineinfile:
    path: /etc/selinux/config
    regexp: "^SELINUX="
    line: "SELINUX=disabled"
    backup: true
`
}

// Run 执行行级变更。
func (m *LineinfileModule) Run(rc *RunContext, args map[string]any, _ string) *Result {
	path, ok := argStr(args, "path")
	if !ok || path == "" {
		return Fail("%s", "lineinfile requires a path parameter")
	}
	line, hasLine := argStr(args, "line")
	state, ok := parseState(args, "present", "present", "absent")
	if !ok {
		return Fail("unsupported state %q (options: present/absent)", state)
	}
	if state != "absent" && !hasLine {
		return Fail("%s", "lineinfile requires a line parameter (state=present)")
	}
	if state == "absent" {
		p, _ := argStr(args, "regexp")
		// line:"" 显式空串经 argStr 视为"已提供"——但空行匹配会删除文件中
		// 所有空行，语义上几乎必然是误用，与无 regexp 的 absent 一并拒绝
		if !hasLine || line == "" {
			if p == "" {
				return Fail("%s", "state=absent requires a non-empty line or regexp (an empty line would match every blank line)")
			}
		}
	}
	pattern, _ := argStr(args, "regexp")
	afterPattern, _ := argStr(args, "insertafter")
	create, _ := argBool(args, "create")
	backup, _ := argBool(args, "backup")
	owner, _ := argStr(args, "owner")
	group, _ := argStr(args, "group")
	mode := int64(0o644) // 仅 create 新建时生效；已有文件保持原权限
	if mv, ok := argMode(args, "mode"); ok {
		mode = int64(mv.Perm())
	}
	if (owner != "" || group != "") && !rc.Become {
		return Fail("setting owner/group requires become: true (%s)", path)
	}

	var re, after *regexp.Regexp
	if pattern != "" {
		r, err := regexp.Compile(pattern)
		if err != nil {
			return Fail("unable to parse regexp: %v", err)
		}
		re = r
	}
	if afterPattern != "" && afterPattern != "EOF" {
		r, err := regexp.Compile(afterPattern)
		if err != nil {
			return Fail("unable to parse insertafter: %v", err)
		}
		after = r
	}

	// 读取远端内容（下载为只读探测，check 模式同样允许）。
	// 存在性判定用显式探测：下载失败的语义是错误（权限/断连），
	// 不能与"文件不存在"混为一谈——absent 时混同会吞错报"无需变更"。
	var oldContent string
	probe := fmt.Sprintf(`p=%s
[ -e "$p" ] || exit 3
[ -f "$p" ] || exit 4`, shellquote.Quote(path))
	pout, pbad := rc.exec(probe)
	if pbad != nil {
		return pbad
	}
	switch pout.Code {
	case 4:
		return Fail("remote path %s exists and is not a regular file", path)
	case 0:
		var buf bytes.Buffer
		if err := rc.Conn.DownloadFile(rc.Ctx, path, &buf); err != nil {
			return Fail("failed to read %s: %v", path, err)
		}
		oldContent = buf.String()
	default:
		if state == "absent" {
			return &Result{Msg: fmt.Sprintf("%s does not exist, no change needed for state=absent", path)}
		}
		if !create {
			return Fail("file does not exist: %s (use create: true to create it)", path)
		}
	}
	exists := pout.Code == 0

	newContent := lineTransform(oldContent, line, re, after, state == "absent")
	changed := !exists || newContent != oldContent

	// check 模式：只读对比返回变更预估（--diff 产出内容级差异），不回写
	if rc.CheckMode {
		res := &Result{Changed: changed, Msg: fmt.Sprintf("[check] %s will %s", path, changeLabel(changed))}
		if changed && rc.DiffMode {
			res.Diff = diffText(oldContent, newContent, "remote "+path, "target "+path)
		}
		return res
	}

	if changed {
		if exists && backup {
			bak := fmt.Sprintf("%s.bak.%d", path, time.Now().UnixNano()) // 亚秒：同秒二次备份不再覆盖
			if out, bad := rc.exec(fmt.Sprintf("cp -a -- %s %s", shellquote.Quote(path), shellquote.Quote(bak))); bad != nil {
				return bad
			} else if out.Code != 0 {
				return Fail("backup failed: %s", firstLine(out.Stderr))
			}
		}
		// 变更前登记回滚动作（auto_rollback）：已存在 → 快照恢复；新建 → 回滚时删除
		if rc.Rollback != nil {
			if exists {
				rc.Rollback.Snapshot(rc, path)
			} else {
				rc.Rollback.RecordRemove(path)
			}
		}
		// 已有文件保持原权限；新建文件用 mode（缺省 0644）
		uploadMode := mode
		if exists {
			if cur, ok, bad := remoteMode(rc, path); bad != nil {
				return bad
			} else if ok {
				uploadMode = cur
			}
		}
		if err := uploadBytes(rc, path, []byte(newContent), uploadMode, true); err != nil {
			return Fail("upload failed: %v", err)
		}
	}
	// 属主漂移才校正（与 copy 的幂等收尾一致：探测驱动，变更计入 changed）
	if owner != "" || group != "" {
		if co, cg, ok, obad := remoteOwnerGroup(rc, path); obad != nil {
			return obad
		} else if !ok || (owner != "" && co != owner) || (group != "" && cg != group) {
			if bad := chownPath(rc, path, owner, group); bad != nil {
				return bad
			}
			changed = true
		}
	}

	msg := fmt.Sprintf("%s is already in the desired state", path)
	if changed {
		msg = fmt.Sprintf("%s updated", path)
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
