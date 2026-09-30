package module

import (
	"fmt"
	"strings"

	"wdp/internal/fmtutil"
	"wdp/internal/i18n"
)

// Snippet 输出指定内置模块的参数文档与示例任务（`wdp module <名>` 用）。
func Snippet(name string) (string, error) {
	m, ok := Get(name)
	if !ok {
		return "", fmt.Errorf("%s %q (%s)",
			i18n.T("unknown module", "未知模块"), name,
			i18n.T("see `wdp module` for the list of built-in modules", "内置模块清单见 `wdp module`"))
	}
	var sb strings.Builder
	RenderDoc(&sb, name, m.Desc(), Usage(m), Example(m))
	return sb.String(), nil
}

// RenderDoc 渲染「名称 — 描述 + 参数表 + 示例任务」文档片段：Snippet 与
// playbook.ChartRefDoc（chart 伪模块）共用同一实现，杜绝两份格式漂移。
// 参数表按终端显示宽度对齐（fmtutil.DisplayWidth，中文说明不错位），
// Enum 值域前置进说明列（"|" 分隔：候选值可能自身含 "/"，如登录 shell
// 路径），说明文案由各模块 ParamDesc 提供。
func RenderDoc(sb *strings.Builder, name, desc string, params []ParamDoc, example string) {
	fmt.Fprintf(sb, "%s — %s\n", name, desc)
	if len(params) == 0 {
		sb.WriteString(i18n.T(
			"\n(parameter docs pending: this module does not implement UsageProvider)\n",
			"\n（参数文档待补：模块未实现 UsageProvider）\n"))
	} else {
		type row struct{ name, typ, def, desc string }
		rows := make([]row, 0, len(params)+1)
		rows = append(rows, row{
			i18n.T("parameter", "参数"), i18n.T("type", "类型"),
			i18n.T("default", "默认"), i18n.T("description", "说明"),
		})
		for _, p := range params {
			def, typ := p.Default, p.Type
			if def == "" {
				def = "-"
			}
			if typ == "" {
				typ = "-"
			}
			d := p.Desc
			if len(p.Enum) > 0 {
				d = strings.Join(p.Enum, "|") + " — " + d
			}
			rows = append(rows, row{p.Name, typ, def, d})
		}
		var w [3]int
		for _, r := range rows {
			w[0] = max(w[0], fmtutil.DisplayWidth(r.name))
			w[1] = max(w[1], fmtutil.DisplayWidth(r.typ))
			w[2] = max(w[2], fmtutil.DisplayWidth(r.def))
		}
		sb.WriteString("\n")
		for _, r := range rows {
			fmt.Fprintf(sb, "  %s  %s  %s  %s\n",
				padTo(r.name, w[0]), padTo(r.typ, w[1]), padTo(r.def, w[2]), r.desc)
		}
	}
	if example != "" {
		sb.WriteString("\n" + i18n.T("example task", "示例任务") + "\n")
		sb.WriteString(example)
		if !strings.HasSuffix(example, "\n") {
			sb.WriteString("\n")
		}
	}
}

// padTo 按终端显示宽度右补空格（不足宽度时至少留两格分隔）。
func padTo(s string, w int) string {
	if n := w - fmtutil.DisplayWidth(s); n > 0 {
		return s + strings.Repeat(" ", n)
	}
	return s
}
