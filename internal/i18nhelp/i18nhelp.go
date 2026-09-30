// Package i18nhelp 本地化 cobra 的框架文案。
//
// 各命令自己的 Short/Long/flag usage 由各命令经 i18n.T 给出，但下面这些
// 硬编码在 cobra 里、只能靠改模板或抢先注册 flag 覆盖：
//
//   - usage 模板的框架词（Usage: / Available Commands: / Flags: / …）
//   - `-h/--help` 的说明（cobra 在 execute() 里惰性补 "help for <命令名>"）
//   - 内置 help / completion 命令（含 4 个 shell 子命令）的描述
//   - 无组子命令的兜底小节（本项目把它消掉：业务命令全部分组，框架命令
//     归入显式的「框架命令」组）
//
// 语言由 internal/i18n 在 init() 里定死，本包只读不判；英文一律保持 cobra
// 原文（Setup 直接返回），中文才做替换——英文侧因此与升级前逐字节一致。
//
// 两个 CLI 入口（cmd/wdp 与 cmd/wdp-agent）共用本包，避免各写一份模板。
package i18nhelp

import (
	"github.com/spf13/cobra"

	"wdp/internal/i18n"
)

// FrameworkGroupID 是 cobra 内置 help/completion 命令的归属组 ID。
// 导出供命令树的测试对账（断言框架命令确实归了组、未落进未分组兜底小节）。
const FrameworkGroupID = "framework"

// zhUsageTemplate 是 cobra defaultUsageTemplate 的中文版（仅框架词不同，
// 结构与空白逐字对应，确保分组渲染、列对齐行为完全一致）。
const zhUsageTemplate = `用法:{{if .Runnable}}
  {{.UseLine}}{{end}}{{if .HasAvailableSubCommands}}
  {{.CommandPath}} [command]{{end}}{{if gt (len .Aliases) 0}}

别名:
  {{.NameAndAliases}}{{end}}{{if .HasExample}}

示例:
{{.Example}}{{end}}{{if .HasAvailableSubCommands}}{{$cmds := .Commands}}{{if eq (len .Groups) 0}}

可用命令:{{range $cmds}}{{if (or .IsAvailableCommand (eq .Name "help"))}}
  {{rpad .Name .NamePadding }} {{.Short}}{{end}}{{end}}{{else}}{{range $group := .Groups}}

{{.Title}}{{range $cmds}}{{if (and (eq .GroupID $group.ID) (or .IsAvailableCommand (eq .Name "help")))}}
  {{rpad .Name .NamePadding }} {{.Short}}{{end}}{{end}}{{end}}{{if not .AllChildCommandsHaveGroup}}

其他命令:{{range $cmds}}{{if (and (eq .GroupID "") (ne .Name "help") (ne .Name "completion") (or .IsAvailableCommand (eq .Name "help")))}}
  {{rpad .Name .NamePadding }} {{.Short}}{{end}}{{end}}{{end}}{{end}}{{end}}{{if .HasAvailableLocalFlags}}

参数:
{{.LocalFlags.FlagUsages | trimTrailingWhitespaces}}{{end}}{{if .HasAvailableInheritedFlags}}

全局参数:
{{.InheritedFlags.FlagUsages | trimTrailingWhitespaces}}{{end}}{{if .HasHelpSubCommands}}

其他帮助主题:{{range .Commands}}{{if .IsAdditionalHelpTopicCommand}}
  {{rpad .CommandPath .CommandPathPadding}} {{.Short}}{{end}}{{end}}{{end}}{{if .HasAvailableSubCommands}}

使用 "{{.CommandPath}} [command] --help" 查看某条命令的更多信息。{{end}}
`

// Setup 把框架文案按当前语言装到 root。幂等，可在每次构造命令树时调用。
//
// 无论语言都会执行的两件事：构造 help/completion 并归入框架组（否则帮助里
// 会多出一个空的「未分组」兜底小节），以及给整棵树预注册 `-h`（cobra 惰性
// 补的默认文案是英文）。其余替换只在中文下发生。
func Setup(root *cobra.Command) {
	registerFrameworkGroup(root)
	if i18n.Current() != i18n.Zh {
		return // 英文：cobra 模板与原文案，零差异
	}
	root.SetUsageTemplate(zhUsageTemplate)
	localizeHelpFlags(root)
	localizeHelpCmd(root)
	localizeCompletionCmd(root)
}

// registerFrameworkGroup 构造 help/completion 并归入显式的框架组。
// cobra 的 usage 模板在「存在无组子命令」时会打印兜底小节（Additional
// Commands / 其他命令），而这两个内置命令本就没有组——归组后该小节自然
// 消失，它们也有了正当位置。先注册组再构造命令，AddCommand 时才能带上
// GroupID。
func registerFrameworkGroup(root *cobra.Command) {
	root.AddGroup(&cobra.Group{
		ID:    FrameworkGroupID,
		Title: i18n.T("Framework", "框架命令"),
	})
	root.InitDefaultHelpCmd()
	root.InitDefaultCompletionCmd()
	for _, c := range root.Commands() {
		if c.Name() == "help" || c.Name() == "completion" {
			c.GroupID = FrameworkGroupID
		}
	}
}

// localizeHelpFlags 递归给整棵命令树预注册中文的 -h/--help 说明。
func localizeHelpFlags(cmd *cobra.Command) {
	if cmd.Flags().Lookup("help") == nil {
		cmd.Flags().BoolP("help", "h", false, i18n.T("show help", "显示帮助"))
	}
	for _, sub := range cmd.Commands() {
		localizeHelpFlags(sub)
	}
}

// localizeHelpCmd 改写内置 help 命令的描述。
func localizeHelpCmd(root *cobra.Command) {
	for _, c := range root.Commands() {
		if c.Name() != "help" {
			continue
		}
		c.Short = i18n.T("Help about any command", "查看任意命令的帮助")
		c.Long = i18n.T(
			"Help provides help for any command in the application.\nSimply type "+root.Name()+" help [path to command] for full details.",
			"帮助命令提供应用中任意命令的说明。\n输入 "+root.Name()+" help [命令路径] 查看完整说明。")
	}
}

// localizeCompletionCmd 改写内置 completion 命令及其 shell 子命令的描述。
func localizeCompletionCmd(root *cobra.Command) {
	for _, c := range root.Commands() {
		if c.Name() != "completion" {
			continue
		}
		c.Short = i18n.T("Generate the autocompletion script for the specified shell",
			"为指定 shell 生成自动补全脚本")
		c.Long = i18n.T(`Generate the autocompletion script for `+root.Name()+` for the specified shell.
See each sub-command's help for details on how to use the generated script.
`, `为 `+root.Name()+` 生成指定 shell 的自动补全脚本。
各子命令的帮助里有生成脚本的使用说明。
`)
		for _, sub := range c.Commands() {
			if sub.Name() == "help" {
				continue
			}
			shell := sub.Name()
			sub.Short = i18n.T("Generate the autocompletion script for "+shell,
				"生成 "+shell+" 的自动补全脚本")
			if f := sub.Flags().Lookup("no-descriptions"); f != nil {
				f.Usage = i18n.T("disable completion descriptions", "不输出补全项的描述")
			}
		}
	}
}
