package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"wdp/internal/i18n"
	"wdp/internal/module"
	"wdp/internal/skel"
)

// newNewCmd 构造 `wdp new`（应用包生成）。
func newNewCmd() *cobra.Command {
	var (
		full  bool
		dir   string
		mod   string
		listM bool
	)
	cmd := &cobra.Command{
		Use: "new <app-name>",
		Short: i18n.T("scaffold a working chart skeleton (--full for a full-featured reference; --module to print module usage)",
			"生成可用的应用包骨架（--full 全能力参考；--module 查看模块用法片段）"),
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			if mod != "" {
				snippet, err := skel.ModuleSnippet(mod)
				if err != nil {
					return err
				}
				fmt.Fprint(out, snippet)
				return nil
			}
			if listM {
				for _, n := range module.Names() {
					fmt.Fprintln(out, n)
				}
				return nil
			}
			if len(args) != 1 {
				return fmt.Errorf("%s",
					i18n.T("app name required (or use --module <name> to view usage)", "需要应用名（或使用 --module <模块名> 查看用法）"))
			}
			root, err := skel.Scaffold(dir, args[0], full)
			if err != nil {
				return err
			}
			variant := i18n.T("minimal skeleton", "最小骨架")
			if full {
				variant = i18n.T("full-featured reference skeleton", "全能力参考骨架")
			}
			fmt.Fprintf(out, "%s: %s: %s\n", i18n.T("generated", "已生成"), variant, root)
			fmt.Fprintln(out, i18n.T("next steps:", "下一步："))
			fmt.Fprintf(out, "  wdp lint %s\n", root)
			fmt.Fprintf(out, "  wdp run %s --check -i <inventory>   # %s\n", root,
				i18n.T("zero-risk dry run", "零风险预演"))
			if full {
				fmt.Fprintf(out, "  wdp run %s --check --diff -i <inventory>  # %s\n", root,
					i18n.T("with content-level diff", "含内容级差异"))
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.BoolVar(&full, "full", false,
		i18n.T("generate a full-featured reference skeleton (strategy/hooks/delegation/dynamic groups/sub-charts/new modules)",
			"生成全能力参考骨架（策略/hook/委托/动态分组/子 chart/新模块均有示例）"))
	f.StringVar(&dir, "dir", ".", i18n.T("output directory (default: current dir)", "生成目录（默认当前目录）"))
	f.StringVar(&mod, "module", "",
		i18n.T("print parameter docs and an example task for the given module (no scaffolding)",
			"输出指定内置模块的参数文档与示例任务片段（不生成骨架）"))
	f.BoolVar(&listM, "list-modules", false, i18n.T("list all built-in module names", "列出全部内置模块名"))
	return cmd
}
