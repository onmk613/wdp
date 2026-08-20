// Package render 提供基于 Go text/template 的变量渲染能力，
// 替代 Ansible 的 Jinja2。所有含 {{ }} 的字符串在执行期按主机变量域渲染。
package render

import (
	"fmt"
	"strings"
	"text/template"

	"gopkg.in/yaml.v3"
)

// funcs 是 wdp 自有模板函数集（在 sprig 全集之上覆盖注册，
// 因此 join/split/default 等保持 wdp 既有签名，旧 chart 不破坏）。
var funcs = template.FuncMap{
	"default": func(def, v any) any {
		if v == nil {
			return def
		}
		if s, ok := v.(string); ok && s == "" {
			return def
		}
		return v
	},
	"upper":     strings.ToUpper,
	"lower":     strings.ToLower,
	"trim":      strings.TrimSpace,
	"quote":     func(s any) string { return fmt.Sprintf("%q", toString(s)) },
	"b64enc":    func(s any) string { return b64encode(toString(s)) },
	"b64dec":    func(s any) string { return b64decode(toString(s)) },
	"split":     func(sep, s string) []string { return strings.Split(s, sep) },
	"join":      strings.Join,
	"replace":   func(old, new, s string) string { return strings.ReplaceAll(s, old, new) },
	"contains":  func(substr, s string) bool { return strings.Contains(s, substr) },
	"hasPrefix": func(prefix, s string) bool { return strings.HasPrefix(s, prefix) },
	"hasSuffix": func(suffix, s string) bool { return strings.HasSuffix(s, suffix) },
	"to_json":   toJSON,
	// Helm 同款的 yaml 互转（sprig 未提供，wdp 自带）
	"to_yaml": func(v any) string {
		b, err := yaml.Marshal(v)
		if err != nil {
			return ""
		}
		return string(b)
	},
	"from_yaml": func(s string) map[string]any {
		var m map[string]any
		if err := yaml.Unmarshal([]byte(s), &m); err != nil {
			return map[string]any{}
		}
		return m
	},
}

// Render 用变量域渲染模板字符串（默认引擎，无 helpers）。未定义变量视为错误（尽早暴露拼写问题）。
func Render(tpl string, vars map[string]any) (string, error) {
	return defaultEngine.Render(tpl, vars)
}

// MustRender 渲染失败时 panic（仅用于内部确定无误的模板）。
func MustRender(tpl string, vars map[string]any) string {
	s, err := Render(tpl, vars)
	if err != nil {
		panic(err)
	}
	return s
}

// RenderValue 递归渲染任意值中的所有字符串（map / slice / string）。
func RenderValue(v any, vars map[string]any) (any, error) {
	return defaultEngine.RenderValue(v, vars)
}

// Truthy 判断渲染结果是否为真：空 / false / 0 / no 视为假。
func Truthy(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "false", "0", "no", "off", "[]", "{}", "nil", "<nil>":
		return false
	}
	return true
}
