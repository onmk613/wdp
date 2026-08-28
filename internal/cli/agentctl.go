package cli

// agentctl：控制端对常驻 agent 的日常运维命令组——
// 证书生命周期（巡检到期 → 远程换证）、安装上线与退役自清理、日志拉取，
// 全程免登录目标机。操作内核在 internal/agentops；本文件只做参数与
// 主机来源选取（inventory + 主机模式，仅处理 conn: agent 的主机）。

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"wdp/internal/agentops"
	"wdp/internal/config"
	"wdp/internal/model"
)

// newAgentCtlCmd 构造 `wdp agentctl`。
func newAgentCtlCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "agentctl",
		Short: "resident agent ops from the controller: status / install / retire / logs",
	}
	cmd.AddCommand(newAgentStatusCmd(), newAgentInstallCmd(), newAgentRenewCmd(), newAgentRetireCmd(), newAgentLogsCmd())
	return cmd
}

// selectAgentHosts 按主机模式选取 conn: agent 的主机（其它通道跳过）。
func selectAgentHosts(pattern string) ([]*model.Host, error) {
	inv, err := loadInventories()
	if err != nil {
		return nil, err
	}
	hosts, err := inv.Select(pattern)
	if err != nil {
		return nil, err
	}
	var out []*model.Host
	for _, h := range hosts {
		if h.Conn == "agent" {
			out = append(out, h)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s", "no conn: agent hosts matched (agentctl only manages resident agents)")
	}
	return out, nil
}

// newAgentStatusCmd 构造 `wdp agentctl status`：批量巡检证书剩余有效期与
// 空闲自退出剩余时间。
func newAgentStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status <host-pattern>",
		Short: "report agent cert expiry & idle time per host",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			hosts, err := selectAgentHosts(args[0])
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return agentops.Status(ctx, hosts, config.Current().Forks(), connDefaults(), cmd.OutOrStdout())
		},
	}
}

// newAgentRetireCmd 构造 `wdp agentctl retire`：远程退役自清理
// （--yes 跳过确认）。默认清理 agent 二进制、证书材料（含 CA）并停用
// systemd 单元；--systemd-unit/--file 指定单元名与额外要删的路径。
func newAgentRetireCmd() *cobra.Command {
	var (
		unit  string
		files []string
		yes   bool
	)
	cmd := &cobra.Command{
		Use:   "retire <host-pattern>",
		Short: "remote-retire agents: shutdown + self-cleanup (binary, certs incl. CA, systemd unit)",

		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			hosts, err := selectAgentHosts(args[0])
			if err != nil {
				return err
			}
			if !yes {
				noun := "delete agent binary and certs (incl. CA), disable the systemd unit"
				if len(files) > 0 {
					noun += "; also delete the given extra paths"
				}
				fmt.Fprintf(cmd.OutOrStderr(), "about to retire %d host(s): %s. This is irreversible. Pass --yes to confirm.\n", len(hosts), noun)
				return nil
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return agentops.Retire(ctx, hosts, unit, files, config.Current().Forks(), connDefaults())
		},
	}
	cmd.Flags().StringVar(&unit, "systemd-unit", "",
		"systemd unit to disable (default: the unit name the agent started with, typically wdp-agent)",
	)
	cmd.Flags().StringArrayVar(&files, "file", nil,
		"extra file or directory to delete on the target (repeatable)",
	)
	cmd.Flags().BoolVar(&yes, "yes", false, "skip the confirmation prompt")
	return cmd
}

// newAgentLogsCmd 构造 `wdp agentctl logs`：拉取各 agent 近期日志
// （GET /logs 内存缓冲）到控制端逐主机落盘，实现"agent 日志传输到
// 控制端、用文件记录"。
func newAgentLogsCmd() *cobra.Command {
	var out string
	cmd := &cobra.Command{
		Use:   "logs <host-pattern>",
		Short: "fetch recent agent logs into per-host local files",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			hosts, err := selectAgentHosts(args[0])
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return agentops.Logs(ctx, hosts, out, config.Current().Forks(), connDefaults(), cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringVar(&out, "out", "wdp-agent-logs",
		"output directory for per-host log files (<host>.log)",
	)
	return cmd
}
