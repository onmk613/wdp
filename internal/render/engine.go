package render

import (
	"fmt"
	"strings"
	"text/template"

	"github.com/Masterminds/sprig/v3"
)

// Engine 持有一组共享的命名模板（chart _helpers.tpl 中的 define），
// 同一 chart 内所有渲染（playbook 参数、配置模板）都能引用这些命名模板。
type Engine struct {
	base *template.Template
}

var defaultEngine = newEngine()

// DefaultEngine 返回无 helpers 的默认引擎（裸 playbook 使用）。
func DefaultEngine() *Engine { return defaultEngine }

// newEngine 构造引擎骨架（include 以闭包自引用注册，helpers 解析期即可用）。
// 函数集 = sprig 全集（Helm 同款成熟库）打底 + wdp 自有函数覆盖：
// 自有函数签名优先（join/split/default 等保持 wdp 既有语义，旧 chart 不破坏）。
// 安全考量：与 Helm 一致地移除 env/expandenv（chart 模板不得读取控制端
// 环境变量——其中可能含 WDP_CA_PASSPHRASE 与各类 *_env 密钥）以及
// getHostByName（DNS 查询可被用作隐蔽外传信道）。
func newEngine() *Engine {
	e := &Engine{}
	fm := sprig.TxtFuncMap()
	delete(fm, "env")
	delete(fm, "expandenv")
	delete(fm, "getHostByName")
	e.base = template.New("wdp").
		Funcs(fm).
		Funcs(funcs).
		Funcs(template.FuncMap{
			"include": func(name string, data any) (string, error) {
				var sb strings.Builder
				if err := e.base.ExecuteTemplate(&sb, name, data); err != nil {
					return "", fmt.Errorf("include %q 失败: %w", name, err)
				}
				return sb.String(), nil
			},
		}).
		Option("missingkey=error")
	return e
}

// NewEngine 创建带 helpers 的引擎。helpers 是若干 {{ define "x" }}…{{ end }} 片段。
func NewEngine(helpers string) (*Engine, error) {
	e := newEngine()
	if helpers != "" {
		if _, err := e.base.Parse(helpers); err != nil {
			return nil, fmt.Errorf("helpers 模板解析失败: %w", err)
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
		return "", fmt.Errorf("模板解析失败 %q: %w", tpl, err)
	}
	var sb strings.Builder
	if err := clone.ExecuteTemplate(&sb, "w", vars); err != nil {
		return "", fmt.Errorf("模板渲染失败 %q: %w", tpl, err)
	}
	return sb.String(), nil
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
