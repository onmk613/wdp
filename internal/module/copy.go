package module

import (
	"fmt"
	"os"
	"path/filepath"

	"wdp/internal/i18n"
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
	return i18n.T("distribute local files or content to remote hosts", "分发本地文件或 content 内容到远端")
}

// Run 执行分发（校验和幂等管线见 putFile）。
func (m *CopyModule) Run(rc *RunContext, args map[string]any, free string) *Result {
	dest, ok := argStr(args, "dest")
	if !ok || dest == "" {
		return Fail("%s", i18n.T("copy requires a dest parameter", "copy 需要 dest 参数"))
	}
	content, _ := argStr(args, "content")
	src, _ := argStr(args, "src")
	if content != "" && src != "" {
		return Fail("%s", i18n.T("copy accepts either content or src, not both", "copy 的 content 与 src 只能二选一"))
	}
	if content == "" && src == "" {
		return Fail("%s", i18n.T("copy requires a content or src parameter", "copy 需要 content 或 src 参数"))
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
			return Fail(i18n.T("failed to read local file: %v", "读取本地文件失败: %v"), err)
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
	msg := fmt.Sprintf(i18n.T("%s content is unchanged", "%s 内容一致"), dest)
	if changed {
		msg = fmt.Sprintf(i18n.T("distributed %d bytes to %s", "已分发 %d 字节到 %s"), len(data), dest)
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
		{Name: "dest", Type: "string", Desc: "远端目标路径（必需）"},
		{Name: "content", Type: "string", Desc: "字面量内容（与 src 二选一）"},
		{Name: "src", Type: "string", Desc: "本地源文件路径（与 content 二选一）"},
		{Name: "mode", Type: "mode", Default: "0644", Desc: "权限（src 未指定时沿用本地文件权限）"},
		{Name: "owner", Type: "string", Desc: "属主（需 become）"},
		{Name: "group", Type: "string", Desc: "属组（需 become）"},
		{Name: "backup", Type: "bool", Default: "false", Desc: "覆盖前备份为 dest.bak.<时间戳>"},
	}
}

// Example 示例任务。
func (m *CopyModule) Example() string {
	return `- name: 下发静态配置
  copy:
    src: files/app.conf
    dest: /etc/app/app.conf
    mode: "0644"
    backup: true
`
}
