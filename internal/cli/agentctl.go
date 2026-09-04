package cli

// agentctl：控制端对常驻 agent 的日常运维命令组——
// 证书生命周期（巡检到期 → 安装上线 → 远程换证 → 退役自清理）、日志拉取，
// 全程免登录目标机。操作内核在 internal/agentops；本文件只做参数与
// 主机来源选取（inventory + 主机模式，仅处理 conn: agent 的主机）。

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"wdp/internal/agentops"
	"wdp/internal/ca"
	"wdp/internal/config"
	"wdp/internal/model"
)

const agentctlHelp = `
控制端对常驻 agent 的日常运维（免登录目标机）

status 巡检证书到期与空闲时间；install SSH 安装上线；renew-cert 远程换证；
retire 退役自清理；logs 拉取近期日志
<host-pattern> 语法同 inventory（逗号联合 / ! 排除 / :& 交集 / path.Match 通配），
只处理 conn: agent 的主机
`

// newAgentCtlCmd 构造 `wdp agentctl`。
func newAgentCtlCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "agentctl",
		Short: "resident agent ops from the controller: status / install / retire / logs",
		Long:  agentctlHelp,
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

const agentStatusHelp = `
批量巡检 agent 状态

逐主机报告服务端证书剩余有效期与空闲自退出剩余时间，
用于到期前安排 renew-cert

示例：
wdp agentctl status all
`

// newAgentStatusCmd 构造 `wdp agentctl status`：批量巡检证书剩余有效期与
// 空闲自退出剩余时间。
func newAgentStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status <host-pattern>",
		Short: "report agent cert expiry & idle time per host",
		Long:  agentStatusHelp,
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

const agentInstallHelp = `
经 SSH 批量安装常驻 agent（上线，与 retire 对称）

流程：推送控制端自身二进制到目标（--bin 目标机路径）→ 逐主机签发服务端证书
（--ca-cert/--ca-key 指定根 CA）→ 安装 systemd 单元并启动（--systemd-unit）
→ 可选 mTLS 验证（--client-cert/--client-key，空则跳过）
证书本地存 --cert-dir（<identity>.crt/.key），目标机证书目录 --dir（默认 /etc/wdp）
--systemd-unit 须与将来 retire --systemd-unit 一致

示例：
wdp agentctl install new-hosts --ca-cert ca.crt --ca-key ca.key \
  --client-cert me.crt --client-key me.key
`

// newAgentInstallCmd 构造 `wdp agentctl install`：SSH 推自身二进制 +
// 逐主机签发服务端证书 + 安装 systemd 单元并启动 + 可选 mTLS 验证。
// 与 retire 对称：retire 负责退役清理，install 负责上线。
func newAgentInstallCmd() *cobra.Command {
	o := agentops.Options{}
	cmd := &cobra.Command{
		Use:   "install <host-pattern>",
		Short: "install resident agents over SSH: push binary + per-host cert + systemd unit (counterpart of retire)",
		Long:  agentInstallHelp,

		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			hosts, err := selectAgentHosts(args[0])
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return agentops.Install(ctx, hosts, o, config.Current().Forks(), connDefaults(), cmd.OutOrStdout())
		},
	}
	f := cmd.Flags()
	f.StringVar(&o.CACertPath, "ca-cert", "ca.crt", "root CA that signs host certs")
	f.StringVar(&o.CAKeyPath, "ca-key", "ca.key", "root CA private key")
	f.StringVar(&o.CertDir, "cert-dir", ".", "local dir for per-host certs (<identity>.crt/.key)")
	f.StringVar(&o.BinPath, "bin", "/usr/local/bin/wdp", "binary path on the target")
	f.StringVar(&o.UnitName, "systemd-unit", "wdp-agent", "systemd unit name (must match retire --systemd-unit)")
	f.StringVar(&o.RemoteDir, "dir", "/etc/wdp", "cert directory on the target")
	f.StringVar(&o.ClientCert, "client-cert", "", "controller client cert for post-install verification (empty = skip)")
	f.StringVar(&o.ClientKey, "client-key", "", "controller client key")
	return cmd
}

const agentRenewHelp = `
远程换证：本地延期 → SSH 推送新证书 → 重启 agent → 可选验证

复用 wdp ca renew 语义：--days 在当前到期时间上增量（默认 30），
--new-key 轮换私钥（算法不变），本地自动备份旧证书
证书从 --cert-dir 取（<identity>.crt/.key），推送到目标机 --dir 并重启
--systemd-unit 单元；--client-cert/--client-key 可选装后验证

示例：
wdp agentctl renew-cert all --days 30
`

// newAgentRenewCmd 构造 `wdp agentctl renew-cert`：本地更新延期（增量+备份，
// 复用 wdp ca renew 语义）→ SSH 推送新证书 → 重启 agent → 可选验证。
func newAgentRenewCmd() *cobra.Command {
	o := agentops.Options{}
	cmd := &cobra.Command{
		Use:   "renew-cert <host-pattern>",
		Short: "renew host certs locally (delta days, backup) then push via SSH and restart agents",
		Long:  agentRenewHelp,

		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			hosts, err := selectAgentHosts(args[0])
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return agentops.RenewCert(ctx, hosts, o, config.Current().Forks(), connDefaults(), cmd.OutOrStdout())
		},
	}
	f := cmd.Flags()
	f.StringVar(&o.CACertPath, "ca-cert", "ca.crt", "root CA that re-signs")
	f.StringVar(&o.CAKeyPath, "ca-key", "ca.key", "root CA private key")
	f.StringVar(&o.CertDir, "cert-dir", ".", "local dir holding <identity>.crt/.key")
	f.StringVar(&o.UnitName, "systemd-unit", "wdp-agent", "systemd unit to restart")
	f.StringVar(&o.RemoteDir, "dir", "/etc/wdp", "cert directory on the target")
	f.StringVar(&o.ClientCert, "client-cert", "", "controller client cert for verification (empty = skip)")
	f.StringVar(&o.ClientKey, "client-key", "", "controller client key")
	f.BoolVar(&o.NewKey, "new-key", false, "rotate the private key (algorithm preserved)")
	f.IntVar(&o.Days, "days", ca.DefaultDays, "days to ADD to the current expiry")
	return cmd
}

const agentRetireHelp = `
远程退役 agent：停机 + 自清理（与 install 对称）

默认删除 agent 二进制、证书材料（含 CA）并停用 systemd 单元；
--file 追加要删的额外路径（可重复）
操作不可逆：默认只打印将要执行的动作，--yes 确认执行
--systemd-unit 覆盖单元名（默认用 agent 启动时记录的，通常 wdp-agent）

示例：
wdp agentctl retire 'db,!db1' --yes
`

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
		Long:  agentRetireHelp,

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
		"systemd unit to disable (default: the unit name the agent started with, typically wdp-agent)")
	cmd.Flags().StringArrayVar(&files, "file", nil,
		"extra file or directory to delete on the target (repeatable)")
	cmd.Flags().BoolVar(&yes, "yes", false, "skip the confirmation prompt")
	return cmd
}

const agentLogsHelp = `
拉取各 agent 近期日志到控制端

读取 agent 内存日志缓冲，逐主机落盘为 <输出目录>/<host>.log
--out 指定输出目录（默认 wdp-agent-logs）
落盘的 --log-file 审计日志也可经 agent 文件接口获取

示例：
wdp agentctl logs webservers
`

// newAgentLogsCmd 构造 `wdp agentctl logs`：拉取各 agent 近期日志
// （GET /logs 内存缓冲）到控制端逐主机落盘，实现"agent 日志传输到
// 控制端、用文件记录"。
func newAgentLogsCmd() *cobra.Command {
	var out string
	cmd := &cobra.Command{
		Use:   "logs <host-pattern>",
		Short: "fetch recent agent logs into per-host local files",
		Long:  agentLogsHelp,
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
		"output directory for per-host log files (<host>.log)")
	return cmd
}
