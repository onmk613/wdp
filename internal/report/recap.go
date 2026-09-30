package report

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"wdp/internal/fmtutil"
	"wdp/internal/model"
)

// statCell 统计数值单元格：0 灰显弱化，非 0 按语义着色。
func statCell(v int, clr fmtutil.Color) fmtutil.Cell {
	if v == 0 {
		return fmtutil.CC("0", fmtutil.Dim)
	}
	return fmtutil.CC(strconv.Itoa(v), clr)
}

// statCells 按固定列序（OK/CHANGED/FAILED/UNREACHABLE/SKIPPED/IGNORED/ELAPSED）生成单元格。
func statCells(s *model.Stats) []fmtutil.Cell {
	return []fmtutil.Cell{
		statCell(s.Ok, fmtutil.Green),
		statCell(s.Changed, fmtutil.Yellow),
		statCell(s.Failed, fmtutil.Red),
		statCell(s.Unreachable, fmtutil.Red),
		statCell(s.Skipped, fmtutil.Yellow),
		statCell(s.Ignored, fmtutil.None),
		elapsedCell(s.ElapsedMs),
	}
}

// elapsedCell 耗时单元格：0 灰显，其余青色（与主机名列呼应，弱化数字噪音）。
func elapsedCell(ms int64) fmtutil.Cell {
	if ms == 0 {
		return fmtutil.CC("0", fmtutil.Dim)
	}
	return fmtutil.CC(FormatMs(ms), fmtutil.Cyan)
}

// FormatMs 毫秒时长的人读格式：0 / 350ms / 1.24s / 2m03s / 1h02m03s。
func FormatMs(ms int64) string {
	switch {
	case ms <= 0:
		return "0"
	case ms < 1000:
		return strconv.FormatInt(ms, 10) + "ms"
	case ms < 60*1000:
		return fmt.Sprintf("%.2fs", float64(ms)/1000)
	case ms < 60*60*1000:
		return fmt.Sprintf("%dm%02ds", ms/60000, ms%60000/1000)
	default:
		return fmt.Sprintf("%dh%02dm%02ds", ms/3600000, ms%3600000/60000, ms%60000/1000)
	}
}

// recapHeader RECAP 表头（数字列右对齐）。
var recapHeader = []string{"HOST", "OK", "CHANGED", "FAILED", "UNREACHABLE", "SKIPPED", "IGNORED", "ELAPSED"}

// Recap 输出 play 汇总表格（非 verbose 且主机超过 100 时折叠为 TOTAL 行；
// quiet 保持行式输出；超大主机数自动降级 TSV）。表后附 wall（play 整体
// 墙钟）与最慢主机（按任务耗时之和），是并发/分批性能对比的依据。
func (c *Console) Recap(playName string, stats map[string]*model.Stats, wallMs int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.printf("\n%s %s %s\n",
		c.p.Sprint(fmtutil.BoldCyan, "PLAY RECAP"),
		c.p.Sprint(fmtutil.Bold, "["+playName+"]"),
		c.p.Sprint(fmtutil.Cyan, strings.Repeat("*", 20)))
	names := make([]string, 0, len(stats))
	for n := range stats {
		names = append(names, n)
	}
	slices.Sort(names)

	slowestName, slowestMs := slowestHost(stats)
	if c.Level < 0 {
		for _, n := range names {
			c.printf("%-20s %s\n", n+":", c.recapLine(stats[n]))
		}
		c.printf("%-20s %s\n", "wall:", c.wallLine(wallMs, slowestName, slowestMs))
		return
	}

	tb := c.p.NewTable(recapHeader...).AlignRight(1, 2, 3, 4, 5, 6, 7).PlainRowLimit(taskRowLimit)
	if c.Level < 1 && len(names) > 100 {
		total := &model.Stats{}
		for _, s := range stats {
			total.Ok += s.Ok
			total.Changed += s.Changed
			total.Failed += s.Failed
			total.Unreachable += s.Unreachable
			total.Skipped += s.Skipped
			total.Ignored += s.Ignored
			total.ElapsedMs += s.ElapsedMs
		}
		tb.AddRow(append([]fmtutil.Cell{fmtutil.CC(fmt.Sprintf("TOTAL(%d hosts):", len(names)), fmtutil.Bold)}, statCells(total)...)...)
		tb.Render()
		c.printf("%s\n", c.wallLine(wallMs, slowestName, slowestMs))
		return
	}
	for _, n := range names {
		tb.AddRow(append([]fmtutil.Cell{fmtutil.CC(n, fmtutil.None)}, statCells(stats[n])...)...)
	}
	tb.Render()
	c.printf("%s\n", c.wallLine(wallMs, slowestName, slowestMs))
}

// slowestHost 返回任务耗时之和最大的主机（无统计时名称为空）。
func slowestHost(stats map[string]*model.Stats) (string, int64) {
	name, ms := "", int64(0)
	for n, s := range stats {
		if s.ElapsedMs > ms {
			name, ms = n, s.ElapsedMs
		}
	}
	return name, ms
}

// wallLine 汇总行：wall=12.3s slowest=h3(8.1s)。无主机统计时只报 wall。
func (c *Console) wallLine(wallMs int64, slowestName string, slowestMs int64) string {
	wall := c.p.Sprint(fmtutil.Cyan, "wall="+FormatMs(wallMs))
	if slowestName == "" {
		return wall
	}
	return wall + " " + c.p.Sprint(fmtutil.Yellow,
		fmt.Sprintf("slowest=%s(%s)", slowestName, FormatMs(slowestMs)))
}

// recapLine 行式 RECAP 单主机行（quiet 模式用，保持旧格式，追加耗时）。
func (c *Console) recapLine(s *model.Stats) string {
	return fmt.Sprintf("%s %s %s %s %s %s %s",
		c.p.Sprint(fmtutil.Green, fmt.Sprintf("ok=%d", s.Ok)),
		c.p.Sprint(fmtutil.Yellow, fmt.Sprintf("changed=%d", s.Changed)),
		c.p.Sprint(fmtutil.Red, fmt.Sprintf("failed=%d", s.Failed)),
		c.p.Sprint(fmtutil.Red, fmt.Sprintf("unreachable=%d", s.Unreachable)),
		c.p.Sprint(fmtutil.Yellow, fmt.Sprintf("skipped=%d", s.Skipped)),
		fmt.Sprintf("ignored=%d", s.Ignored),
		c.p.Sprint(fmtutil.Cyan, "elapsed="+FormatMs(s.ElapsedMs)),
	)
}
