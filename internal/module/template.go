package module

import (
	"fmt"
	"io/fs"
	"wdp/internal/i18n"
)

func init() {
	Register(&TemplateModule{})
}

// TemplateModule 本地渲染 Go 模板后分发到远端（校验和幂等）。
type TemplateModule struct{}

func (m *TemplateModule) Name() string { return "template" }

// RollbackCapability 变更经快照登记可自动回滚，且可用 file absent 逆操作卸载。
func (m *TemplateModule) RollbackCapability() RollbackCapability { return RollbackFull }

func (m *TemplateModule) Desc() string {
	return i18n.T("Render a Go template and distribute it to the remote host", "渲染 Go 模板并分发到远端")
}

// Run 渲染本地模板并经 putFile 幂等分发（幂等/备份/回滚/check/diff 语义同 copy）。
func (m *TemplateModule) Run(rc *RunContext, args map[string]any, _ string) *Result {
	src, ok := argStr(args, "src")
	if !ok || src == "" {
		return Fail("template requires a src parameter")
	}
	dest, ok := argStr(args, "dest")
	if !ok || dest == "" {
		return Fail("template requires a dest parameter")
	}
	local, lerr := resolveLocal(rc, src)
	if lerr != nil {
		return Fail("%v", lerr)
	}
	tpl, err := readLocalSrc(rc, local)
	if err != nil {
		return Fail("failed to read template: %v", err)
	}
	rendered, err := rc.engine().Render(string(tpl), rc.Vars)
	if err != nil {
		return Fail("template rendering failed: %v", err)
	}

	mode := fs.FileMode(0o644)
	if mv, ok := argMode(args, "mode"); ok {
		mode = mv.Perm()
	}
	owner, _ := argStr(args, "owner")
	group, _ := argStr(args, "group")
	backup, _ := argBool(args, "backup")

	changed, res := putFile(rc, putFileOpts{data: []byte(rendered), dest: dest, mode: &mode, backup: backup, owner: owner, group: group})
	if res != nil {
		return res
	}
	msg := fmt.Sprintf("%s content is unchanged", dest)
	if changed {
		msg = fmt.Sprintf("rendered %s to %s", src, dest)
	}
	return &Result{Changed: changed, Msg: msg}
}

func (m *TemplateModule) Params() []ParamDoc {
	return []ParamDoc{
		{Name: "src", Type: "string", Desc: i18n.T("local template path (relative to the chart; _helpers.tpl definitions are available)", "本地模板路径（chart 内相对路径，可用 _helpers.tpl 定义）")},
		{Name: "dest", Type: "string", Desc: i18n.T("remote destination path", "远端目标路径")},
		{Name: "mode", Type: "mode", Default: "0644", Desc: i18n.T("permission bits", "权限位")},
		{Name: "owner", Type: "string", Desc: i18n.T("owner (requires become)", "属主（需 become）")},
		{Name: "group", Type: "string", Desc: i18n.T("group (requires become)", "属组（需 become）")},
		{Name: "backup", Type: "bool", Default: "false", Desc: i18n.T("back up before overwriting", "覆盖前先备份")},
	}
}

func (m *TemplateModule) Example() string {
	return i18n.T(`- name: render and push the config
  template:
    src: templates/app.conf.tpl
    dest: "{{ .global.workdir }}/app.conf"
  notify: reload app
`, `- name: 渲染并下发配置
  template:
    src: templates/app.conf.tpl
    dest: "{{ .global.workdir }}/app.conf"
  notify: reload app
`)
}
