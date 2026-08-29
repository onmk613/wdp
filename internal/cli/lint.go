package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"wdp/internal/chart"
	"wdp/internal/config"
	"wdp/internal/playbook"
)

// newLintCmd 构造 `wdp lint`。
func newLintCmd() *cobra.Command {
	var valuesFiles, setArgs []string
	cmd := &cobra.Command{
		Use:   "lint <chart-dir|tgz|playbook.yaml>",
		Short: "statically validate a chart or a bare playbook",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			// 裸 playbook：只做任务树静态检查（模块名/结构/chart 引用拒绝）
			if strings.HasSuffix(args[0], ".yaml") || strings.HasSuffix(args[0], ".yml") {
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
