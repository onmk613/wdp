package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"wdp/internal/chart"
	"wdp/internal/config"
	"wdp/internal/playbook"
)

const lintHelp = `
chart 或裸 playbook 静态校验（不连主机）

裸 playbook（参数以 .yaml/.yml 结尾）：任务树结构、模块名、chart 引用合法性
chart（目录/tgz）：全相位任务树校验（含子 chart 引用、block 组递归、模块名）、
模板字段 parse-only 拦截裸变量引用、phases 声明与相位文件匹配（防拼写错误
静默失效）、模板文件可渲染（样例域）、envs 文件可解析且过 schema、
合并 values 过 values.schema.json（子 chart 逐层静态走查）、
inventory_override 白名单键存在性
-f/--set 传入实际 values 可校验真实合并结果
存在 ERROR 时非零退出（可直接进 CI），WARN 仅提示

示例：
wdp lint ./myapp
wdp lint ./myapp -f envs/prod.yaml
wdp lint site.yaml
`

// newLintCmd 构造 `wdp lint`。
func newLintCmd() *cobra.Command {
	var valuesFiles, setArgs []string
	cmd := &cobra.Command{
		Use:   "lint <chart-dir|tgz|playbook.yaml>",
		Short: "statically validate a chart or a bare playbook",
		Long:  lintHelp,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			// 裸 playbook 只做任务树静态检查（模块名/结构/chart 引用拒绝）。
			// 判定与 run 同口径（chart.IsChartPath：目录或 .tgz 后缀），
			// 避免"lint 按后缀当 playbook、run 按目录当 chart"两条规则漂移。
			if !chart.IsChartPath(args[0]) {
				errCount := 0
				for _, is := range playbook.Lint(args[0]) {
					fmt.Fprintf(out, "[%s] %s: %s\n", is.Level, is.Path, is.Msg)
					if is.Level == "ERROR" {
						errCount++
					}
				}
				if errCount > 0 {
					return fmt.Errorf("playbook %s: %d error(s)", args[0], errCount)
				}
				fmt.Fprintf(out, "playbook %s: validation passed\n", args[0])
				return nil
			}

			ch, values, _, err := chart.OpenWithLimits(args[0], valuesFiles, setArgs, chart.Limits{MaxExtractBytes: config.Current().MaxExtractBytes()})
			if err != nil {
				return err
			}
			defer ch.Close()

			errCount := 0
			for _, is := range chart.Lint(ch, values) {
				fmt.Fprintln(out, is)
				if is.Level == chart.ERROR {
					errCount++
				}
			}
			if errCount > 0 {
				return fmt.Errorf("chart %s: %d error(s)", ch.Meta.Name, errCount)
			}
			fmt.Fprintf(out, "%s %s %s: %s\n", "chart", ch.Meta.Name, ch.Meta.Version,
				"validation passed")
			return nil
		},
	}
	chartValueFlags(cmd, &valuesFiles, &setArgs)
	return cmd
}
