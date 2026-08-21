package cli

// 运维与记录命令组（--help 的 Operations & Records Commands）：
// release（部署记录）与 modules（内置模块文档）。

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"wdp/internal/fmtutil"
	"wdp/internal/i18n"
	"wdp/internal/module"
	"wdp/internal/release"
	"wdp/internal/skel"
)

// newReleaseCmd 构造 `wdp release`（部署记录）。
func newReleaseCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "release",
		Short: i18n.T("deployment record viewing (list / show)", "部署记录查看（list / show）"),
	}
	cmd.AddCommand(newReleaseListCmd(), newReleaseShowCmd(), newReleaseDiffCmd())
	return cmd
}

// newReleaseListCmd 构造 `wdp release list`。
func newReleaseListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list [chart名前缀]",
		Short: "列出部署记录（新在前）",
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
				fmt.Fprintln(out, i18n.T("(no records)", "（无记录）"))
				return nil
			}
			tb := outPrinter(cmd).NewTable("ID", i18n.T("VERSION", "版本"), i18n.T("RESULT", "结果"), i18n.T("TIME", "时间"))
			for _, r := range recs {
				if r.Failed {
					tb.AddRow(fmtutil.C(r.ID), fmtutil.C(r.Version),
						fmtutil.CC(i18n.T("failed", "失败"), fmtutil.Red), fmtutil.C(r.Time.Format("2006-01-02 15:04:05")))
				} else {
					tb.AddRow(fmtutil.C(r.ID), fmtutil.C(r.Version),
						fmtutil.CC(i18n.T("ok", "成功"), fmtutil.Green), fmtutil.C(r.Time.Format("2006-01-02 15:04:05")))
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
		Short: "查看记录详情（--values 输出 values 快照 YAML，可直接 -f 重放）",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			rec, err := release.Load(args[0])
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if asValues {
				if rec.Values == nil {
					return fmt.Errorf("该记录无 values（裸 playbook 模式）")
				}
				return printYAML(out, rec.Values)
			}
			fmt.Fprintf(out, "ID:     %s\n时间:  %s\nchart:  %s %s\n结果:  %s\n主机:  %v\n",
				rec.ID, rec.Time.Format("2006-01-02 15:04:05"), rec.Chart, rec.Version,
				boolLabel(rec.Failed), rec.Hosts)
			if len(rec.ValuesRef) > 0 {
				fmt.Fprintf(out, "参数:  %v\n", rec.ValuesRef)
			}
			if rec.Values != nil {
				fmt.Fprintln(out, "\nvalues 快照（--values 查看完整内容）:")
				if err := printYAML(out, rec.Values); err != nil {
					return err
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asValues, "values", false,
		i18n.T("print only the values snapshot (YAML)", "仅输出 values 快照（YAML）"))
	return cmd
}

// newReleaseDiffCmd 构造 `wdp release diff`。
func newReleaseDiffCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "diff <id1> <id2>",
		Short: "对比两次部署记录的 values 快照（升级前回答这次会改哪些参数）",
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
				return fmt.Errorf("对比双方都需要 values 快照（裸 playbook 记录无法对比）")
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "对比 %s（%s %s）→ %s（%s %s）\n",
				a.ID, a.Chart, a.Version, b.ID, b.Chart, b.Version)
			lines := release.DiffValues(a.Values, b.Values)
			if len(lines) == 0 {
				fmt.Fprintln(out, "values 完全一致（无参数变更）")
				return nil
			}
			for _, l := range lines {
				fmt.Fprintln(out, l)
			}
			return nil
		},
	}
}

// newModulesCmd 构造 `wdp modules`。
func newModulesCmd() *cobra.Command {
	return &cobra.Command{
		Use: "modules [module]",
		Short: i18n.T("list built-in modules (with a module name, print parameter docs and example)",
			"列出内置模块（带模块名时输出参数文档与示例）"),
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			if len(args) == 1 {
				snippet, err := skel.ModuleSnippet(args[0])
				if err != nil {
					return err
				}
				fmt.Fprint(out, snippet)
				return nil
			}
			tb := outPrinter(cmd).NewTable(i18n.T("MODULE", "模块"), i18n.T("DESCRIPTION", "说明"))
			for _, name := range module.Names() {
				m, _ := module.Get(name)
				tb.AddRow(fmtutil.C(name), fmtutil.C(m.Desc()))
			}
			tb.Render()
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
		return "失败"
	}
	return "成功"
}
