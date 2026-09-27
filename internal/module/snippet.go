package module

import (
	"fmt"
	"strings"
)

// Snippet 输出指定内置模块的参数文档与示例任务（`wdp module <名>` 用）。
func Snippet(name string) (string, error) {
	m, ok := Get(name)
	if !ok {
		return "", fmt.Errorf("unknown module: %q (see the built-in module list)", name)
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "# %s — %s\n", name, m.Desc())
	params := Usage(m)
	if len(params) == 0 {
		sb.WriteString("(parameter docs pending: module does not implement UsageProvider)\n")
	} else {
		sb.WriteString("parameters:\n")
		for _, p := range params {
			def := p.Default
			if def == "" {
				def = "-"
			}
			// 值域前置进说明列（与各模块解析白名单同源的 Enum 字段渲染，
			// Desc 本身不再手写值域列举）。"|" 分隔：候选值可能自身含 "/"
			// （如 user.shell 的登录 shell 路径）
			desc := p.Desc
			if len(p.Enum) > 0 {
				desc = strings.Join(p.Enum, "|") + " — " + desc
			}
			fmt.Fprintf(&sb, "  %-14s %-6s %s %-8s %s\n", p.Name, p.Type,
				"default", def, desc)
		}
	}
	if ex := Example(m); ex != "" {
		fmt.Fprintf(&sb, "%s\n%s", "example task:", ex)
		if !strings.HasSuffix(ex, "\n") {
			sb.WriteString("\n")
		}
	}
	return sb.String(), nil
}
