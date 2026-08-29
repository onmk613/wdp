package report

import (
	"fmt"
	"strings"

	"wdp/internal/fmtutil"
	"wdp/internal/model"
)

// hostEach 是任务级每主机状态记录（缺省级别计数行下的主机清单）。
type hostEach struct {
	host string
	cell fmtutil.Cell // 状态（含语义色）
	msg  string       // 异常时的错误信息首行
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
	c.curEach = nil
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

	// 每主机状态不随展示门槛裁剪：缺省级别计数行下需要完整主机清单
	e := hostEach{host: host, cell: statusCell(r)}
	if r.DelegateTo != "" {
		e.host = host + " -> " + r.DelegateTo
	}
	if r.Failed || r.Unreachable {
		e.msg = firstLine(r.Msg)
		if e.msg == "" {
			e.msg = firstLine(r.Stderr)
		}
		if e.msg == "" {
			e.msg = firstLine(r.Stdout)
		}
	}
	c.curEach = append(c.curEach, e)

	// 展示门槛：quiet 仅异常；聚合(0)仅异常与带 diff 的预估；>=1 全量。
	// debug 模块例外：其 Msg 就是产出物（巡检/汇报场景），聚合模式也放行
	abnormal := r.Failed || r.Unreachable
	hide := c.Level < 1 && !abnormal && r.Diff == ""
	if hide && c.Level >= 0 && r.Module == "debug" {
		hide = false
	}
	if hide {
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

// TaskDone 渲染当前任务的结果表格、详情块与聚合汇总行
// （由 executor 在任务结果收齐后调用）。
func (c *Console) TaskDone() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.curStats == nil && len(c.curRows) == 0 {
		return
	}
	rows, s, hosts, each := c.curRows, c.curStats, c.curHosts, c.curEach
	c.curTask, c.curStats, c.curRows, c.curHosts, c.curEach = "", nil, nil, 0, nil

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
	c.printf("%s %s\n", c.p.Sprint(fmtutil.Dim, "»"),
		fmt.Sprintf("%d hosts: %s %s %s %s %s",
			hosts,
			c.p.Sprint(fmtutil.Green, fmt.Sprintf("ok=%d", s.Ok)),
			c.p.Sprint(fmtutil.Yellow, fmt.Sprintf("changed=%d", s.Changed)),
			c.p.Sprint(fmtutil.Red, fmt.Sprintf("failed=%d", s.Failed)),
			c.p.Sprint(fmtutil.Red, fmt.Sprintf("unreachable=%d", s.Unreachable)),
			c.p.Sprint(fmtutil.Yellow, fmt.Sprintf("skipped=%d", s.Skipped)),
		))
	// 缺省聚合模式：计数行下列出每主机状态（异常主机附错误信息首行）。
	// -v 起上方表格已逐主机呈现；主机数超过 PlayStart 的折叠阈值（20）时
	// 保持纯计数聚合，避免大规模主机的任务输出退化为逐主机清单。
	if c.Level == 0 && len(each) <= 20 {
		for _, e := range each {
			if e.msg != "" {
				c.printf("    %-20s %s: %s\n", e.host, c.p.Sprint(e.cell.Color, e.cell.Text), e.msg)
			} else {
				c.printf("    %-20s %s\n", e.host, c.p.Sprint(e.cell.Color, e.cell.Text))
			}
		}
	}
	c.printf("\n")
}
