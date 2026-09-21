package cli

import (
	"os"

	"github.com/spf13/cobra"

	"wdp/internal/config"
	"wdp/internal/fmtutil"
	"wdp/internal/report"
)

// buildReporter 按全局 --output 构造 reporter；json 模式返回最终文档输出函数。
func buildReporter() (report.Reporter, func()) {
	if gOutput == "json" {
		j := report.NewJSONReporter(os.Stdout)
		return j, j.Finish
	}
	level := gVerbosity
	if gQuiet {
		level = -1
	}
	rep := report.NewConsole(os.Stdout, config.Current().Color() && fmtutil.ColorAuto(os.Stdout), level)
	return rep, func() {}
}

// outPrinter 返回绑定命令输出流的着色 printer（颜色遵循 --no-color、
// 终端检测与 NO_COLOR 约定），供列表类命令渲染 fmtutil 表格。
func outPrinter(cmd *cobra.Command) *fmtutil.Printer {
	p := fmtutil.New()
	p.SetWriter(cmd.OutOrStdout())
	if !config.Current().Color() {
		p.SetColor(false)
	}
	// 未显式 --no-color 时保持自动模式（终端检测 + NO_COLOR）
	return p
}
