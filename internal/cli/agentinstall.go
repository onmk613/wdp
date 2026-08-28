package cli

// `wdp agentctl install / renew-cert` 的命令装配。安装与换证的操作内核
// （SSH 推送、证书签发/延期、systemd 单元、mTLS 验证）在
// internal/agentops，本文件只声明 flag 与主机来源。

import (
	"os"
	"os/signal"
	"syscall"

	"wdp/internal/agentops"
	"wdp/internal/ca"
	"wdp/internal/config"

	"github.com/spf13/cobra"
)

// newAgentInstallCmd 构造 `wdp agentctl install`：SSH 推自身二进制 +
// 逐主机签发服务端证书 + 安装 systemd 单元并启动 + 可选 mTLS 验证。
// 与 retire 对称：retire 负责退役清理，install 负责上线。
func newAgentInstallCmd() *cobra.Command {
	o := agentops.Options{}
	cmd := &cobra.Command{
		Use:   "install <host-pattern>",
		Short: "install resident agents over SSH: push binary + per-host cert + systemd unit (counterpart of retire)",

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

// newAgentRenewCmd 构造 `wdp agentctl renew-cert`：本地更新延期（增量+备份，
// 复用 wdp ca renew 语义）→ SSH 推送新证书 → 重启 agent → 可选验证。
func newAgentRenewCmd() *cobra.Command {
	o := agentops.Options{}
	cmd := &cobra.Command{
		Use:   "renew-cert <host-pattern>",
		Short: "renew host certs locally (delta days, backup) then push via SSH and restart agents",

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
