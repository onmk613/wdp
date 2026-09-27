package module

// debug 模块：打印变量或消息（排查 playbook 用，对齐 ansible debug 的
// var/msg 两种形态）。不产生变更；输出进 Result.Msg（-v 可见、report 留档）。

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

func init() { Register(&DebugModule{}) }

// DebugModule 打印变量/消息。
type DebugModule struct{}

func (m *DebugModule) Name() string { return "debug" }

func (m *DebugModule) Desc() string {
	return "print a variable or a message (playbook debugging)"
}

func (m *DebugModule) Params() []ParamDoc {
	return []ParamDoc{
		{Name: "var", Type: "string", Desc: "variable name (e.g. a register'd result, or result.stdout): prints its value as YAML"},
		{Name: "msg", Type: "string", Desc: "message text (template vars supported); may combine with var, msg prints first"},
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
