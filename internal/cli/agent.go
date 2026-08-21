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
	)
	cmd := &cobra.Command{
		Use:   "agent",
		Short: i18n.T("start the resident agent (on target hosts)", "启动常驻 agent（目标机）"),
		RunE: func(cmd *cobra.Command, args []string) error {
			srv := agent.New(listen)
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
			}
			fmt.Printf(i18n.T("wdp agent %s listening on %s (%s)\n", "wdp agent %s 监听 %s（%s）\n"), Version, listen, mode)
			return srv.ListenAndServe()
		},
	}

	cmd.Flags().StringVar(&listen, "listen", "127.0.0.1:"+fmt.Sprint(config.Current().AgentPort()),
		i18n.T("listen address (default binds loopback only; use 0.0.0.0:PORT to expose)", "监听地址（默认仅绑定回环；对外监听需显式 0.0.0.0:端口）"))
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
	return cmd
}
