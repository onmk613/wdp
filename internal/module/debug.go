package module

// debug 模块：打印变量或消息（排查 playbook 用，对齐 ansible debug 的
// var/msg 两种形态）。不产生变更；输出进 Result.Msg（-v 可见、report 留档）。

import (
	"fmt"
	"strings"
	"wdp/internal/i18n"

	"gopkg.in/yaml.v3"
)

func init() { Register(&DebugModule{}) }

// DebugModule 打印变量/消息。
type DebugModule struct{}

func (m *DebugModule) Name() string { return "debug" }

func (m *DebugModule) Desc() string {
	return i18n.T("Print a variable or message (playbook debugging)", "打印变量或消息（playbook 调试）")
}

func (m *DebugModule) Params() []ParamDoc {
	return []ParamDoc{
		{Name: "var", Type: "string", Desc: i18n.T("variable name (e.g. a register result; dot paths such as result.stdout are supported): prints its value as YAML", "变量名（如 register 的结果，支持点路径 result.stdout）：以 YAML 打印其值")},
		{Name: "msg", Type: "string", Desc: i18n.T("message text (template variables supported); can be combined with var, msg is printed first", "消息文本（支持模板变量）；可与 var 同用，msg 先输出")},
	}
}

// Run 执行：var 按点路径在主机变量域解析，msg 经模板渲染后原样输出。
// msg/var 显式给了但类型不是字符串时直接失败：静默跳过会落到
// "requires var or msg"，把"类型写错"（如 msg: 42）误导成"参数没给"。
func (m *DebugModule) Run(rc *RunContext, args map[string]any, _ string) *Result {
	res := &Result{Msg: ""}
	if raw := args["msg"]; raw != nil {
		msg, ok := raw.(string)
		if !ok {
			return Fail("debug: msg must be a string, got %T", raw)
		}
		if msg != "" {
			// msg 渲染失败显式失败（fail-loud，对齐 template 的口径）：
			// 调试工具吞错会把模板笔误伪装成原文输出，排查被误导
			rendered, err := rc.engine().Render(msg, rc.Vars)
			if err != nil {
				return Fail("debug: msg rendering failed: %v", err)
			}
			msg = rendered
			res.Msg += msg
		}
	}
	if raw := args["var"]; raw != nil {
		v, ok := raw.(string)
		if !ok {
			return Fail("debug: var must be a string, got %T", raw)
		}
		if v != "" {
			val, found := resolveVarPath(rc.Vars, v)
			if !found {
				return Fail("debug: variable %q not found", v)
			}
			b, err := yaml.Marshal(val)
			if err != nil {
				return Fail("debug: marshal %q: %v", v, err)
			}
			if res.Msg != "" {
				res.Msg += "\n"
			}
			res.Msg += fmt.Sprintf("%s =\n%s", v, string(b))
		}
	}
	if res.Msg == "" {
		return Fail("debug: requires var or msg")
	}
	return res
}

// Example 使用示例（实现 UsageProvider 的另一半：Params+Example 齐备后
// var/msg 才会进 /api/modules，编辑器补全与文档随之可用）。
func (m *DebugModule) Example() string {
	return i18n.T(`- name: Print a register result (dot path)
  debug:
    var: result

- name: Print a rendered message (template variables supported)
  debug:
    msg: 'current host {{ .inventory_hostname }}'
`, `- name: 打印 register 结果（点路径）
  debug:
    var: result

- name: 打印渲染消息（支持模板变量）
  debug:
    msg: '当前主机 {{ .inventory_hostname }}'
`)
}

// resolveVarPath 按点路径解析变量（"result.stdout" → Vars["result"].(map)["stdout"]）。
// 数组下标（a.b[0]）不支持——复杂取值用模板表达式更清晰。
func resolveVarPath(vars map[string]any, path string) (any, bool) {
	var cur any = vars
	for seg := range strings.SplitSeq(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		if cur, ok = m[seg]; !ok {
			return nil, false
		}
	}
	return cur, true
}
