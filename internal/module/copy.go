package module

import (
	"fmt"
	"os"
	"path/filepath"
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
		local := resolveLocal(rc, src)
		b, err := os.ReadFile(local)
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

// resolveLocal 解析 playbook 相对路径。
func resolveLocal(rc *RunContext, p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(rc.BaseDir, p)
}

// Params 参数文档。
func (m *CopyModule) Params() []ParamDoc {
	return []ParamDoc{
		{Name: "dest", Type: "string", Desc: "remote destination path (required)"},
		{Name: "content", Type: "string", Desc: "literal content (mutually exclusive with src)"},
		{Name: "src", Type: "string", Desc: "local source file path (mutually exclusive with content)"},
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
