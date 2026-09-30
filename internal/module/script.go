package module

import (
	"fmt"
	"strings"

	"wdp/internal/i18n"
	"wdp/internal/shellquote"
)

func init() {
	Register(&ScriptModule{})
}

// ScriptModule 上传本地脚本到远端临时路径并执行。
type ScriptModule struct{}

func (m *ScriptModule) Name() string { return "script" }

func (m *ScriptModule) Desc() string {
	return i18n.T("Upload a local script to the target host and run it (arguments allowed)", "上传本地脚本到目标机执行（可带参数）")
}

// Run 上传脚本（0755）到远端临时路径执行，结束自删。
// map 形式 src + free-form 参数；free-form 形式首 token 为脚本路径、其余为参数。
func (m *ScriptModule) Run(rc *RunContext, args map[string]any, free string) *Result {
	src, _ := argStr(args, "src")
	scriptArgs := free
	if src == "" {
		fields := strings.Fields(free)
		if len(fields) == 0 {
			return Fail("script requires a src parameter or a free-form script path")
		}
		src, scriptArgs = fields[0], strings.Join(fields[1:], " ")
	}
	local, lerr := resolveLocal(rc, src)
	if lerr != nil {
		return Fail("%v", lerr)
	}
	data, err := readLocalSrc(rc, local)
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
	if bad != nil {
		return bad
	}
	res := &Result{Stdout: out.Stdout, Stderr: out.Stderr, Rc: out.Code, Changed: true, Msg: src}
	if out.Code != 0 {
		res.Failed = true
		res.Msg = fmt.Sprintf("script exit code rc=%d", out.Code)
	}
	return res
}

func (m *ScriptModule) Params() []ParamDoc {
	return []ParamDoc{
		{Name: "src", Type: "string", Desc: i18n.T("local script path (relative to the chart/playbook)", "本地脚本路径（chart/playbook 内相对路径）")},
		{Name: "(free-form)", Type: "string", Desc: i18n.T("arguments passed to the script, written inline in the module value", "传给脚本的参数，写在模块键本位")},
	}
}

func (m *ScriptModule) Example() string {
	return i18n.T(`- name: upload and run the migration script
  script: scripts/migrate.sh --verbose
`, `- name: 上传并执行迁移脚本
  script: scripts/migrate.sh --verbose
`)
}
