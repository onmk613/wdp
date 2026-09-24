package module

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func init() {
	Register(&CopyModule{})
}

// CopyModule 将本地文件或字面量内容分发到远端（校验和幂等）。
type CopyModule struct{}

// Name 模块名。
func (m *CopyModule) Name() string { return "copy" }

// RollbackCapability 变更经快照登记可自动回滚，且可用 file absent 逆操作卸载。
func (m *CopyModule) RollbackCapability() RollbackCapability { return RollbackFull }

// Desc 模块说明。
func (m *CopyModule) Desc() string {
	return "distribute local files or content to remote hosts"
}

// Run 执行分发（校验和幂等管线见 putFile）。
func (m *CopyModule) Run(rc *RunContext, args map[string]any, _ string) *Result {
	dest, ok := argStr(args, "dest")
	if !ok || dest == "" {
		return Fail("%s", "copy requires a dest parameter")
	}
	content, _ := argStr(args, "content")
	src, _ := argStr(args, "src")
	if content != "" && src != "" {
		return Fail("%s", "copy accepts either content or src, not both")
	}
	if content == "" && src == "" {
		return Fail("%s", "copy requires a content or src parameter")
	}
	owner, _ := argStr(args, "owner")
	group, _ := argStr(args, "group")
	backup, _ := argBool(args, "backup")

	var data []byte
	mode := int64(0o644) // 缺省 0644；src 未显式给 mode 时沿用本地文件权限
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
			mode = int64(mv.Perm())
		} else if fi, err := os.Stat(local); err == nil {
			mode = int64(fi.Mode().Perm())
		}
	} else {
		data = []byte(content)
		if mv, ok := argMode(args, "mode"); ok {
			mode = int64(mv.Perm())
		}
	}

	changed, res := putFile(rc, data, dest, mode, backup, true, owner, group)
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
// 为什么不许绝对路径与 .. 逃逸：chart 由 operator 级账号上传或编辑
// （app:create / app:upload），若 src/cache 可以是任意控制端路径，一句
// `copy: {src: ../../../../home/u/.ssh/id_rsa, dest: /tmp/x}` 就能把控制端
// 私钥、~/.wdp/releases/*.json（含 values 明文）分发到目标机；artifact 的
// cache 落盘方向还能反过来**写**控制端任意文件。相对路径、以及 chart 内
// 的 packages/ 制品目录照常可用（docs 的离线制品就放在 chart 内）。
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
		return "", fmt.Errorf("path %q is outside the chart/playbook directory %s (absolute paths and .. escapes are refused; put the file inside the chart)", p, absBase)
	}
	return abs, nil
}

// Params 参数文档。顺序即使用顺序：src → dest 是主用法（与示例一致），
// 其余按使用频率排列——补全候选与 snippet 骨架都按此序展示。
func (m *CopyModule) Params() []ParamDoc {
	return []ParamDoc{
		{Name: "src", Type: "string", Desc: "local source file path (mutually exclusive with content)"},
		{Name: "dest", Type: "string", Desc: "remote destination path (required)"},
		{Name: "content", Type: "string", Desc: "literal content (mutually exclusive with src)"},
		{Name: "mode", Type: "mode", Default: "0644", Desc: "mode (inherits the local file mode when src is set)"},
		{Name: "owner", Type: "string", Desc: "owner (requires become)"},
		{Name: "group", Type: "string", Desc: "group (requires become)"},
		{Name: "backup", Type: "bool", Default: "false", Desc: "back up as dest.bak.<timestamp> before overwriting"},
	}
}

// Example 示例任务。
func (m *CopyModule) Example() string {
	return `- name: push a static config
  copy:
    src: files/app.conf
    dest: /etc/app/app.conf
    mode: "0644"
    backup: true
`
}
