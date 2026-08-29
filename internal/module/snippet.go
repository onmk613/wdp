package module

import (
	"fmt"
	"strings"
)

// Snippet 输出指定内置模块的参数文档与示例任务（`wdp module <名>` 用）。
func Snippet(name string) (string, error) {
	m, ok := Get(name)
	if !ok {
		return "", fmt.Errorf("%s: %q (%s)",
			"unknown module", name, "see the built-in module list")
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "# %s — %s\n", name, m.Desc())
	params := Usage(m)
	if len(params) == 0 {
		fmt.Fprintf(&sb, "%s\n", "(parameter docs pending: module does not implement UsageProvider)")
	} else {
		fmt.Fprintf(&sb, "%s\n", "parameters:")
		for _, p := range params {
			def := p.Default
			if def == "" {
				def = "-"
			}
			fmt.Fprintf(&sb, "  %-14s %-6s %s %-8s %s\n", p.Name, p.Type,
				"default", def, p.Desc)
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
