package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"wdp/internal/fmtutil"
	"wdp/internal/module"
)

// newModuleCmd 构造 `wdp module`：无参列出全部内置模块，带名输出参数
// 文档与示例片段。
func newModuleCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "module [module-name]",
		Short: "list built-in modules (with a name, print parameter docs and example)",

		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: completeModuleNames,
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			if len(args) == 1 {
				snippet, err := module.Snippet(args[0])
				if err != nil {
					return err
				}
				fmt.Fprint(out, snippet)
				return nil
			}
			tb := outPrinter(cmd).NewTable("MODULE", "DESCRIPTION")
			for _, name := range module.Names() {
				m, _ := module.Get(name)
				tb.AddRow(fmtutil.C(name), fmtutil.C(m.Desc()))
			}
			tb.Render()
			return nil
		},
	}
}

// completeModuleNames 补全内置模块名（cobra 约定：候选以 "name\tdesc"
// 形式返回，zsh/fish 补全菜单展示描述；首个参数已给出后不再补全）。
func completeModuleNames(_ *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	var out []string
	for _, name := range module.Names() {
		if strings.HasPrefix(name, toComplete) {
			if m, ok := module.Get(name); ok {
				out = append(out, name+"\t"+m.Desc())
			}
		}
	}
	return out, cobra.ShellCompDirectiveNoFileComp
}
