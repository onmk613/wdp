package module

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"wdp/internal/i18n"
)

func init() {
	Register(&CopyModule{})
}

// CopyModule 将本地文件或字面量内容分发到远端（校验和幂等）。
type CopyModule struct{}

func (m *CopyModule) Name() string { return "copy" }

// RollbackCapability 变更经快照登记可自动回滚，且可用 file absent 逆操作卸载。
func (m *CopyModule) RollbackCapability() RollbackCapability { return RollbackFull }

func (m *CopyModule) Desc() string {
	return i18n.T("Distribute local files or literal content to remote hosts", "分发本地文件或字面量内容到远端")
}

// Run 执行分发（校验和幂等管线见 putFile）。
func (m *CopyModule) Run(rc *RunContext, args map[string]any, _ string) *Result {
	dest, ok := argStr(args, "dest")
	if !ok || dest == "" {
		return Fail("copy requires a dest parameter")
	}
	// content/src 以"键存在（且非 null）"判定而非"值非空"：content: ""
	// （显式空串，想写空文件——清空远端文件是合法的收敛目标）必须与
	// "键不存在"区分，旧的非空判定会把前者误报成缺参数。与 systemd_unit
	// 的互斥口径对齐：显式给了 content（哪怕空串）再给 src 即报互斥。
	_, hasContent := args["content"]
	if args["content"] == nil {
		hasContent = false
	}
	src, hasSrc := argStr(args, "src")
	if hasContent && hasSrc {
		return Fail("copy accepts either content or src, not both")
	}
	if !hasContent && !hasSrc {
		return Fail("copy requires a content or src parameter")
	}
	if hasSrc && src == "" {
		return Fail("src must not be empty")
	}
	content, _ := argStr(args, "content")
	owner, _ := argStr(args, "owner")
	group, _ := argStr(args, "group")
	backup, _ := argBool(args, "backup")

	var data []byte
	mode := fs.FileMode(0o644) // 缺省 0644；src 未显式给 mode 时沿用本地文件权限
	if src != "" {
		local, lerr := resolveLocal(rc, src)
		if lerr != nil {
			return Fail("%v", lerr)
		}
		b, err := readLocalSrc(rc, local)
		if err != nil {
			return Fail("failed to read local file: %v", err)
		}
		data = b
		if mv, ok := argMode(args, "mode"); ok {
			mode = mv.Perm()
		} else if fi, err := os.Stat(local); err == nil {
			mode = fi.Mode().Perm()
		}
	} else {
		data = []byte(content)
		if mv, ok := argMode(args, "mode"); ok {
			mode = mv.Perm()
		}
	}

	changed, res := putFile(rc, putFileOpts{data: data, dest: dest, mode: &mode, backup: backup, owner: owner, group: group})
	if res != nil {
		return res // 失败或 check 预估（含 --diff 内容差异）直接透传
	}
	msg := fmt.Sprintf("%s content is unchanged", dest)
	if changed {
		msg = fmt.Sprintf("distributed %d bytes to %s", len(data), dest)
	}
	return &Result{Changed: changed, Msg: msg}
}

// resolveLocal 解析 chart/playbook 引用的控制端本地路径，并把它约束在
// BaseDir（chart 目录 / playbook 所在目录）之内。
//
// chart 由 operator 级账号上传/编辑，故禁止绝对路径与 .. 穿越读（或经
// artifact cache 反向写）控制端任意文件；chart 内相对路径与 packages/
// 制品目录照常可用。
func resolveLocal(rc *RunContext, p string) (string, error) {
	base := rc.BaseDir
	if base == "" {
		base = "."
	}
	absBase, err := filepath.Abs(base)
	if err != nil {
		return "", fmt.Errorf("resolve base dir %q: %w", base, err)
	}
	joined := p
	if !filepath.IsAbs(joined) {
		joined = filepath.Join(absBase, p)
	}
	abs, err := filepath.Abs(joined)
	if err != nil {
		return "", fmt.Errorf("resolve %q: %w", p, err)
	}
	rel, err := filepath.Rel(absBase, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		// 绝对路径大概率是把目标机路径误当本地 src（get_url remote 下载后
		// 就地处理的常见写法）——提示直接指向修法
		hint := "; put the file inside the chart"
		if filepath.IsAbs(p) {
			hint = "; if this is a path on the target host, add remote_src: true"
		}
		return "", fmt.Errorf("path %q is outside the chart/playbook directory %s (absolute paths and .. escapes are refused%s)", p, absBase, hint)
	}
	return abs, nil
}

// Params 参数文档。顺序即使用顺序：src → dest 是主用法（与示例一致），
// 其余按使用频率排列——补全候选与 snippet 骨架都按此序展示。
func (m *CopyModule) Params() []ParamDoc {
	return []ParamDoc{
		{Name: "src", Type: "string", Desc: i18n.T("local source file path, relative to the chart/playbook (mutually exclusive with content)", "本地源文件路径，chart/playbook 内相对路径（与 content 互斥）")},
		{Name: "dest", Type: "string", Desc: i18n.T("remote destination path (required)", "远端目标路径（必填）")},
		{Name: "content", Type: "string", Desc: i18n.T("literal content (mutually exclusive with src; an empty string writes an empty file)", "字面量内容（与 src 互斥；空串写入空文件）")},
		{Name: "mode", Type: "mode", Default: "0644", Desc: i18n.T("mode (with src, defaults to the local file's permissions)", "权限位（src 方式缺省继承本地文件权限）")},
		{Name: "owner", Type: "string", Desc: i18n.T("owner (requires become)", "属主（需 become）")},
		{Name: "group", Type: "string", Desc: i18n.T("group (requires become)", "属组（需 become）")},
		{Name: "backup", Type: "bool", Default: "false", Desc: i18n.T("back up to dest.bak.<timestamp> before overwriting", "覆盖前先备份为 dest.bak.<时间戳>")},
	}
}

func (m *CopyModule) Example() string {
	return i18n.T(`- name: Push a static config file
  copy:
    src: files/app.conf
    dest: /etc/app/app.conf
    mode: "0644"
    backup: true
`, `- name: 下发静态配置
  copy:
    src: files/app.conf
    dest: /etc/app/app.conf
    mode: "0644"
    backup: true
`)
}
