package module

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"
	"time"

	"wdp/internal/i18n"
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
	return i18n.T("manage single lines in remote files (present/absent/replace)", "管理远端文件中的单行（存在/缺席/替换）")
}

// Params 参数文档。
func (m *LineinfileModule) Params() []ParamDoc {
	return []ParamDoc{
		{Name: "path", Type: "string", Desc: "远端文件路径"},
		{Name: "line", Type: "string", Desc: "目标行内容（state=absent 时可与 regexp 二选一）"},
		{Name: "regexp", Type: "string", Desc: "匹配目标行的正则：present 替换首个匹配行；absent 删除全部匹配行"},
		{Name: "state", Type: "string", Default: "present", Desc: "present 确保存在 / absent 确保缺席"},
		{Name: "insertafter", Type: "string", Default: "EOF", Desc: "未命中时的插入位置：最后一个匹配行之后（EOF 表示文件末尾）"},
		{Name: "create", Type: "bool", Default: "false", Desc: "文件不存在时创建（缺省报错）"},
		{Name: "backup", Type: "bool", Default: "false", Desc: "修改前备份原文件（path.bak.时间戳）"},
		{Name: "mode", Type: "mode", Default: "0644", Desc: "新建文件时的权限（已有文件保持原权限）"},
		{Name: "owner", Type: "string", Desc: "属主（需 become: true）"},
		{Name: "group", Type: "string", Desc: "属组（需 become: true）"},
	}
}

// Example 示例任务。
func (m *LineinfileModule) Example() string {
	return `- name: 确保授权行存在（文件缺失则创建）
  lineinfile:
    path: /etc/sudoers.d/deploy
    line: "deploy ALL=(ALL) NOPASSWD: ALL"
    create: true
    mode: "0440"

- name: 替换首个匹配行（改前备份）
  lineinfile:
    path: /etc/selinux/config
    regexp: "^SELINUX="
    line: "SELINUX=disabled"
    backup: true
`
}

// Run 执行行级变更。
func (m *LineinfileModule) Run(rc *RunContext, args map[string]any, free string) *Result {
	path, ok := argStr(args, "path")
	if !ok || path == "" {
		return Fail("%s", i18n.T("lineinfile requires a path parameter", "lineinfile 需要 path 参数"))
	}
	line, hasLine := argStr(args, "line")
	state, ok := parseState(args, "present", "present", "absent")
	if !ok {
		return Fail(i18n.T("unsupported state %q (options: present/absent)", "不支持的 state %q（可选: present/absent）"), state)
	}
	if state != "absent" && !hasLine {
		return Fail("%s", i18n.T("lineinfile requires a line parameter (state=present)", "lineinfile 需要 line 参数（state=present）"))
	}
	if state == "absent" {
		p, _ := argStr(args, "regexp")
		// line:"" 显式空串经 argStr 视为"已提供"——但空行匹配会删除文件中
		// 所有空行，语义上几乎必然是误用，与无 regexp 的 absent 一并拒绝
		if !hasLine || line == "" {
			if p == "" {
				return Fail("%s", i18n.T("state=absent requires a non-empty line or regexp (an empty line would match every blank line)", "state=absent 需要非空 line 或 regexp（空行会匹配全部空行）"))
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
		return Fail(i18n.T("setting owner/group requires become: true (%s)", "设置 owner/group 需要 become: true（%s）"), path)
	}

	var re, after *regexp.Regexp
	if pattern != "" {
		r, err := regexp.Compile(pattern)
		if err != nil {
			return Fail(i18n.T("unable to parse regexp: %v", "regexp 无法解析: %v"), err)
		}
		re = r
	}
	if afterPattern != "" && afterPattern != "EOF" {
		r, err := regexp.Compile(afterPattern)
		if err != nil {
			return Fail(i18n.T("unable to parse insertafter: %v", "insertafter 无法解析: %v"), err)
		}
		after = r
	}

	// 读取远端内容（下载为只读探测，check 模式同样允许）
	var oldContent string
	exists := true
	var buf bytes.Buffer
	if err := rc.Conn.DownloadFile(rc.Ctx, path, &buf); err != nil {
		exists = false
		if state == "absent" {
			return &Result{Msg: fmt.Sprintf(i18n.T("%s does not exist, no change needed for state=absent", "%s 不存在，state=absent 无需变更"), path)}
		}
		if !create {
			return Fail(i18n.T("file does not exist: %s (use create: true to create it)", "文件不存在: %s（可用 create: true 创建）"), path)
		}
	} else {
		oldContent = buf.String()
	}

	newContent := lineTransform(oldContent, line, re, after, state == "absent")
	changed := !exists || newContent != oldContent

	// check 模式：只读对比返回变更预估（--diff 产出内容级差异），不回写
	if rc.CheckMode {
		res := &Result{Changed: changed, Msg: fmt.Sprintf(i18n.T("[check] %s will %s", "[check] %s 将%s"), path, changeLabel(changed))}
		if changed && rc.DiffMode {
			res.Diff = diffText(oldContent, newContent, i18n.T("remote ", "远端 ")+path, i18n.T("target ", "目标 ")+path)
		}
		return res
	}

	if changed {
		if exists && backup {
			bak := fmt.Sprintf("%s.bak.%d", path, time.Now().Unix())
			if out, bad := rc.exec(fmt.Sprintf("cp -a -- %s %s", shellquote.Quote(path), shellquote.Quote(bak))); bad != nil {
				return bad
			} else if out.Code != 0 {
				return Fail(i18n.T("backup failed: %s", "备份失败: %s"), firstLine(out.Stderr))
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
			return Fail(i18n.T("upload failed: %v", "上传失败: %v"), err)
		}
	}
	// 属主漂移才校正（与 copy 的幂等收尾一致：探测驱动，变更计入 changed）
	if owner != "" || group != "" {
		if co, cg, ok, obad := remoteOwnerGroup(rc, path); obad != nil {
			return obad
		} else if !ok || co != owner || cg != group {
			if bad := chownPath(rc, path, owner, group); bad != nil {
				return bad
			}
			changed = true
		}
	}

	msg := fmt.Sprintf(i18n.T("%s is already in the desired state", "%s 已是期望状态"), path)
	if changed {
		msg = fmt.Sprintf(i18n.T("%s updated", "%s 已更新"), path)
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
		for _, l := range lines {
			if l == line {
				idx = -2
				break
			}
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
			for i := len(out) - 1; i >= 0; i-- {
				if after.MatchString(out[i]) {
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
	if len(out) > 0 && (trailing || idx >= 0) {
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
