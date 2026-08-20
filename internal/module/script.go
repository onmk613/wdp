package module

import (
	"fmt"
	"os"
	"strings"
	"time"

	"wdp/internal/i18n"
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
	return i18n.T("upload and execute a local script", "上传并执行本地脚本")
}

// Run 上传脚本（0755）到远端临时路径执行，结束自删。
// map 形式 src + free-form 参数；free-form 形式首 token 为脚本路径、其余为参数。
func (m *ScriptModule) Run(rc *RunContext, args map[string]any, free string) *Result {
	src, _ := argStr(args, "src")
	scriptArgs := free
	if src == "" {
		fields := strings.Fields(free)
		if len(fields) == 0 {
			return Fail("script 需要 src 参数或 free-form 脚本路径")
		}
		src, scriptArgs = fields[0], strings.Join(fields[1:], " ")
	}
	data, err := os.ReadFile(resolveLocal(rc, src))
	if err != nil {
		return Fail("读取本地脚本失败: %v", err)
	}
	if rc.CheckMode {
		return &Result{Changed: true, Msg: fmt.Sprintf("[check] 将执行: %s %s", src, scriptArgs)}
	}

	remote := fmt.Sprintf("/tmp/.wdp-script-%d", time.Now().UnixNano())
	if err := uploadBytes(rc, remote, data, 0o755, true); err != nil {
		return Fail("上传脚本失败: %v", err)
	}
	defer func() {
		_, _ = rc.exec("rm -f -- " + shellquote.Quote(remote))
	}()

	script := shellquote.Quote(remote)
	if scriptArgs != "" {
		script += " " + scriptArgs
	}
	out, bad := rc.exec(script)
	res := &Result{Stdout: out.Stdout, Stderr: out.Stderr, Rc: out.Code, Changed: true, Msg: src}
	if bad != nil {
		return bad
	}
	if out.Code != 0 {
		res.Failed = true
		res.Msg = fmt.Sprintf("脚本退出码 rc=%d", out.Code)
	}
	return res
}

// Params 参数文档。
func (m *ScriptModule) Params() []ParamDoc {
	return []ParamDoc{
		{Name: "src", Type: "string", Desc: "本地脚本路径（chart/playbook 相对路径）"},
		{Name: "(free-form)", Type: "string", Desc: "传给脚本的参数"},
	}
}

// Example 示例任务。
func (m *ScriptModule) Example() string {
	return `- name: 上传并执行迁移脚本
  script: scripts/migrate.sh --verbose
`
}
