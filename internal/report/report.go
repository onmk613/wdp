// Package report 定义执行过程的输出抽象。
// Console 按详细级别工作（-q / 缺省 / -v / -vv / -vvv），着色与表格由
// internal/fmtutil 提供（--no-color / 非终端自动降级为纯文本）：
//   - quiet(-q)：仅异常主机行 + RECAP，行式输出（脚本/管道友好，不做表格）
//   - 缺省：聚合模式（面向大规模主机）：每任务仅以表格呈现异常主机（含
//     loop 异常项与 diff 预演），任务结束一行汇总，汇总行下列出每主机
//     状态清单（≤20 台；异常主机附错误信息首行，超阈值退化为纯计数）
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
	"io"
	"sync"

	"wdp/internal/fmtutil"
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
	curEach  []hostEach // 每主机状态（计数行下的清单用，不随展示门槛裁剪）
}

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

// Finish 控制台模式无最终文档。
func (c *Console) Finish() {}
