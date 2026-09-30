package module

import (
	"fmt"
	"strconv"
	"strings"

	"wdp/internal/i18n"
	"wdp/internal/shellquote"
)

func init() {
	Register(&StatModule{})
}

// StatModule 采集远端路径 facts（类型/权限/大小/属主/校验和），
// 并入主机变量域 stat 键（register 后可在后续任务引用）。只读，永不产生变更。
type StatModule struct{}

func (m *StatModule) Name() string { return "stat" }

func (m *StatModule) Desc() string {
	return i18n.T("Gather remote file/directory attributes into the stat variable (existence/owner/checksum)", "采集远端文件/目录属性进 stat 变量（存在性/属主/摘要）")
}

func (m *StatModule) Params() []ParamDoc {
	return []ParamDoc{
		{Name: "path", Type: "string", Desc: i18n.T("remote path", "远端路径")},
		{Name: "get_checksum", Type: "bool", Default: "true", Desc: i18n.T("gather sha256 (regular files up to 1MB only)", "采集 sha256（仅限 1MB 以内的普通文件）")},
		{Name: "follow", Type: "bool", Default: "false", Desc: i18n.T("follow symlinks and resolve the final target (readlink -f)", "跟随符号链接，取最终目标（readlink -f）")},
	}
}

func (m *StatModule) Example() string {
	return i18n.T(`- name: gather config file attributes (results land in .stat.*, same semantics as setup)
  stat:
    path: /etc/nginx/nginx.conf
    get_checksum: true

- name: restart only if the config exists
  service:
    name: nginx
    state: restarted
  when: '{{ .stat.exists }}'
`, `- name: 采集配置文件属性（结果落在 .stat.*，语义与 setup 相同）
  stat:
    path: /etc/nginx/nginx.conf
    get_checksum: true

- name: 配置存在才重启
  service:
    name: nginx
    state: restarted
  when: '{{ .stat.exists }}'
`)
}

// Run 执行采集。
func (m *StatModule) Run(rc *RunContext, args map[string]any, _ string) *Result {
	path, ok := argStr(args, "path")
	if !ok || path == "" {
		return Fail("stat requires a path parameter")
	}
	getChecksum := true
	if b, ok := argBool(args, "get_checksum"); ok {
		getChecksum = b
	}
	follow, _ := argBool(args, "follow")

	facts := map[string]any{
		"exists":   false,
		"isdir":    false,
		"islink":   false,
		"isfile":   false,
		"mode":     "",
		"size":     int64(0),
		"owner":    "",
		"group":    "",
		"checksum": "",
		"path":     path,
	}

	kind, bad := probePath(rc, path)
	if bad != nil {
		return bad
	}
	if kind == "missing" {
		return &Result{Msg: fmt.Sprintf("%s does not exist", path), Facts: map[string]any{"stat": facts}}
	}
	facts["exists"] = true
	origKind := kind

	// follow：符号链接解析到最终目标再统计（解析失败按原路径，islink 仍如实报告）。
	// follow=false 时 helper 的 stat 不带 -L（lstat 口径），mode/size/owner
	// 报链接自身的属性——两种口径各自一致
	target := path
	if follow && origKind == "link" {
		if resolved := canonicalLink(rc, path); resolved != "" {
			target = resolved
			if k2, bad := probePath(rc, target); bad != nil {
				return bad
			} else {
				kind = k2
			}
		}
	}
	facts["islink"] = origKind == "link"
	facts["isdir"] = kind == "directory"
	facts["isfile"] = kind == "file"

	if mode, ok, bad := remoteMode(rc, target); bad != nil {
		return bad
	} else if ok {
		facts["mode"] = fmt.Sprintf("%04o", mode)
	}
	var fileSize int64
	sizeKnown := false
	if size, ok, bad := remoteSize(rc, target); bad != nil {
		return bad
	} else if ok {
		fileSize, sizeKnown = size, true
		facts["size"] = size
	}
	if owner, group, ok, bad := remoteOwnerGroup(rc, target); bad != nil {
		return bad
	} else if ok {
		facts["owner"], facts["group"] = owner, group
	}
	// 校验和：普通文件且小于 1MB 才计算（复用 diff 上限的量级约束）
	if getChecksum && kind == "file" && sizeKnown && fileSize < maxDiffBytes {
		if sum, ok, bad := remoteChecksum(rc, target); bad != nil {
			return bad
		} else if ok {
			facts["checksum"] = sum
		}
	}

	// follow 解析后 kind=missing 只可能是 broken symlink（链接在而目标不在；
	// 原路径缺失已在上方提前返回）——"exists (missing)" 前后矛盾，换如实表述
	var msg string
	if kind == "missing" {
		msg = fmt.Sprintf("%s is a broken symlink (target missing)", path)
	} else {
		msg = fmt.Sprintf("%s exists (%s", path, kind)
		if s, _ := facts["mode"].(string); s != "" {
			msg += ", mode " + s
		}
		if kind == "file" && sizeKnown {
			msg += fmt.Sprintf(", %d bytes", fileSize)
		}
		msg += ")"
	}
	return &Result{Msg: msg, Facts: map[string]any{"stat": facts}}
}

// canonicalLink 解析符号链接的最终目标（readlink -f，失败返回空串）。
func canonicalLink(rc *RunContext, path string) string {
	out, bad := rc.exec(fmt.Sprintf("readlink -f -- %s", shellquote.Quote(path)))
	if bad != nil || out.Code != 0 {
		return ""
	}
	return strings.TrimSpace(out.Stdout)
}

// remoteSize 读取远端路径字节数（GNU stat 优先，BSD stat 兜底）。
func remoteSize(rc *RunContext, path string) (int64, bool, *Result) {
	script := fmt.Sprintf(`p=%s
[ -e "$p" ] || exit 3
s=$(stat -c %%s "$p" 2>/dev/null || stat -f %%z "$p" 2>/dev/null)
[ -n "$s" ] || exit 4
printf '%%s' "$s"
exit 0`, shellquote.Quote(path))
	out, bad := rc.exec(script)
	if bad != nil {
		return 0, false, bad
	}
	switch out.Code {
	case 0:
	case 3:
		return 0, false, nil
	default:
		return 0, false, Fail("failed to read size: %s", firstLine(out.Stderr))
	}
	n, err := strconv.ParseInt(strings.TrimSpace(out.Stdout), 10, 64)
	if err != nil || n < 0 {
		return 0, false, Fail("unable to parse size %q", out.Stdout)
	}
	return n, true, nil
}
