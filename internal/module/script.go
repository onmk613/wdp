package module

import (
	"fmt"
	"os"
	"strings"

	"wdp/internal/shellquote"
)

func init() {
	Register(&ScriptModule{})
}

// ScriptModule 上传本地脚本到远端临时路径并执行。
type ScriptModule struct{}

// Name 模块名。
func (m *ScriptModule) Name() string { return "script" }

// Desc 模块说明。
func (m *ScriptModule) Desc() string {
	return "upload and execute a local script"
}

// Run 上传脚本（0755）到远端临时路径执行，结束自删。
// map 形式 src + free-form 参数；free-form 形式首 token 为脚本路径、其余为参数。
func (m *ScriptModule) Run(rc *RunContext, args map[string]any, free string) *Result {
	src, _ := argStr(args, "src")
	scriptArgs := free
	if src == "" {
		fields := strings.Fields(free)
		if len(fields) == 0 {
			return Fail("%s", "script requires a src parameter or a free-form script path")
		}
		src, scriptArgs = fields[0], strings.Join(fields[1:], " ")
	}
	data, err := os.ReadFile(resolveLocal(rc, src))
	if err != nil {
		return Fail("failed to read local script: %v", err)
	}
	if rc.CheckMode {
		return &Result{Changed: true, Msg: fmt.Sprintf("[check] will execute: %s %s", src, scriptArgs)}
	}

	remote := "/tmp/.wdp-script-" + tempSuffix()
	if err := uploadBytes(rc, remote, data, 0o755, true); err != nil {
		return Fail("failed to upload script: %v", err)
	}
	defer func() {
		_, _ = rc.exec("rm -f -- " + shellquote.Quote(remote))
	}()

	script := shellquote.Quote(remote)
	if scriptArgs != "" {
		// 参数逐词转义后拼接：free-form 中的 shell 元字符（$ ; && 反引号…）
		// 一律字面传给脚本，不经远端 shell 二次解释（与全项目 Quote 惯例一致）
		quoted, err := shellquote.QuoteWords(scriptArgs)
		if err != nil {
			return Fail("unterminated quote in script arguments: %v", err)
		}
		script += " " + quoted
	}
	out, bad := rc.exec(script)
	res := &Result{Stdout: out.Stdout, Stderr: out.Stderr, Rc: out.Code, Changed: true, Msg: src}
	if bad != nil {
		return bad
	}
	if out.Code != 0 {
		res.Failed = true
		res.Msg = fmt.Sprintf("script exit code rc=%d", out.Code)
	}
	return res
}

// Params 参数文档。
func (m *ScriptModule) Params() []ParamDoc {
	return []ParamDoc{
		{Name: "src", Type: "string", Desc: "local script path (chart/playbook-relative)"},
		{Name: "(free-form)", Type: "string", Desc: "arguments passed to the script"},
	}
}

// Example 示例任务。
func (m *ScriptModule) Example() string {
	return `- name: upload and run the migration script
  script: scripts/migrate.sh --verbose
`
}
