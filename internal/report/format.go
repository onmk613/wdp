package report

import (
	"fmt"
	"io"
	"sync"

	"wdp/internal/i18n"
	"wdp/internal/model"
	"wdp/internal/render"
)

// Formatter 是逐主机格式化 reporter：每主机一行，
// 按 Go 模板渲染结果域（.host .stdout .stderr .rc .changed .failed .msg），
// 不输出任务标题与 RECAP，适配 shell 管道。
type Formatter struct {
	Out    io.Writer
	Format string
	engine *render.Engine

	mu sync.Mutex
}

// NewFormatter 创建格式化 reporter。
func NewFormatter(out io.Writer, format string) *Formatter {
	return &Formatter{Out: out, Format: format, engine: render.DefaultEngine()}
}

func (f *Formatter) PlayStart(name string, hosts []string) {}
func (f *Formatter) TaskStart(task, module string)         {}
func (f *Formatter) TaskDone()                             {}
func (f *Formatter) PlayMsg(format string, a ...any)       {}

// Recap 静默（脚本消费 stdout，统计走 stderr 由调用方按需重定向）。
func (f *Formatter) Recap(playName string, stats map[string]*model.Stats) {}

// HostResult 渲染单主机结果。
func (f *Formatter) HostResult(host string, r *model.TaskResult) {
	f.mu.Lock()
	defer f.mu.Unlock()
	vars := map[string]any{
		"host":        host,
		"stdout":      r.Stdout,
		"stderr":      r.Stderr,
		"rc":          r.Rc,
		"changed":     r.Changed,
		"failed":      r.Failed,
		"skipped":     r.Skipped,
		"unreachable": r.Unreachable,
		"msg":         r.Msg,
		"elapsed_ms":  r.ElapsedMs,
	}
	line, err := f.engine.Render(f.Format, vars)
	if err != nil {
		fmt.Fprintf(f.Out, i18n.T("[format render failed: %v]\n", "[format 渲染失败: %v]\n"), err)
		return
	}
	fmt.Fprint(f.Out, line)
	if len(line) == 0 || line[len(line)-1] != '\n' {
		fmt.Fprintln(f.Out)
	}
}

// Finish 无最终文档。
func (f *Formatter) Finish() {}
