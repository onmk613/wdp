package module

import (
	"fmt"

	"wdp/internal/i18n"
	"wdp/internal/shellquote"
)

func init() {
	Register(&ShellModule{})
	Register(&CommandModule{})
}

// ShellModule 以 /bin/sh 执行命令（支持管道与变量展开）。
type ShellModule struct{}

// Name 模块名。
func (m *ShellModule) Name() string { return "shell" }

// Desc 模块说明。
func (m *ShellModule) Desc() string {
	return i18n.T("run commands on remote hosts via sh", "在远端以 sh 执行命令")
}

// Run 执行 free-form 命令。
func (m *ShellModule) Run(rc *RunContext, args map[string]any, free string) *Result {
	return runCommandModule(rc, args, free)
}

// CommandModule 直接执行命令（与 shell 行为一致：远端统一经 /bin/sh）。
type CommandModule struct{}

// Name 模块名。
func (m *CommandModule) Name() string { return "command" }

// Desc 模块说明。
func (m *CommandModule) Desc() string {
	return i18n.T("run commands on remote hosts (same as shell; both go through /bin/sh)",
		"在远端执行命令（与 shell 一致，都经 /bin/sh）")
}

// Run 执行命令（同一实现）。
func (m *CommandModule) Run(rc *RunContext, args map[string]any, free string) *Result {
	return runCommandModule(rc, args, free)
}

func runCommandModule(rc *RunContext, args map[string]any, free string) *Result {
	script := free
	if script == "" {
		if s, ok := argStr(args, "cmd"); ok {
			script = s
		}
	}
	if script == "" {
		return Fail("%s", i18n.T("shell/command requires command content, e.g. `shell: uptime`", "shell/command 需要命令内容，如 `shell: uptime`"))
	}
	// creates: 文件已存在则跳过（幂等保护）
	if creates, ok := argStr(args, "creates"); ok && creates != "" {
		out, bad := rc.exec(fmt.Sprintf("[ -e %s ]", shellquote.Quote(creates)))
		if bad != nil {
			return bad
		}
		if out.Code == 0 {
			return &Result{Msg: fmt.Sprintf(i18n.T("%s already exists, skipping", "%s 已存在，跳过"), creates)}
		}
	}
	// removes: 文件不存在则跳过
	if removes, ok := argStr(args, "removes"); ok && removes != "" {
		out, bad := rc.exec(fmt.Sprintf("[ -e %s ]", shellquote.Quote(removes)))
		if bad != nil {
			return bad
		}
		if out.Code != 0 {
			return &Result{Msg: fmt.Sprintf(i18n.T("%s does not exist, skipping", "%s 不存在，跳过"), removes)}
		}
	}
	if cwd, ok := argStr(args, "chdir"); ok && cwd != "" {
		script = fmt.Sprintf("cd %s && %s", shellquote.Quote(cwd), script)
	}
	if rc.CheckMode {
		return &Result{Changed: true, Msg: i18n.T("[check] will execute: ", "[check] 将执行: ") + firstLine(script)}
	}
	out, bad := rc.exec(script)
	res := &Result{
		Stdout:  out.Stdout,
		Stderr:  out.Stderr,
		Rc:      out.Code,
		Changed: true,
	}
	if bad != nil {
		return bad
	}
	if out.Code != 0 {
		res.Failed = true
		res.Msg = fmt.Sprintf(i18n.T("non-zero exit code rc=%d", "非零退出码 rc=%d"), out.Code)
	}
	return res
}

// Params 参数文档。
func (m *ShellModule) Params() []ParamDoc {
	return []ParamDoc{
		{Name: "(free-form)", Type: "string", Desc: "要执行的命令（shell/command 一致：均经 /bin/sh 执行）"},
		{Name: "cmd", Type: "string", Desc: "命令（free-form 为空时的替代写法）"},
		{Name: "creates", Type: "string", Desc: "路径存在则跳过（幂等守卫）"},
		{Name: "removes", Type: "string", Desc: "路径不存在则跳过（幂等守卫）"},
		{Name: "chdir", Type: "string", Desc: "执行前 cd 到该目录"},
	}
}

// Example 示例任务。
func (m *ShellModule) Example() string {
	return `- name: 等待服务就绪
  shell: 'curl -sf http://localhost:{{ .app.port }}/health'
  until: '{{ if eq .result.rc 0 }}ok{{ end }}'
  retries: 10
  delay: 3
`
}

// Params 参数文档（command 与 shell 同构）。
func (m *CommandModule) Params() []ParamDoc { return (&ShellModule{}).Params() }

// Example 示例任务。
func (m *CommandModule) Example() string {
	return `- name: 幂等执行（产物存在则跳过）
  command: ./migrate.sh
  args:
    creates: /opt/app/.migrated
`
}
