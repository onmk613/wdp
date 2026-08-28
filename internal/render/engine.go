package render

import (
	"fmt"
	"maps"
	"strings"
	"text/template"

	"github.com/Masterminds/sprig/v3"
)

// Engine 持有一组共享的命名模板（chart _helpers.tpl 中的 define），
// 同一 chart 内所有渲染（playbook 参数、配置模板）都能引用这些命名模板。
type Engine struct {
	base  *template.Template
	funcs template.FuncMap // 最终函数集（白名单 + 自有，诊断与测试用）
}

var defaultEngine = newEngine()

// DefaultEngine 返回无 helpers 的默认引擎（裸 playbook 使用）。
func DefaultEngine() *Engine { return defaultEngine }

// newEngine 构造引擎骨架（include 以闭包自引用注册，helpers 解析期即可用）。
// 函数集 = sprig 白名单（见 allowlist.go：文档承诺的函数集合，显式排除
// 证书/密钥生成与凭据散列等高危原语）+ wdp 自有函数覆盖：
// 自有函数签名优先（join/split/default 等保持 wdp 既有语义，旧 chart 不破坏）。
// 安全考量：env/expandenv（chart 模板不得读取控制端环境变量——其中可能含
// 各类 *_env 密钥与 SSH 口令）与 getHostByName（DNS 可作隐蔽外传
// 信道）不在白名单内。
func newEngine() *Engine {
	e := &Engine{}
	full := sprig.TxtFuncMap()
	fm := template.FuncMap{}
	for _, name := range sprigAllowlist {
		if fn, ok := full[name]; ok {
			fm[name] = fn
		}
	}
	// 自有函数覆盖同名 sprig 函数
	maps.Copy(fm, funcs)
	e.base = template.New("wdp").
		Funcs(fm).
		Funcs(template.FuncMap{
			"include": func(name string, data any) (string, error) {
				var sb strings.Builder
				if err := e.base.ExecuteTemplate(&sb, name, data); err != nil {
					return "", fmt.Errorf("include %q failed: %w", name, err)
				}
				return sb.String(), nil
			},
		}).
		Option("missingkey=error")
	e.funcs = fm
	return e
}

// NewEngine 创建带 helpers 的引擎。helpers 是若干 {{ define "x" }}…{{ end }} 片段。
func NewEngine(helpers string) (*Engine, error) {
	e := newEngine()
	if helpers != "" {
		if _, err := e.base.Parse(helpers); err != nil {
			return nil, fmt.Errorf("helpers template parse failed: %w", err)
		}
	}
	return e, nil
}

// DefinedNames 返回已注册的命名模板名（调试/lint 用）。
func (e *Engine) DefinedNames() []string {
	if e.base == nil {
		return nil
	}
	var out []string
	for _, t := range e.base.Templates() {
		if t.Name() != "" && t.Name() != "wdp" {
			out = append(out, t.Name())
		}
	}
	return out
}

// funcsForTest 返回引擎最终函数集（仅测试用）。
func (e *Engine) funcsForTest() map[string]any { return e.funcs }

// Render 渲染模板字符串（可引用 helpers 中的命名模板）。
func (e *Engine) Render(tpl string, vars map[string]any) (string, error) {
	if !strings.Contains(tpl, "{{") {
		return tpl, nil
	}
	clone, err := e.base.Clone()
	if err != nil {
		return "", err
	}
	if _, err := clone.New("w").Parse(tpl); err != nil {
		return "", fmt.Errorf("template parse failed %q: %w%s", tpl, err, bareIdentHint(err))
	}
	var sb strings.Builder
	if err := clone.ExecuteTemplate(&sb, "w", vars); err != nil {
		return "", fmt.Errorf("template render failed %q: %w", tpl, err)
	}
	return sb.String(), nil
}

// bareIdentHint 对"裸标识符被当函数调用"的 parse 错误附加修正提示——
// Ansible 习语 {{ var }} 在 Go template 语义下必须带前导点 {{ .var }}。
func bareIdentHint(err error) string {
	if strings.Contains(err.Error(), `function "`) && strings.Contains(err.Error(), "not defined") {
		return ` (variable references need a leading dot: {{ .var.stdout }} — a bare name parses as a function call, see docs/07)`
	}
	return ""
}

// RenderValue 递归渲染任意值中的所有字符串。
func (e *Engine) RenderValue(v any, vars map[string]any) (any, error) {
	switch x := v.(type) {
	case string:
		return e.Render(x, vars)
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			rv, err := e.RenderValue(val, vars)
			if err != nil {
				return nil, err
			}
			out[k] = rv
		}
		return out, nil
	case []any:
		out := make([]any, len(x))
		for i, val := range x {
			rv, err := e.RenderValue(val, vars)
			if err != nil {
				return nil, err
			}
			out[i] = rv
		}
		return out, nil
	default:
		return v, nil
	}
}
