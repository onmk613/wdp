package report

import (
	"strings"

	"wdp/internal/fmtutil"
	"wdp/internal/model"
)

// detailCellWidth 摘要单元格显示宽度上限，超宽截断加省略号，
// 全量内容进详情块，信息不丢。
const detailCellWidth = 80

// statusCell 结果状态 → 表格单元格（文本 + 语义色）。
func statusCell(r *model.TaskResult) fmtutil.Cell {
	switch {
	case r.Unreachable:
		return fmtutil.CC("UNREACHABLE", fmtutil.BoldRed)
	case r.Failed:
		return fmtutil.CC("fatal", fmtutil.BoldRed)
	case r.Skipped:
		return fmtutil.CC("skipping", fmtutil.BoldYellow)
	case r.Changed:
		return fmtutil.CC("changed", fmtutil.BoldYellow)
	default:
		return fmtutil.CC("ok", fmtutil.BoldGreen)
	}
}

// buildRow 构造单主机结果行：摘要进单元格，多行内容进详情块。
func (c *Console) buildRow(host string, r *model.TaskResult) taskRow {
	row := taskRow{host: host, status: statusCell(r)}
	if r.DelegateTo != "" {
		row.host = host + " -> " + r.DelegateTo
	}

	spec := r.Output
	if spec == "none" {
		row.detail = fmtutil.CC("[output=none]", fmtutil.Dim)
		return row
	}

	detail := r.Msg
	if detail == "" && r.Stdout != "" {
		limit := 300
		if c.Level >= 2 || spec == "full" {
			limit = 1 << 20
		}
		detail = truncateOut(model.ApplyOutputSpec(spec, strings.TrimSpace(r.Stdout)), limit)
	} else if detail != "" {
		detail = model.ApplyOutputSpec(spec, detail)
	}
	if detail == "" && r.Diff != "" {
		// 无消息有 diff 时以首行作摘要，全量差异在详情块
		detail = strings.SplitN(strings.TrimRight(r.Diff, "\n"), "\n", 2)[0]
	}

	cell, block := splitDetail(detail)
	row.detail = fmtutil.C(cell)
	var blocks []string
	if block != "" {
		blocks = append(blocks, block)
	}
	// diff 展示同样遵循任务级 output 控制（output=none / no_log 时隐藏，docs/11）
	if r.Diff != "" {
		blocks = append(blocks, c.renderDiff(r.Diff))
	}
	if r.Stderr != "" && (r.Failed || c.Level >= 3) {
		blocks = append(blocks, prefixLines(strings.TrimRight(model.ApplyOutputSpec(spec, r.Stderr), "\n"), "    stderr | "))
	}
	if b := c.itemsBlock(r); b != "" {
		blocks = append(blocks, b)
	}
	if len(blocks) > 0 {
		row.blocks, row.hasBlk = strings.Join(blocks, ""), true
	}
	return row
}

// splitDetail 把展示文本拆为「单元格首行 + 详情块」。展示内容只出现一次：
//   - 单行且放得下 → 只进单元格
//   - 单行但超列宽 → 单元格不重复展示截断文本，全量进详情块
//   - 多行 → 首行进单元格，全量（含首行）进详情块
func splitDetail(detail string) (cell, block string) {
	if detail == "" {
		return "", ""
	}
	lines := strings.Split(strings.TrimRight(detail, "\n"), "\n")
	truncated := fmtutil.TruncateDisplay(lines[0], detailCellWidth)
	if len(lines) == 1 {
		if truncated == lines[0] {
			return truncated, ""
		}
		return "", "    " + lines[0] + "\n"
	}
	var sb strings.Builder
	for _, l := range lines {
		sb.WriteString("    " + l + "\n")
	}
	return truncated, sb.String()
}

// prefixLines 给每行加前缀（stderr 块用）。
func prefixLines(s, prefix string) string {
	var sb strings.Builder
	for line := range strings.SplitSeq(s, "\n") {
		sb.WriteString(prefix + line + "\n")
	}
	return sb.String()
}

// itemsBlock 输出 loop 逐项结果（-vv 起全量显示）。
func (c *Console) itemsBlock(r *model.TaskResult) string {
	if c.Level < 2 || len(r.Items) == 0 {
		return ""
	}
	var sb strings.Builder
	for _, it := range r.Items {
		st := statusCell(it)
		detail := it.Msg
		if detail == "" && it.Stdout != "" {
			detail = truncateOut(strings.TrimSpace(it.Stdout), 300)
		}
		if detail != "" {
			detail = ": " + detail
		}
		sb.WriteString("    item=" + it.Item + " " + c.p.Sprint(st.Color, st.Text) + detail + "\n")
	}
	return sb.String()
}

// renderDiff 渲染内容级差异（+绿 -红 @@青）。
func (c *Console) renderDiff(d string) string {
	var sb strings.Builder
	for line := range strings.SplitSeq(strings.TrimRight(d, "\n"), "\n") {
		clr := fmtutil.None
		switch {
		case strings.HasPrefix(line, "+"):
			clr = fmtutil.Green
		case strings.HasPrefix(line, "-"):
			clr = fmtutil.Red
		case strings.HasPrefix(line, "@@"):
			clr = fmtutil.Cyan
		}
		sb.WriteString("    " + c.p.Sprint(clr, line) + "\n")
	}
	return sb.String()
}
