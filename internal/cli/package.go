package cli

import (
	"fmt"
	"wdp/internal/chart"
	"wdp/internal/i18n"

	"github.com/spf13/cobra"
)

// packageHelp 返回 `wdp package` 的长帮助（调用时求值）。
func packageHelp() string {
	return i18n.T(`Package a chart directory into <name>-<version>.tgz

The artifact is self-contained and can be distributed as input to run/plan/render/lint/drift
-o sets the output directory (default: current directory)

Examples:
wdp package ./myapp -o dist/
`, `把 chart 目录打包为 <name>-<version>.tgz

产物自包含，可直接作为 run/plan/render/lint/drift 的输入分发
-o 指定输出目录（默认当前目录）

示例：
wdp package ./myapp -o dist/
`)
}

// newPackageCmd 构造 `wdp package`。
func newPackageCmd() *cobra.Command {
	var outDir string
	cmd := &cobra.Command{
		Use:   "package <chart-dir>",
		Short: i18n.T("package a chart into <name>-<version>.tgz", "把 chart 打包为 <name>-<version>.tgz"),
		Long:  packageHelp(),
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			out, err := chart.Package(args[0], outDir)
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), out)
			return nil
		},
	}
	cmd.Flags().StringVarP(&outDir, "out-dir", "o", ".", i18n.T(
		"output directory (name kept distinct from the global --output format flag)",
		"输出目录（命名刻意区别于全局 --output 输出格式标志）"))
	return cmd
}
