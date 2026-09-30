package cli

// 注册全部命令组，顺序即 help 展示序。
//
// 全量档是唯一的控制端产物："专用服务器上不想要 run/apply"这类诉求不在
// 编译期裁命令（原 wdp_no_cli console 档已删除：只省 0.4 MB，却要维护一对
// tag 与 stub 的同步），真正的收敛手段是 RBAC——见 internal/web/perm.go。

import (
	"github.com/spf13/cobra"

	"wdp/internal/agentcmd"
)

func registerCommandGroups(root *cobra.Command) {
	addCommandGroups(root,
		commandGroup{"deploy", "Deployment", []*cobra.Command{
			newRunCmd(),
			newPlanCmd(),
			newApplyCmd(),
			newAdhocCmd(),
		}},
		commandGroup{"chart", "Package", []*cobra.Command{
			newSchemaCmd(),
			newModuleCmd(),
			newRenderCmd(),
			newLintCmd(),
			newPackageCmd(),
		}},
		commandGroup{"repo", "Repository", []*cobra.Command{
			newRepoCmd(),
		}},
		commandGroup{"security", "Security", []*cobra.Command{
			newCACmd(),
		}},
		commandGroup{"agent", "Agent", []*cobra.Command{
			agentcmd.New(),
			newAgentCtlCmd(),
		}},
		commandGroup{"console", "Console", []*cobra.Command{
			newServerCmd(),
		}},
		commandGroup{"ops", "Operations", []*cobra.Command{
			newDriftCmd(),
			newReleaseCmd(),
			newInventoryCmd(),
		}},
	)
}
