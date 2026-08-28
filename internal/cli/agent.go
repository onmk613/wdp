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
