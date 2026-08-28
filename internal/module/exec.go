package module

import (
	"fmt"

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
	return "run commands on remote hosts via sh"
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
	return "run commands on remote hosts (same as shell; both go through /bin/sh)"

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
		return Fail("%s", "shell/command requires command content, e.g. `shell: uptime`")
	}
	// creates: 文件已存在则跳过（幂等保护）
	if creates, ok := argStr(args, "creates"); ok && creates != "" {
		out, bad := rc.exec(fmt.Sprintf("[ -e %s ]", shellquote.Quote(creates)))
		if bad != nil {
			return bad
		}
		if out.Code == 0 {
			return &Result{Msg: fmt.Sprintf("%s already exists, skipping", creates)}
		}
	}
	// removes: 文件不存在则跳过
	if removes, ok := argStr(args, "removes"); ok && removes != "" {
		out, bad := rc.exec(fmt.Sprintf("[ -e %s ]", shellquote.Quote(removes)))
		if bad != nil {
			return bad
		}
		if out.Code != 0 {
			return &Result{Msg: fmt.Sprintf("%s does not exist, skipping", removes)}
		}
	}
	if cwd, ok := argStr(args, "chdir"); ok && cwd != "" {
		script = fmt.Sprintf("cd %s && %s", shellquote.Quote(cwd), script)
	}
	if rc.CheckMode {
		return &Result{Changed: true, Msg: "[check] will execute: " + firstLine(script)}
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
		res.Msg = fmt.Sprintf("non-zero exit code rc=%d", out.Code)
	}
	return res
}

// Params 参数文档。
func (m *ShellModule) Params() []ParamDoc {
	return []ParamDoc{
		{Name: "(free-form)", Type: "string", Desc: "command to run (shell and command are identical: both run via /bin/sh)"},
		{Name: "cmd", Type: "string", Desc: "command (alternative when free-form is empty)"},
		{Name: "creates", Type: "string", Desc: "skip when this path exists (idempotency guard)"},
		{Name: "removes", Type: "string", Desc: "skip when this path is missing (idempotency guard)"},
		{Name: "chdir", Type: "string", Desc: "chdir here before execution"},
	}
}

// Example 示例任务。
func (m *ShellModule) Example() string {
	return `- name: wait for the service to be ready
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
	return `- name: idempotent run (skipped once the artifact exists)
  command: ./migrate.sh
  args:
    creates: /opt/app/.migrated
`
}
