package cli

import (
	"fmt"
	"wdp/internal/chart"

	"github.com/spf13/cobra"
)

// newPackageCmd 构造 `wdp package`。
func newPackageCmd() *cobra.Command {
	var outDir string
	cmd := &cobra.Command{
		Use:   "package <chart-dir>",
		Short: "package a chart into <name>-<version>.tgz",
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
	cmd.Flags().StringVarP(&outDir, "out-dir", "o", ".", "output directory (name kept distinct from the global --output format flag)")
	return cmd
}
