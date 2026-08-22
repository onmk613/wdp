package cli

// 代理命令组（--help 的 Agent Commands）：目标机常驻 agent。

import (
	"fmt"

	"github.com/spf13/cobra"

	"wdp/internal/agent"
	"wdp/internal/config"
	"wdp/internal/i18n"
)

// newAgentCmd 构造 `wdp agent`：目标机常驻服务。
func newAgentCmd() *cobra.Command {
	var (
		listen        string
		ca, cert, key string
		cleanup       bool
		pins          []string
		allowNoAuth   bool
		maxRequestMB  int64
	)
	cmd := &cobra.Command{
		Use:   "agent",
		Short: i18n.T("start the resident agent (on target hosts)", "启动常驻 agent（目标机）"),
		RunE: func(cmd *cobra.Command, args []string) error {
			// 默认值在 RunE 内求值：命令树构造早于 wdp.cfg 加载，
			// 构造期取 config.Current() 会拿到内置默认端口而非配置值
			if !cmd.Flags().Changed("listen") {
				listen = "127.0.0.1:" + fmt.Sprint(config.Current().AgentPort())
			}
			srv := agent.New(listen)
			srv.SetMaxRequestBody(maxRequestMB)
			if err := srv.ConfigureAuth(ca, cert, key); err != nil {
				return err
			}
			if err := srv.PinClientFingerprints(pins); err != nil {
				return err
			}
			srv.CleanupOnShutdown(cleanup)
			srv.AllowNoAuth(allowNoAuth)

			mode := i18n.T("no auth", "无认证")
			if ca != "" && cert != "" && key != "" {
				mode = i18n.T("mutual TLS", "mTLS 双向认证")
				if len(pins) > 0 {
					mode += i18n.T(" + pinned clients", " +指纹准许名单")
				}
			} else if agent.IsLoopbackListen(listen) {
				mode = i18n.T("no auth (loopback only)", "无认证（仅本机回环）")
				fmt.Fprintln(cmd.ErrOrStderr(), i18n.T(
					"warning: loopback without auth only blocks remote access; any local user on this host can invoke /exec and /file. Use mTLS on multi-user hosts.",
					"警告：无认证回环监听只挡远程访问，本机任意用户均可调用 /exec 与 /file；多用户主机请启用 mTLS。"))
			}
			fmt.Printf(i18n.T("wdp agent %s listening on %s (%s)\n", "wdp agent %s 监听 %s（%s）\n"), Version, listen, mode)
			return srv.ListenAndServe()
		},
	}

	cmd.Flags().StringVar(&listen, "listen", "",
		i18n.T("listen address (default 127.0.0.1:<agent port from wdp.cfg>; use 0.0.0.0:PORT to expose)",
			"监听地址（默认 127.0.0.1:<wdp.cfg 中 agent 端口>；对外监听需显式 0.0.0.0:端口）"))
	cmd.Flags().StringVar(&ca, "ca", "", i18n.T("mTLS: CA certificate (client certs must be issued by it)", "mTLS：CA 证书（客户端证书由该 CA 签发）"))
	cmd.Flags().StringVar(&cert, "cert", "", i18n.T("mTLS: server certificate", "mTLS：服务端证书"))
	cmd.Flags().StringVar(&key, "key", "", i18n.T("mTLS: server private key", "mTLS：服务端私钥"))
	cmd.Flags().BoolVar(&cleanup, "cleanup-on-shutdown", false,
		i18n.T("delete own binary and mTLS material files on /shutdown then exit (for push temp agents)",
			"收到 /shutdown 时删除自身二进制与 mTLS 证书文件后退出（push 临时 agent 用）"))
	cmd.Flags().StringArrayVar(&pins, "pin-client-fp", nil,
		i18n.T("allowed client cert SHA256 fingerprints (repeatable; exact revocation: drop a fingerprint and restart)",
			"客户端证书 SHA256 指纹准许名单（可多次；精确吊销：移除指纹重启即拒收）"))
	cmd.Flags().BoolVar(&allowNoAuth, "allow-no-auth", false,
		i18n.T("explicitly allow unauthenticated non-loopback listen (trusted LAN only)",
			"显式允许无认证对外监听（仅限可信内网）"))
	cmd.Flags().Int64Var(&maxRequestMB, "max-request-mb", 0,
		i18n.T("request body size limit in MiB (0 = built-in default 64)",
			"请求体大小上限（MiB，0 = 内置默认 64）"))
	return cmd
}
