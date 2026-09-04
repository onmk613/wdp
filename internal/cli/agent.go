package cli

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/spf13/cobra"

	"wdp/internal/agent"
	"wdp/internal/config"
)

const agentHelp = `
在目标主机上启动常驻 agent（agent 通道的服务端）

默认监听 127.0.0.1:<wdp.cfg [agent].port>；--listen 0.0.0.0:PORT 对外暴露
认证：--ca/--cert/--key 启用 mTLS（客户端证书须由该 CA 签发）；
--pin-client-fp 钉住允许的客户端指纹——精确吊销：删一个指纹重启即生效
仅回环监听可不认证（多用户主机上本机任意用户均可调用 /exec 与 /file，建议 mTLS）；
非回环无认证需显式 --allow-no-auth（仅可信内网）
生命周期：--idle-timeout 无认证请求达此时长即退出（/health 探测不计入；0 = 永不）；
--cleanup-on-shutdown 退出时自清理（push 临时 agent 场景；/shutdown 信号总是自清理）；
--systemd-unit 匹配单元名，避免 Restart=always 循环拉起已删除的二进制
日志：--log-level trace|debug|info|warn|error；--log-file 追加落盘（自清理不删，供审计，
可经 wdp agentctl logs 拉取）；--max-request-mib 请求体上限（默认 64）

通常由 wdp agentctl install 经 SSH 安装为 systemd 服务，无需手工运行
`

func newAgentCmd() *cobra.Command {
	var (
		listen        string
		ca, cert, key string
		cleanup       bool
		pins          []string
		allowNoAuth   bool
		systemdUnit   string
		maxRequestMB  int64
		idleTimeout   time.Duration
		logLevel      string
		logFile       string
	)

	cmd := &cobra.Command{
		Use:   "agent",
		Short: "start the resident agent (on target hosts)",
		Long:  agentHelp,
		RunE: func(cmd *cobra.Command, args []string) error {
			// 默认值在 RunE 内求值：命令树构造早于 wdp.cfg 加载，
			// 构造期取 config.Current() 会拿到内置默认端口而非配置值
			if !cmd.Flags().Changed("listen") {
				listen = "127.0.0.1:" + fmt.Sprint(config.Current().AgentPort())
			}
			// 最小值校验放在 CLI 层：防手滑配 1s 之类把常驻 agent 秒杀
			// （服务端不限制，测试可用任意短周期）
			if idleTimeout != 0 && idleTimeout < time.Minute {
				return fmt.Errorf("%s", "--idle-timeout must be 0 (disabled) or at least 1m")
			}
			srv := agent.New(listen)
			srv.SetMaxRequestBody(maxRequestMB)
			if err := srv.SetLogLevel(logLevel); err != nil {
				return err
			}
			if err := srv.SetLogFile(logFile); err != nil {
				return err
			}
			if err := srv.ConfigureAuth(ca, cert, key); err != nil {
				return err
			}
			if err := srv.PinClientFingerprints(pins); err != nil {
				return err
			}
			srv.CleanupOnShutdown(cleanup)
			srv.AllowNoAuth(allowNoAuth)
			srv.SetSystemdUnit(systemdUnit)
			srv.SetIdleTimeout(idleTimeout)

			mode := "no auth"
			if ca != "" && cert != "" && key != "" {
				mode = "mutual TLS"
				if len(pins) > 0 {
					mode += " + pinned clients"
				}
			} else if agent.IsLoopbackListen(listen) {
				mode = "no auth (loopback only)"
				fmt.Fprintln(cmd.ErrOrStderr(), "warning: loopback without auth only blocks remote access; any local user on this host can invoke /exec and /file. Use mTLS on multi-user hosts.")
			}
			if idleTimeout > 0 {
				mode += ", idle exit after " + idleTimeout.String()
			}
			if logFile != "" {
				mode += ", log file " + logFile
			}
			fmt.Printf("wdp agent %s listening on %s (%s)\n", Version, listen, mode)
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				return err
			}
			return nil
		},
	}

	// Listen
	cmd.Flags().StringVar(&listen, "listen", "",
		"listen address (default 127.0.0.1:<agent port from wdp.cfg>; use 0.0.0.0:PORT to expose)",
	)

	// mTLS
	cmd.Flags().StringVar(&ca, "ca", "", "mTLS: CA certificate (client certs must be issued by it)")
	cmd.Flags().StringVar(&cert, "cert", "", "mTLS: server certificate")
	cmd.Flags().StringVar(&key, "key", "", "mTLS: server private key")

	// auth
	cmd.Flags().StringArrayVar(&pins, "pin-client-fp", nil,
		"allowed client cert SHA256 fingerprints (repeatable; exact revocation: drop a fingerprint and restart)",
	)
	cmd.Flags().BoolVar(&allowNoAuth, "allow-no-auth", false,
		"explicitly allow unauthenticated non-loopback listen (trusted LAN only)",
	)

	// cleanup & idle
	cmd.Flags().BoolVar(&cleanup, "cleanup-on-shutdown", false,
		"self-clean on idle-timeout exit (push temp agents); the /shutdown signal always self-cleans regardless",
	)
	cmd.Flags().StringVar(&systemdUnit, "systemd-unit", "wdp-agent",
		"systemd unit to disable on self-cleanup (match your unit name, else Restart=always may loop on the deleted binary)",
	)
	cmd.Flags().DurationVar(&idleTimeout, "idle-timeout", 0,
		"exit (with --cleanup-on-shutdown cleanup) when no authenticated request completes for this long; /health probes do not count (0 = never; push agents are started with 60m by wdp.cfg [agent].idle_timeout_min)",
	)

	// request body size limit
	cmd.Flags().Int64Var(&maxRequestMB, "max-request-mb", 0,
		"request body size limit in MiB (0 = built-in default 64)",
	)

	// logging
	cmd.Flags().StringVar(&logLevel, "log-level", "info",
		"log level: trace|debug|info|warn|error (default info). info = executed commands, file transfers, extracts, cert updates, shutdown signals; debug = every operation in detail; trace = debug + httpdump of every request/response (sensitive fields redacted)",
	)
	cmd.Flags().StringVar(&logFile, "log-file", "",
		"also append logs to this file (auto-creates the parent dir; NOT removed by self-cleanup — kept for audit; controller can fetch via agentctl logs or GET /file)",
	)

	return cmd
}
