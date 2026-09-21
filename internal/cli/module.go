package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"wdp/internal/fmtutil"
	"wdp/internal/module"
	"wdp/internal/playbook"
)

const moduleHelp = `
内置模块速查

不带参数：表格列出全部内置模块（名称 + 一句话描述）
带模块名：输出该模块的参数文档与可直接粘贴的示例片段

示例：
wdp module                # 全部模块列表
wdp module template       # template 的参数与示例
`

// newModuleCmd 构造 `wdp module`：无参列出全部内置模块，带名输出参数
// 文档与示例片段。
func newModuleCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "module [module-name]",
		Short: "list built-in modules (with a name, print parameter docs and example)",
		Long:  moduleHelp,

		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: completeModuleNames,
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			if len(args) == 1 {
				// chart 引用不是注册表里的内置模块（由执行器展开），
				// 文档单独维护——`wdp module chart` 同样可查
				if args[0] == "chart" {
					fmt.Fprint(out, playbook.ChartRefDoc())
					return nil
				}
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
			tb.AddRow(fmtutil.C("chart"), fmtutil.C("chart 引用任务（非内置模块：chart 内=子 chart，裸 playbook=同级 charts/ 目录）"))
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
	if strings.HasPrefix("chart", toComplete) {
		out = append(out, "chart\tchart 引用任务（chart 内=子 chart，裸 playbook=同级 charts/ 目录）")
	}
	return out, cobra.ShellCompDirectiveNoFileComp
}
