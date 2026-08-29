package report

import (
	"fmt"
	"strings"

	"wdp/internal/fmtutil"
)

// PlayStart 输出 play 标题。
func (c *Console) PlayStart(name string, hosts []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.printf("\n%s %s %s\n",
		c.p.Sprint(fmtutil.BoldCyan, "PLAY"),
		c.p.Sprint(fmtutil.Bold, "["+name+"]"),
		c.p.Sprint(fmtutil.Cyan, strings.Repeat("*", 20)))
	if len(hosts) > 20 && c.Level < 1 {
		c.printf("%s %d hosts (first 10: %v …)\n\n", c.p.Sprint(fmtutil.Dim, "hosts:"), len(hosts), hosts[:10])
	} else {
		c.printf("%s %v\n\n", c.p.Sprint(fmtutil.Dim, "hosts:"), hosts)
	}
}

// PlayMsg 输出 play 级消息（quiet 模式仅输出警告/错误类）。
func (c *Console) PlayMsg(format string, a ...any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Level < 0 {
		// quiet 模式只放行警告/错误类消息：按小写关键字粗筛，
		// 与各调用方的英文文案（fail/warn/terminat/abort/cancel）对齐
		msg := strings.ToLower(fmt.Sprintf(format, a...))
		keep := false
		for _, kw := range [...]string{"fail", "warn", "terminat", "abort", "cancel"} {
			if strings.Contains(msg, kw) {
				keep = true
				break
			}
		}
		if !keep {
			return
		}
	}
	c.printf("%s\n", c.p.Sprint(fmtutil.Yellow, fmt.Sprintf(format, a...)))
}
