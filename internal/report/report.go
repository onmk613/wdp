// Package report 定义执行过程的输出抽象。
// Console 按详细级别工作（-q / 缺省 / -v / -vv / -vvv），着色与表格由
// internal/fmtutil 提供（--no-color / 非终端自动降级为纯文本）：
//   - quiet(-q)：仅异常主机行 + RECAP，行式输出（脚本/管道友好，不做表格）
//   - 缺省：聚合模式（面向大规模主机）：每任务仅以表格呈现异常主机（含
//     loop 异常项与 diff 预演），任务结束一行汇总
//   - -v：逐主机全量表格（ok/skipped 也显示）
//   - -vv：表格 + 逐主机详情块（完整 stdout/stderr 不截断、loop 逐项）
//   - -vvv：调试（stderr 恒显示、委托细节）
//
// 任务结果按任务缓冲、TaskDone 时统一渲染（executor 的 fanOut 在任务
// 全部主机完成后才逐主机回调，缓冲不引入输出延迟）。多行内容（diff、
// stderr、全量输出）不放单元格，而是表格下方按主机分组缩进输出。
//
// 任务级 output 属性（full/none/oneline/head=N/tail=N）只控展示、不控数据，
// 在任何级别下生效并覆盖级别默认。RECAP 汇总在所有模式下输出。
package report

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"

	"wdp/internal/fmtutil"
	"wdp/internal/i18n"
	"wdp/internal/model"
)

// Reporter 接收执行事件并呈现。
type Reporter interface {
	PlayStart(name string, hosts []string)
	TaskStart(task, module string)
	HostResult(host string, r *model.TaskResult)
	TaskDone() // 单任务全部主机结果收齐后调用
	PlayMsg(format string, a ...any)
	Recap(playName string, stats map[string]*model.Stats)
	Finish() // 整个 run 结束时调用（JSON 模式输出最终文档）
}

// Console 是带颜色的控制台输出实现。
type Console struct {
	Out    io.Writer
	UseTTY bool
	Level  int // -1 quiet / 0 聚合 / 1 逐主机 / 2 全量 / 3 调试

	mu sync.Mutex
	p  *fmtutil.Printer

	// 当前任务的聚合计数与结果行缓冲
	curTask  string
	curStats *model.Stats
	curHosts int
	curRows  []taskRow
}

// taskRow 是任务表格的一行：host/status/摘要进表格，
// 多行详情（diff/stderr/全量输出/loop 项）缓存在 blocks，表格后逐主机输出。
type taskRow struct {
	host   string // 含委托标记（"h1 -> bastion"）
	status fmtutil.Cell
	detail fmtutil.Cell
	blocks string
	hasBlk bool
}

// taskRowLimit 任务表格行数上限：超过后降级为 TSV 流式（对齐缓存无界
// 会拖慢大规模主机输出），可配合 `column -t` 还原对齐。
const taskRowLimit = 1000

// detailCellWidth 摘要单元格显示宽度上限，超宽截断加省略号，
// 全量内容进详情块，信息不丢。
const detailCellWidth = 80

// NewConsole 创建控制台 reporter（level 语义见包注释）。
func NewConsole(out io.Writer, tty bool, level int) *Console {
	p := fmtutil.New()
	p.SetWriter(out)
	p.SetColor(tty)
	return &Console{Out: out, UseTTY: tty, Level: level, p: p}
}

func (c *Console) printf(format string, a ...any) {
	c.p.Printf(fmtutil.None, format, a...)
}

// PlayStart 输出 play 标题。
func (c *Console) PlayStart(name string, hosts []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.printf("\n%s %s %s\n",
		c.p.Sprint(fmtutil.BoldCyan, "PLAY"),
		c.p.Sprint(fmtutil.Bold, "["+name+"]"),
		c.p.Sprint(fmtutil.Cyan, strings.Repeat("*", 20)))
	if len(hosts) > 20 && c.Level < 1 {
		c.printf(i18n.T("%s %d hosts (first 10: %v …)\n\n", "%s %d 台（前 10: %v …）\n\n"), c.p.Sprint(fmtutil.Dim, "hosts:"), len(hosts), hosts[:10])
	} else {
		c.printf("%s %v\n\n", c.p.Sprint(fmtutil.Dim, "hosts:"), hosts)
	}
}

// TaskStart 输出任务标题并重置聚合计数。
func (c *Console) TaskStart(task, module string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.printf("%s %s %s\n",
		c.p.Sprint(fmtutil.BoldCyan, "TASK"),
		c.p.Sprint(fmtutil.Bold, "["+task+" ("+module+")]"),
		c.p.Sprint(fmtutil.Cyan, strings.Repeat("*", 20)))
	c.curTask = task
	c.curStats = &model.Stats{}
	c.curHosts = 0
	c.curRows = nil
}

// HostResult 缓冲单主机结果（按级别与任务级 output 裁剪展示），TaskDone 统一渲染。
func (c *Console) HostResult(host string, r *model.TaskResult) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.curStats != nil {
		switch {
		case r.Unreachable:
			c.curStats.Unreachable++
		case r.Failed:
			c.curStats.Failed++
		case r.Skipped:
			c.curStats.Skipped++
		case r.Changed:
			c.curStats.Changed++
		default:
			c.curStats.Ok++
		}
		c.curHosts++
	}

	// 展示门槛：quiet 仅异常；聚合(0)仅异常与带 diff 的预估；>=1 全量
	abnormal := r.Failed || r.Unreachable
	if c.Level < 1 && !abnormal && r.Diff == "" {
		// 聚合模式下 loop 中的异常项仍需可见（以独立行入表）
		for _, it := range r.Items {
			if it.Failed || it.Unreachable {
				c.curRows = append(c.curRows, taskRow{
					host:   host,
					status: fmtutil.CC("fatal", fmtutil.BoldRed),
					detail: fmtutil.C("item=" + it.Item + " " + it.Msg),
				})
			}
		}
		return
	}
	c.curRows = append(c.curRows, c.buildRow(host, r))
}

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

// splitDetail 把展示文本拆为「单元格首行 + 详情块」：首行按显示宽度截断；
// 存在后续行或首行被截断时，全量内容以 4 空格缩进进块，保证信息不丢。
func splitDetail(detail string) (cell, block string) {
	if detail == "" {
		return "", ""
	}
	lines := strings.Split(strings.TrimRight(detail, "\n"), "\n")
	cell = fmtutil.TruncateDisplay(lines[0], detailCellWidth)
	if len(lines) == 1 && cell == lines[0] {
		return cell, ""
	}
	var sb strings.Builder
	for _, l := range lines {
		sb.WriteString("    " + l + "\n")
	}
	return cell, sb.String()
}

// prefixLines 给每行加前缀（stderr 块用）。
func prefixLines(s, prefix string) string {
	var sb strings.Builder
	for _, line := range strings.Split(s, "\n") {
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
	for _, line := range strings.Split(strings.TrimRight(d, "\n"), "\n") {
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

// TaskDone 渲染当前任务的结果表格、详情块与聚合汇总行
// （由 executor 在任务结果收齐后调用）。
func (c *Console) TaskDone() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.curStats == nil && len(c.curRows) == 0 {
		return
	}
	rows, s, hosts := c.curRows, c.curStats, c.curHosts
	c.curTask, c.curStats, c.curRows, c.curHosts = "", nil, nil, 0

	if c.Level < 0 {
		// quiet：行式输出（脚本/管道友好），保持旧格式
		for _, row := range rows {
			c.p.Print(row.status.Color, row.status.Text+": ")
			c.p.Printf(fmtutil.Bold, "[%s]", row.host)
			if row.detail.Text != "" {
				c.p.Print(fmtutil.None, ": "+row.detail.Text)
			}
			c.p.Print(fmtutil.None, "\n")
			if row.blocks != "" {
				c.p.Print(fmtutil.None, row.blocks)
			}
		}
		return
	}

	if len(rows) > 0 {
		tb := c.p.NewTable("HOST", "STATUS", "DETAIL").PlainRowLimit(taskRowLimit)
		for _, row := range rows {
			tb.AddRow(fmtutil.CC(row.host, fmtutil.Bold), row.status, row.detail)
		}
		tb.Render()
		for _, row := range rows {
			if !row.hasBlk {
				continue
			}
			c.p.Printf(fmtutil.Dim, "  %s:\n", row.host)
			c.p.Print(fmtutil.None, row.blocks)
		}
	}
	if s == nil {
		return
	}
	c.printf("%s %s\n\n", c.p.Sprint(fmtutil.Dim, "»"),
		fmt.Sprintf(i18n.T("%d hosts: %s %s %s %s %s", "%d 台: %s %s %s %s %s"),
			hosts,
			c.p.Sprint(fmtutil.Green, fmt.Sprintf("ok=%d", s.Ok)),
			c.p.Sprint(fmtutil.Yellow, fmt.Sprintf("changed=%d", s.Changed)),
			c.p.Sprint(fmtutil.Red, fmt.Sprintf("failed=%d", s.Failed)),
			c.p.Sprint(fmtutil.Red, fmt.Sprintf("unreachable=%d", s.Unreachable)),
			c.p.Sprint(fmtutil.Yellow, fmt.Sprintf("skipped=%d", s.Skipped)),
		))
}

// PlayMsg 输出 play 级消息（quiet 模式仅输出警告/错误类）。
func (c *Console) PlayMsg(format string, a ...any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Level < 0 {
		msg := fmt.Sprintf(format, a...)
		if !strings.Contains(msg, "失败") && !strings.Contains(msg, "警告") && !strings.Contains(msg, "终止") {
			return
		}
	}
	c.printf("%s\n", c.p.Sprint(fmtutil.Yellow, fmt.Sprintf(format, a...)))
}

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
	sort.Strings(names)

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
		tb.AddRow(append([]fmtutil.Cell{fmtutil.CC(fmt.Sprintf(i18n.T("TOTAL(%d hosts):", "TOTAL(%d 台):"), len(names)), fmtutil.Bold)}, statCells(total)...)...)
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

// truncateOut 截断长输出。
func truncateOut(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

// Finish 控制台模式无最终文档。
func (c *Console) Finish() {}
