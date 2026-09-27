package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"wdp/internal/config"
	"wdp/internal/fmtutil"
	"wdp/internal/report"
)

// buildReporter 按全局 --output 构造 reporter；返回的 finish 在整个 run
// 结束时调用一次（JSON 模式输出最终文档），返回写错误——stdout 破管道
// （`wdp run --output json | head`）时 CI 会拿到 0 字节文档，必须让命令
// 以非零码退出而不是把"报告丢了"当成功。
func buildReporter() (report.Reporter, func() error) {
	if gOutput == "json" {
		j := report.NewJSONReporter(os.Stdout)
		return j, func() error {
			j.Finish() // 签名受 Reporter 接口约束，写错误经 Err() 取回
			if err := j.Err(); err != nil {
				return fmt.Errorf("failed to write the JSON report: %w", err)
			}
			return nil
		}
	}
	level := gVerbosity
	if gQuiet {
		level = -1
	}
	rep := report.NewConsole(os.Stdout, config.Current().Color() && fmtutil.ColorAuto(os.Stdout), level)
	return rep, func() error { return nil }
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
