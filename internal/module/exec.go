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

func (m *ShellModule) Name() string { return "shell" }

func (m *ShellModule) Desc() string {
	return i18n.T("Run commands on the target host via /bin/sh (supports pipes and other shell syntax)", "经 /bin/sh 在远端执行命令（支持管道等 shell 语法）")
}

// Run 执行 free-form 命令。
func (m *ShellModule) Run(rc *RunContext, args map[string]any, free string) *Result {
	return runCommandModule(rc, args, free)
}

// CommandModule 直接执行命令（与 shell 行为一致：远端统一经 /bin/sh）。
type CommandModule struct{}

func (m *CommandModule) Name() string { return "command" }

func (m *CommandModule) Desc() string {
	return i18n.T("Run commands on the target host (exactly equivalent to shell, also via /bin/sh)", "在远端执行命令（与 shell 完全等价，同样经 /bin/sh）")
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
		return Fail("shell/command requires command content, e.g. `shell: uptime`")
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
	if bad != nil {
		return bad
	}
	res := &Result{
		Stdout:  out.Stdout,
		Stderr:  out.Stderr,
		Rc:      out.Code,
		Changed: true,
	}
	if out.Code != 0 {
		res.Failed = true
		res.Msg = fmt.Sprintf("non-zero exit code rc=%d", out.Code)
	}
	return res
}

func (m *ShellModule) Params() []ParamDoc {
	return []ParamDoc{
		{Name: "(free-form)", Type: "string", Desc: i18n.T("command to run, given as the module's own value: `shell: uptime` (command is exactly equivalent to shell)", "要执行的命令，写在模块键本位：`shell: uptime`（command 与 shell 完全等价）")},
		{Name: "cmd", Type: "string", Desc: i18n.T("command content (alternative form when free-form is empty)", "命令内容（free-form 为空时的替代写法）")},
		{Name: "creates", Type: "string", Desc: i18n.T("skip if this path already exists (idempotency guard)", "该路径已存在则跳过（幂等守卫）")},
		{Name: "removes", Type: "string", Desc: i18n.T("skip if this path does not exist (idempotency guard)", "该路径不存在则跳过（幂等守卫）")},
		{Name: "chdir", Type: "string", Desc: i18n.T("change to this directory before running", "执行前先切换到该目录")},
	}
}

func (m *ShellModule) Example() string {
	return i18n.T(`- name: Wait for the service to become ready (auto-retry on failure)
  shell: 'curl -sf http://localhost:{{ .app.port }}/health'
  until: '{{ if eq .result.rc 0 }}ok{{ end }}'
  retries: 10
  delay: 3
`, `- name: 等待服务就绪（失败自动重试）
  shell: 'curl -sf http://localhost:{{ .app.port }}/health'
  until: '{{ if eq .result.rc 0 }}ok{{ end }}'
  retries: 10
  delay: 3
`)
}

// Params 参数文档（command 与 shell 同构）。
func (m *CommandModule) Params() []ParamDoc { return (&ShellModule{}).Params() }

func (m *CommandModule) Example() string {
	return i18n.T(`- name: Idempotent run (skip if the artifact already exists)
  command: ./migrate.sh
  args:
    creates: /opt/app/.migrated
`, `- name: 幂等执行（产物已存在则跳过）
  command: ./migrate.sh
  args:
    creates: /opt/app/.migrated
`)
}
