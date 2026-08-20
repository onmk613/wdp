package module

import (
	"fmt"
	"os"

	"wdp/internal/i18n"
	"wdp/internal/render"
)

func init() {
	Register(&TemplateModule{})
}

// TemplateModule 本地渲染 Go 模板后分发到远端（校验和幂等）。
type TemplateModule struct{}

// Name 模块名。
func (m *TemplateModule) Name() string { return "template" }

// Desc 模块说明。
func (m *TemplateModule) Desc() string {
	return i18n.T("render Go templates and distribute to remote hosts", "渲染 Go 模板并分发到远端")
}

// Run 渲染本地模板并经 putFile 幂等分发（幂等/备份/回滚/check/diff 语义同 copy）。
func (m *TemplateModule) Run(rc *RunContext, args map[string]any, free string) *Result {
	src, ok := argStr(args, "src")
	if !ok || src == "" {
		return Fail("template 需要 src 参数")
	}
	dest, ok := argStr(args, "dest")
	if !ok || dest == "" {
		return Fail("template 需要 dest 参数")
	}
	tpl, err := os.ReadFile(resolveLocal(rc, src))
	if err != nil {
		return Fail("读取模板失败: %v", err)
	}
	engine := rc.Engine
	if engine == nil {
		engine = render.DefaultEngine()
	}
	rendered, err := engine.Render(string(tpl), rc.Vars)
	if err != nil {
		return Fail("模板渲染失败: %v", err)
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
	msg := fmt.Sprintf("%s 内容一致", dest)
	if changed {
		msg = fmt.Sprintf("已渲染 %s 到 %s", src, dest)
	}
	return &Result{Changed: changed, Msg: msg}
}

// Params 参数文档。
func (m *TemplateModule) Params() []ParamDoc {
	return []ParamDoc{
		{Name: "src", Type: "string", Desc: "本地 Go 模板路径（chart 相对，可用 helpers）"},
		{Name: "dest", Type: "string", Desc: "远端目标路径"},
		{Name: "mode", Type: "mode", Default: "0644", Desc: "权限"},
		{Name: "owner", Type: "string", Desc: "属主（需 become）"},
		{Name: "group", Type: "string", Desc: "属组（需 become）"},
		{Name: "backup", Type: "bool", Default: "false", Desc: "覆盖前备份"},
	}
}

// Example 示例任务。
func (m *TemplateModule) Example() string {
	return `- name: 渲染并下发配置
  template:
    src: templates/app.conf.tpl
    dest: "{{ .global.workdir }}/app.conf"
  notify: 重载应用
`
}
