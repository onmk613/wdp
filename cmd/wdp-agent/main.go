package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"wdp/internal/agentcmd"
	"wdp/internal/buildinfo"
	"wdp/internal/i18n"
	"wdp/internal/i18nhelp"
)

func main() {
	root := &cobra.Command{
		Use:           "wdp-agent",
		Short:         i18n.T("wdp agent (thin entry: task acceptance and execution only)", "wdp agent（瘦入口：仅任务接受与执行）"),
		SilenceUsage:  true,
		SilenceErrors: true,
		Version: fmt.Sprintf("%s\ngolang %s\ncommit %s\nbuilt %s\ntier %s\n%s\n%s",
			buildinfo.Version, buildinfo.GoVersion, buildinfo.Commit, buildinfo.BuildDate,
			buildinfo.Tier, buildinfo.BuildVersion(), tierNote()),
	}
	// -h 的说明由 cobra 在 execute() 里惰性补（缺省 "help for wdp-agent"）：
	// InitDefaultHelpFlag 只在该 flag 不存在时才动手，先注册即本地化生效。
	root.Flags().BoolP("help", "h", false, i18n.T("show help", "显示帮助"))
	// 本入口只有 agent 一条业务命令，同样要分组：不分组会触发 cobra 的
	// 未分组兜底小节（"其他命令"），语义含糊。
	root.AddGroup(&cobra.Group{ID: "agent", Title: i18n.T("Agent", "Agent")})
	agentCmd := agentcmd.New()
	agentCmd.GroupID = "agent"
	root.AddCommand(agentCmd)
	// 框架文案（usage 模板、help/completion 描述、分组）与主命令共用一套
	i18nhelp.Setup(root)
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// tierNote 提示裸构建（未经 build.sh 注入 tier）的读者：本入口的正确
// 档位是 agent，build.sh 会注入；裸 go build 时 buildinfo 缺省值不反映
// 实际裁剪面。
func tierNote() string {
	if buildinfo.Tier == "agent" {
		return ""
	}
	return i18n.T(
		"note: tier not injected (bare build); this entry point is the agent tier, build.sh injects the right tier",
		"note: tier 未注入（裸构建）；本入口编译产物即 agent 档，build.sh 会注入正确 tier")
}
