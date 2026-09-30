package cli

// agentctl：控制端对常驻 agent 的日常运维命令组——证书巡检（status）、
// 退役自清理（retire）、日志拉取（logs），全程经 agent HTTP 通道免登录
// 目标机。操作内核在 internal/agentops；本文件只做参数与主机来源选取
// （inventory + 主机模式，仅处理 conn: agent 的主机）。安装上线与换证由
// web-console 方向的 server 纳管流程承接。

import (
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"wdp/internal/agentops"
	"wdp/internal/config"
	"wdp/internal/i18n"
	"wdp/internal/model"
)

// agentctlHelp 返回 `wdp agentctl` 的长帮助（调用时求值）。
func agentctlHelp() string {
	return i18n.T(`Day-to-day operation of resident agents from the controller (no login to the targets)

status checks certificate expiry and idle time; retire shuts an agent down and cleans it up; logs fetches recent logs
<host-pattern> uses inventory syntax (comma union / ! exclusion / :& intersection / path.Match wildcards),
and only conn: agent hosts are handled
`, `控制端对常驻 agent 的日常运维（免登录目标机）

status 巡检证书到期与空闲时间；retire 退役自清理；logs 拉取近期日志
<host-pattern> 语法同 inventory（逗号联合 / ! 排除 / :& 交集 / path.Match 通配），
只处理 conn: agent 的主机
`)
}

// newAgentCtlCmd 构造 `wdp agentctl`。
func newAgentCtlCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use: "agentctl",
		Short: i18n.T("resident agent ops from the controller: status / retire / logs",
			"控制端对常驻 agent 的日常运维：status / retire / logs"),
		Long: agentctlHelp(),
	}
	cmd.AddCommand(newAgentStatusCmd(), newAgentRetireCmd(), newAgentLogsCmd())
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
		return nil, errors.New("no conn: agent hosts matched (agentctl only manages resident agents)")
	}
	return out, nil
}

// agentStatusHelp 返回 `wdp agentctl status` 的长帮助（调用时求值）。
func agentStatusHelp() string {
	return i18n.T(`Check agent status in bulk

Reports each host's remaining server-certificate validity and remaining idle time before self-exit,
so renew-cert can be scheduled ahead of expiry

Examples:
wdp agentctl status all
`, `批量巡检 agent 状态

逐主机报告服务端证书剩余有效期与空闲自退出剩余时间，
用于到期前安排 renew-cert

示例：
wdp agentctl status all
`)
}

// newAgentStatusCmd 构造 `wdp agentctl status`：批量巡检证书剩余有效期与
// 空闲自退出剩余时间。
func newAgentStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use: "status <host-pattern>",
		Short: i18n.T("report agent cert expiry & idle time per host",
			"逐主机报告 agent 证书到期与空闲时间"),
		Long: agentStatusHelp(),
		Args: cobra.ExactArgs(1),
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

// agentRetireHelp 返回 `wdp agentctl retire` 的长帮助（调用时求值）。
func agentRetireHelp() string {
	return i18n.T(`Retire agents remotely: shutdown + self-cleanup

By default deletes the agent binary and certificate material (including the CA) and disables the systemd unit;
--file adds extra paths to delete (repeatable)
The operation is irreversible: by default it only prints the actions to be taken, --yes confirms execution
--systemd-unit overrides the unit name (default: the one recorded when the agent started, usually wdp-agent)

Examples:
wdp agentctl retire 'db,!db1' --yes
`, `远程退役 agent：停机 + 自清理

默认删除 agent 二进制、证书材料（含 CA）并停用 systemd 单元；
--file 追加要删的额外路径（可重复）
操作不可逆：默认只打印将要执行的动作，--yes 确认执行
--systemd-unit 覆盖单元名（默认用 agent 启动时记录的，通常 wdp-agent）

示例：
wdp agentctl retire 'db,!db1' --yes
`)
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
		Use: "retire <host-pattern>",
		Short: i18n.T("remote-retire agents: shutdown + self-cleanup (binary, certs incl. CA, systemd unit)",
			"远程退役 agent：停机 + 自清理（二进制、证书含 CA、systemd 单元）"),
		Long: agentRetireHelp(),

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
				// 与 release del 的批量确认同口径：不可逆操作先列影响面
				// （主机清单），用户核对模式匹配结果后再 --yes——只报数量
				// 时，模式写错（如通配过宽）无从察觉
				names := make([]string, len(hosts))
				for i, h := range hosts {
					names[i] = h.Name
				}
				fmt.Fprintf(cmd.OutOrStderr(), "about to retire %d host(s):\n", len(hosts))
				const maxListed = 20
				shown := names
				if len(shown) > maxListed {
					shown = shown[:maxListed]
					fmt.Fprintf(cmd.OutOrStderr(), "  %s\n  ... and %d more\n", strings.Join(shown, ", "), len(names)-maxListed)
				} else {
					fmt.Fprintf(cmd.OutOrStderr(), "  %s\n", strings.Join(shown, ", "))
				}
				fmt.Fprintf(cmd.OutOrStderr(), "This will %s. This is irreversible. Pass --yes to confirm.\n", noun)
				return nil
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return agentops.Retire(ctx, hosts, unit, files, config.Current().Forks(), connDefaults())
		},
	}
	cmd.Flags().StringVar(&unit, "systemd-unit", "", i18n.T(
		"systemd unit to disable (default: the unit name the agent started with, typically wdp-agent)",
		"要停用的 systemd 单元（默认：agent 启动时记录的单元名，通常为 wdp-agent）"))
	cmd.Flags().StringArrayVar(&files, "file", nil, i18n.T(
		"extra file or directory to delete on the target (repeatable)",
		"目标机上额外要删除的文件或目录（可重复）"))
	cmd.Flags().BoolVar(&yes, "yes", false, "skip the confirmation prompt")
	return cmd
}

// agentLogsHelp 返回 `wdp agentctl logs` 的长帮助（调用时求值）。
func agentLogsHelp() string {
	return i18n.T(`Fetch recent agent logs to the controller

Reads each agent's in-memory log buffer and writes it to <output dir>/<host>.log per host
--out sets the output directory (default wdp-agent-logs)
Rotated --log-file audit logs are also reachable through the agent file API

Examples:
wdp agentctl logs webservers
`, `拉取各 agent 近期日志到控制端

读取 agent 内存日志缓冲，逐主机落盘为 <输出目录>/<host>.log
--out 指定输出目录（默认 wdp-agent-logs）
落盘的 --log-file 审计日志也可经 agent 文件接口获取

示例：
wdp agentctl logs webservers
`)
}

// newAgentLogsCmd 构造 `wdp agentctl logs`：拉取各 agent 近期日志
// （GET /logs 内存缓冲）到控制端逐主机落盘，实现"agent 日志传输到
// 控制端、用文件记录"。
func newAgentLogsCmd() *cobra.Command {
	var out string
	cmd := &cobra.Command{
		Use: "logs <host-pattern>",
		Short: i18n.T("fetch recent agent logs into per-host local files",
			"拉取各 agent 近期日志并逐主机落盘"),
		Long: agentLogsHelp(),
		Args: cobra.ExactArgs(1),
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
	cmd.Flags().StringVar(&out, "out", "wdp-agent-logs", i18n.T(
		"output directory for per-host log files (<host>.log)",
		"逐主机日志文件的输出目录（<host>.log）"))
	return cmd
}
