package report

import (
	"fmt"
	"slices"
	"strings"

	"wdp/internal/fmtutil"
	"wdp/internal/model"
)

// statCell 统计数值单元格：0 灰显弱化，非 0 按语义着色。
func statCell(v int, clr fmtutil.Color) fmtutil.Cell {
	if v == 0 {
		return fmtutil.CC("0", fmtutil.Dim)
	}
	return fmtutil.CC(fmt.Sprintf("%d", v), clr)
}

// statCells 按固定列序（OK/CHANGED/FAILED/UNREACHABLE/SKIPPED/IGNORED）生成单元格。
func statCells(s *model.Stats) []fmtutil.Cell {
	return []fmtutil.Cell{
		statCell(s.Ok, fmtutil.Green),
		statCell(s.Changed, fmtutil.Yellow),
		statCell(s.Failed, fmtutil.Red),
		statCell(s.Unreachable, fmtutil.Red),
		statCell(s.Skipped, fmtutil.Yellow),
		statCell(s.Ignored, fmtutil.None),
	}
}

// recapHeader RECAP 表头（数字列右对齐）。
var recapHeader = []string{"HOST", "OK", "CHANGED", "FAILED", "UNREACHABLE", "SKIPPED", "IGNORED"}

// Recap 输出 play 汇总表格（非 verbose 且主机超过 100 时折叠为 TOTAL 行；
// quiet 保持行式输出；超大主机数自动降级 TSV）。
func (c *Console) Recap(playName string, stats map[string]*model.Stats) {
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

	if c.Level < 0 {
		for _, n := range names {
			c.printf("%-20s %s\n", n+":", c.recapLine(stats[n]))
		}
		return
	}

	tb := c.p.NewTable(recapHeader...).AlignRight(1, 2, 3, 4, 5, 6).PlainRowLimit(taskRowLimit)
	if c.Level < 1 && len(names) > 100 {
		total := &model.Stats{}
		for _, s := range stats {
			total.Ok += s.Ok
			total.Changed += s.Changed
			total.Failed += s.Failed
			total.Unreachable += s.Unreachable
			total.Skipped += s.Skipped
			total.Ignored += s.Ignored
		}
		tb.AddRow(append([]fmtutil.Cell{fmtutil.CC(fmt.Sprintf("TOTAL(%d hosts):", len(names)), fmtutil.Bold)}, statCells(total)...)...)
		tb.Render()
		return
	}
	for _, n := range names {
		tb.AddRow(append([]fmtutil.Cell{fmtutil.C(n)}, statCells(stats[n])...)...)
	}
	tb.Render()
}

// recapLine 行式 RECAP 单主机行（quiet 模式用，保持旧格式）。
func (c *Console) recapLine(s *model.Stats) string {
	return fmt.Sprintf("%s %s %s %s %s %s",
		c.p.Sprint(fmtutil.Green, fmt.Sprintf("ok=%d", s.Ok)),
		c.p.Sprint(fmtutil.Yellow, fmt.Sprintf("changed=%d", s.Changed)),
		c.p.Sprint(fmtutil.Red, fmt.Sprintf("failed=%d", s.Failed)),
		c.p.Sprint(fmtutil.Red, fmt.Sprintf("unreachable=%d", s.Unreachable)),
		c.p.Sprint(fmtutil.Yellow, fmt.Sprintf("skipped=%d", s.Skipped)),
		fmt.Sprintf("ignored=%d", s.Ignored),
	)
}
