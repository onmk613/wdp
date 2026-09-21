package module

import (
	"fmt"
	"os"

	"wdp/internal/render"
)

func init() {
	Register(&TemplateModule{})
}

// TemplateModule 本地渲染 Go 模板后分发到远端（校验和幂等）。
type TemplateModule struct{}

// Name 模块名。
func (m *TemplateModule) Name() string { return "template" }

// RollbackCapability 变更经快照登记可自动回滚，且可用 file absent 逆操作卸载。
func (m *TemplateModule) RollbackCapability() RollbackCapability { return RollbackFull }

// Desc 模块说明。
func (m *TemplateModule) Desc() string {
	return "render Go templates and distribute to remote hosts"
}

// Run 渲染本地模板并经 putFile 幂等分发（幂等/备份/回滚/check/diff 语义同 copy）。
func (m *TemplateModule) Run(rc *RunContext, args map[string]any, _ string) *Result {
	src, ok := argStr(args, "src")
	if !ok || src == "" {
		return Fail("%s", "template requires a src parameter")
	}
	dest, ok := argStr(args, "dest")
	if !ok || dest == "" {
		return Fail("%s", "template requires a dest parameter")
	}
	tpl, err := os.ReadFile(resolveLocal(rc, src))
	if err != nil {
		return Fail("failed to read template: %v", err)
	}
	engine := rc.Engine
	if engine == nil {
		engine = render.DefaultEngine()
	}
	rendered, err := engine.Render(string(tpl), rc.Vars)
	if err != nil {
		return Fail("template rendering failed: %v", err)
	}

	mode := int64(0o644)
	if mv, ok := argMode(args, "mode"); ok {
		mode = int64(mv.Perm())
	}
	owner, _ := argStr(args, "owner")
	group, _ := argStr(args, "group")
	backup, _ := argBool(args, "backup")

	changed, res := putFile(rc, []byte(rendered), dest, mode, backup, true, owner, group)
	if res != nil {
		return res
	}
	msg := fmt.Sprintf("%s content is unchanged", dest)
	if changed {
		msg = fmt.Sprintf("rendered %s to %s", src, dest)
	}
	return &Result{Changed: changed, Msg: msg}
}

// Params 参数文档。
func (m *TemplateModule) Params() []ParamDoc {
	return []ParamDoc{
		{Name: "src", Type: "string", Desc: "local Go template path (chart-relative, helpers available)"},
		{Name: "dest", Type: "string", Desc: "remote destination path"},
		{Name: "mode", Type: "mode", Default: "0644", Desc: "mode"},
		{Name: "owner", Type: "string", Desc: "owner (requires become)"},
		{Name: "group", Type: "string", Desc: "group (requires become)"},
		{Name: "backup", Type: "bool", Default: "false", Desc: "back up before overwriting"},
	}
}

// Example 示例任务。
func (m *TemplateModule) Example() string {
	return `- name: render and push the config
  template:
    src: templates/app.conf.tpl
    dest: "{{ .global.workdir }}/app.conf"
  notify: reload app
`
}
