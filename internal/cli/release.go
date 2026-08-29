package cli

// 运维与记录命令组（--help 的 Operations & Records Commands）：
// release（部署记录）与 modules（内置模块文档）。

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"wdp/internal/fmtutil"
	"wdp/internal/release"
)

// newReleaseCmd 构造 `wdp release`（部署记录）。
func newReleaseCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "release",
		Short: "deployment record viewing (list / show)",
	}
	cmd.AddCommand(newReleaseListCmd(), newReleaseShowCmd(), newReleaseDiffCmd())
	return cmd
}

// newReleaseListCmd 构造 `wdp release list`。
func newReleaseListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list [chart-name prefix]",
		Short: "list deployment records (newest first)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			filter := ""
			if len(args) > 0 {
				filter = args[0]
			}
			recs, err := release.List(filter)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if len(recs) == 0 {
				fmt.Fprintln(out, "(no records)")
				return nil
			}
			tb := outPrinter(cmd).NewTable("ID", "VERSION", "RESULT", "TIME")
			for _, r := range recs {
				if r.Failed {
					tb.AddRow(fmtutil.C(r.ID), fmtutil.C(r.Version),
						fmtutil.CC("failed", fmtutil.Red), fmtutil.C(r.Time.Format("2006-01-02 15:04:05")))
				} else {
					tb.AddRow(fmtutil.C(r.ID), fmtutil.C(r.Version),
						fmtutil.CC("ok", fmtutil.Green), fmtutil.C(r.Time.Format("2006-01-02 15:04:05")))
				}
			}
			tb.Render()
			return nil
		},
	}
}

// newReleaseShowCmd 构造 `wdp release show`。
func newReleaseShowCmd() *cobra.Command {
	var asValues bool
	cmd := &cobra.Command{
		Use:   "show <id>",
		Short: "show record details (--values prints the values snapshot YAML, directly replayable with -f)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			rec, err := release.Load(args[0])
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if asValues {
				if rec.Values == nil {
					return fmt.Errorf("this record has no values (bare playbook mode)")
				}
				return printYAML(out, rec.Values)
			}
			fmt.Fprintf(out, "ID:     %s\nTime:  %s\nChart:  %s %s\nResult:  %s\nHosts:  %v\n",
				rec.ID, rec.Time.Format("2006-01-02 15:04:05"), rec.Chart, rec.Version,
				boolLabel(rec.Failed), rec.Hosts)
			if len(rec.ValuesRef) > 0 {
				fmt.Fprintf(out, "Args:  %v\n", rec.ValuesRef)
			}
			if rec.Values != nil {
				fmt.Fprintln(out, "\nvalues snapshot (--values for the full content):")
				if err := printYAML(out, rec.Values); err != nil {
					return err
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asValues, "values", false,
		"print only the values snapshot (YAML)")
	return cmd
}

// newReleaseDiffCmd 构造 `wdp release diff`。
func newReleaseDiffCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "diff <id1> <id2>",
		Short: "diff the values snapshots of two deployment records (answers which params change before an upgrade)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := release.Load(args[0])
			if err != nil {
				return err
			}
			b, err := release.Load(args[1])
			if err != nil {
				return err
			}
			if a.Values == nil || b.Values == nil {
				return fmt.Errorf("both sides need a values snapshot (bare playbook records cannot be compared)")
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "diff %s (%s %s) -> %s (%s %s)\n",
				a.ID, a.Chart, a.Version, b.ID, b.Chart, b.Version)
			lines := release.DiffValues(a.Values, b.Values)
			if len(lines) == 0 {
				fmt.Fprintln(out, "values identical (no parameter changes)")
				return nil
			}
			for _, l := range lines {
				fmt.Fprintln(out, l)
			}
			return nil
		},
	}
}

// printYAML 序列化输出 YAML（部署记录 values 快照用）。
func printYAML(out io.Writer, v any) error {
	b, err := yaml.Marshal(v)
	if err != nil {
		return err
	}
	_, err = out.Write(b)
	return err
}

func boolLabel(failed bool) string {
	if failed {
		return "failed"
	}
	return "ok"
}
