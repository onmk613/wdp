package agentcmd

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/spf13/cobra"

	"wdp/internal/agent"
	"wdp/internal/buildinfo"
	"wdp/internal/ca"
	"wdp/internal/config"
	"wdp/internal/i18n"
)

// agentHelp 返回 agent 命令的长帮助。写成函数而非常量：语言由 i18n 包在
// init() 里定死，包级常量会在任何求值时机之前就把文案钉死。
func agentHelp() string {
	return i18n.T(`
Start the resident agent on a target host (the server side of the agent channel)

Listens on 127.0.0.1:<wdp.cfg [agent].port> by default; --listen 0.0.0.0:PORT exposes it
Auth: --ca/--cert/--key enable mTLS (client certs must be issued by that CA);
--pin-client-fp pins the allowed client fingerprints — exact revocation: drop a fingerprint and restart
Loopback-only listen may run unauthenticated (on a multi-user host any local user can call /exec and /file, so mTLS is advised);
unauthenticated non-loopback listen requires an explicit --allow-no-auth (trusted LAN only)
File territory: --allow-file-path narrows /file and /archive to the given directory trees (repeatable; default unrestricted) —
once enabled, remote upgrades require the agent binary's directory in the list; /exec stays unrestricted (deployment semantics)
Lifecycle: --idle-timeout exits after this long without an authenticated request (/health probes do not count; 0 = never);
--cleanup-on-shutdown self-cleans on that exit (temporary agents; the /shutdown signal always self-cleans);
--systemd-unit matches the unit name so Restart=always cannot loop on a deleted binary
Logging: --log-level trace|debug|info|warn|error; --log-file appends to disk (kept by self-cleanup for audit,
fetchable via wdp agentctl logs); --max-request-mib request body limit (default 64)

Usually runs permanently as a systemd service (installed by the server's enrollment script), no manual run needed
`, `
在目标主机上启动常驻 agent（agent 通道的服务端）

默认监听 127.0.0.1:<wdp.cfg [agent].port>；--listen 0.0.0.0:PORT 对外暴露
认证：--ca/--cert/--key 启用 mTLS（客户端证书须由该 CA 签发）；
--pin-client-fp 钉住允许的客户端指纹——精确吊销：删一个指纹重启即生效
仅回环监听可不认证（多用户主机上本机任意用户均可调用 /exec 与 /file，建议 mTLS）；
非回环无认证需显式 --allow-no-auth（仅可信内网）
文件领地：--allow-file-path 把 /file 与 /archive 收窄到指定目录树（可重复；缺省不限制）——
启用后远程升级要求清单包含 agent 二进制所在目录；/exec 不在收紧范围（部署语义即全权）
生命周期：--idle-timeout 无认证请求达此时长即退出（/health 探测不计入；0 = 永不）；
--cleanup-on-shutdown 退出时自清理（临时托管场景；/shutdown 信号总是自清理）；
--systemd-unit 匹配单元名，避免 Restart=always 循环拉起已删除的二进制
日志：--log-level trace|debug|info|warn|error；--log-file 追加落盘（自清理不删，供审计，
可经 wdp agentctl logs 拉取）；--max-request-mib 请求体上限（默认 64）

通常以 systemd 服务方式常驻（web-console 方向由 server 纳管脚本安装），无需手工运行
`)
}

// New 构造 agent 命令（wdp agent / wdp-agent agent 共用）。
func New() *cobra.Command {
	var (
		listen        string
		ca, cert, key string
		cleanup       bool
		pins          []string
		allowNoAuth   bool
		filePaths     []string
		systemdUnit   string
		maxRequestMB  int64
		idleTimeout   time.Duration
		logLevel      string
		logFile       string
	)

	cmd := &cobra.Command{
		Use: "agent",
		Short: i18n.T("start the resident agent (on target hosts)",
			"在目标主机上启动常驻 agent"),
		Long: agentHelp(),
		Args: cobra.NoArgs, // agent 不接受位置参数，多余参数此前被静默忽略
		RunE: func(cmd *cobra.Command, args []string) error {
			// 默认值在 RunE 内求值：命令树构造早于 wdp.cfg 加载，
			// 构造期取 config.Current() 会拿到内置默认端口而非配置值
			if !cmd.Flags().Changed("listen") {
				listen = "127.0.0.1:" + fmt.Sprint(config.Current().AgentPort())
			}
			// 最小值校验放在 CLI 层：防手滑配 1s 之类把常驻 agent 秒杀
			// （服务端不限制，测试可用任意短周期）
			if idleTimeout != 0 && idleTimeout < time.Minute {
				return errors.New("--idle-timeout must be 0 (disabled) or at least 1m")
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
			if err := srv.SetFilePathRoots(filePaths); err != nil {
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
			fmt.Printf("wdp agent %s listening on %s (%s)\n", buildinfo.BuildVersion(), listen, mode)
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				return err
			}
			return nil
		},
	}

	// Listen
	cmd.Flags().StringVar(&listen, "listen", "",
		i18n.T("listen address (default 127.0.0.1:<agent port from wdp.cfg>; use 0.0.0.0:PORT to expose)",
			"监听地址（缺省 127.0.0.1:<wdp.cfg 里的 agent 端口>；对外暴露用 0.0.0.0:PORT）"),
	)

	// mTLS
	cmd.Flags().StringVar(&ca, "ca", "", i18n.T("mTLS: CA certificate (client certs must be issued by it)",
		"mTLS：CA 证书（客户端证书须由它签发）"))
	cmd.Flags().StringVar(&cert, "cert", "", i18n.T("mTLS: server certificate", "mTLS：服务端证书"))
	cmd.Flags().StringVar(&key, "key", "", i18n.T("mTLS: server private key", "mTLS：服务端私钥"))

	// auth
	cmd.Flags().StringArrayVar(&pins, "pin-client-fp", nil,
		i18n.T("allowed client cert SHA256 fingerprints (repeatable; exact revocation: drop a fingerprint and restart)",
			"允许的客户端证书 SHA256 指纹（可重复；精确吊销：删一个指纹重启即生效）"),
	)
	cmd.Flags().BoolVar(&allowNoAuth, "allow-no-auth", false,
		i18n.T("explicitly allow unauthenticated non-loopback listen (trusted LAN only)",
			"显式允许非回环的无认证监听（仅可信内网）"),
	)

	// 文件领地（opt-in 收紧 /file、/archive）
	cmd.Flags().StringArrayVar(&filePaths, "allow-file-path", nil,
		i18n.T("restrict /file and /archive to this directory tree (repeatable; default unrestricted). Remote upgrades then require the agent binary's directory in the list",
			"把 /file 与 /archive 收窄到该目录树（可重复；缺省不限制）。启用后远程升级须将 agent 二进制所在目录列入"),
	)

	// cleanup & idle
	cmd.Flags().BoolVar(&cleanup, "cleanup-on-shutdown", false,
		i18n.T("self-clean on idle-timeout exit (temp agents); the /shutdown signal always self-cleans regardless",
			"空闲超时退出时自清理（临时托管场景）；/shutdown 信号总是自清理"),
	)
	cmd.Flags().StringVar(&systemdUnit, "systemd-unit", "wdp-agent",
		i18n.T("systemd unit to disable on self-cleanup (match your unit name, else Restart=always may loop on the deleted binary)",
			"自清理时停用的 systemd 单元名（须与实际单元名一致，否则 Restart=always 会循环拉起已删除的二进制）"),
	)
	cmd.Flags().DurationVar(&idleTimeout, "idle-timeout", 0,
		i18n.T("exit (with --cleanup-on-shutdown cleanup) when no authenticated request completes for this long; /health probes do not count (0 = never)",
			"无认证请求达此时长即退出（配合 --cleanup-on-shutdown 自清理）；/health 探测不计入（0 = 永不）"),
	)

	// request body size limit
	cmd.Flags().Int64Var(&maxRequestMB, "max-request-mib", 0,
		i18n.T("request body size limit in MiB (0 = built-in default 512)",
			"请求体体积上限（MiB，0 = 内置默认 512）"),
	)

	// logging
	cmd.Flags().StringVar(&logLevel, "log-level", "info",
		i18n.T("log level: trace|debug|info|warn|error (default info). info = executed commands, file transfers, extracts, cert updates, shutdown signals; debug = every operation in detail; trace = debug + httpdump of every request/response (sensitive fields redacted)",
			"日志级别：trace|debug|info|warn|error（缺省 info）。info = 执行的命令、文件传输、解包、证书更新、退出信号；debug = 每个操作的细节；trace = debug + 每个请求/响应的 httpdump（敏感字段脱敏）"),
	)
	cmd.Flags().StringVar(&logFile, "log-file", "",
		i18n.T("also append logs to this file (auto-creates the parent dir; NOT removed by self-cleanup — kept for audit; controller can fetch via agentctl logs or GET /file)",
			"日志同时追加落盘到此文件（自动创建父目录；自清理不删——供审计；控制端可经 agentctl logs 或 GET /file 拉取）"),
	)

	cmd.AddCommand(newGenCSRCmd())
	return cmd
}

// gencsrHelp 返回 gencsr 的长帮助（函数而非 const，理由见 agentHelp）。
func gencsrHelp() string {
	return i18n.T(`
Generate the agent private key locally and write a certificate signing request.

The key never leaves this host: enrollment submits only the CSR, and the
server signs a certificate against it (identity/SANs come from the server-side
enrollment record, never from the CSR). If --key already exists and parses, it
is reused so reinstalling keeps the host identity.
`, `
在本机生成 agent 私钥并写出证书签名请求（CSR）。

私钥不离开本机：纳管只提交 CSR，由 server 对其签发证书（身份/SAN 取自
server 侧的纳管记录，绝不取自 CSR）。--key 已存在且可解析时复用，
重装不换主机身份。
`)
}

// newGenCSRCmd 目标机侧纳管钥匙生成（server 纳管脚本/SSH 推装
// 调用）：私钥在本机生成并留在本机，只把 CSR 交给 server 签发——
// 私钥不落 server 磁盘、不经网络交付（docs/20）。已存在可解析的私钥
// 时复用（重装不换身份）。
func newGenCSRCmd() *cobra.Command {
	var keyPath, csrPath string
	cmd := &cobra.Command{
		Use: "gencsr",
		Short: i18n.T("generate (or reuse) the agent key on this host and emit a CSR for enrollment",
			"在本机生成（或复用）agent 私钥并输出纳管用 CSR"),
		Long: gencsrHelp(),
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			reused, err := ca.GenCSR(keyPath, csrPath)
			if err != nil {
				return err
			}
			if reused {
				fmt.Printf("reused existing key %s\n", keyPath)
			} else {
				fmt.Printf("generated key %s (0600)\n", keyPath)
			}
			fmt.Printf("CSR written to %s\n", csrPath)
			return nil
		},
	}
	cmd.Flags().StringVar(&keyPath, "key", "/etc/wdp/agent.key",
		i18n.T("agent private key path (reused as-is when it exists; 0600 when generated)",
			"agent 私钥路径（已存在则原样复用；新生成时权限 0600）"))
	cmd.Flags().StringVar(&csrPath, "csr", "/tmp/wdp-agent.csr",
		i18n.T("output CSR path (PEM); submit it to the server's enroll /csr endpoint",
			"输出 CSR 路径（PEM）；交由 server 的纳管 /csr 端点签发"))
	return cmd
}
