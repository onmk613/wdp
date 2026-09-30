package cli

// wdp server：Web 控制台（web-console 方向的组合根）。数据落本地 SQLite
// （默认 ./wdp-data/wdp.db），首启自动引导管理员账号；主机纳管、远程
// 执行与 chart 管理随后续版本在此命令上扩展。
//
// 本文件是 cli 包里唯一 import web/store 的地方——console 域即由此进入
// 全量档的依赖闭包。控制端不再分"带/不带 console"两档（原 wdp_no_console
// tag 已删除，理由见 build.sh 头部档位说明）：server 命令面只在全量档提供，
// 瘦 agent 档走独立入口 cmd/wdp-agent，压根不经 internal/cli。

import (
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/spf13/cobra"

	"wdp/internal/datalock"
	"wdp/internal/i18n"
	"wdp/internal/model"
	"wdp/internal/store"
	"wdp/internal/web"
)

// serverHelp 返回 `wdp server` 的长帮助（调用时求值）。
func serverHelp() string {
	return i18n.T(`Start the web console (host inventory · liveness probing · enrollment)

Data lives in local SQLite (wdp.db under the --data directory by default; pure-Go driver, zero external
dependencies; --db names the database file explicitly)
Admin account (--admin-user, default admin): when --admin-pass / --admin-pass-env is given explicitly,
every start ensures the account password matches that value (change the value and restart to rotate,
idempotent); when neither is given, a random password is generated and printed once at first start.
First start also bootstraps the enrollment trust chain (<data>/ca: root CA with a 10-year default +
a controller client certificate).

Host enrollment: the console generates a one-off token (valid for 30 minutes by default); running the
handed-out one-liner on the target completes the install — the script pulls the wdp binary from the server
for the target's architecture (the server must run from the multi-arch bin directory produced by build.sh),
collects a per-host certificate and installs a systemd-managed resident agent, all over mTLS.

The app library is also exposed as a Helm-compatible chart repository (/charts/index.yaml and
/charts/<name>-<version>.tgz, session/Basic auth) — consumed from the CLI by wdp repo list/show/pull/push
and wdp run --repo.

Listening on 127.0.0.1:7603 by default — use --addr 0.0.0.0 when targets must call back, plus --advertise
for an externally reachable base URL; for public exposure put it behind a TLS reverse proxy, or enable
native console TLS with --tls-cert/--tls-key (certificates can be issued with wdp ca issue).
The enrollment script runs as root, so **handing out one-liners over plaintext HTTP is refused by default**
(the CA fingerprint travels in the same script and cannot stop an active man-in-the-middle); pass
--allow-plaintext-enroll explicitly to bootstrap over plaintext on a trusted network.

--data is best given as an absolute path (the relative default wdp-data drifts with the working directory);
--admin-pass is visible to other users on the machine (ps) and lands in shell history, so prefer
--admin-pass-env. The root CA private key is stored unencrypted (0600) by default; set the WDP_CA_PASS
environment variable and the bootstrapped root CA key is encrypted with it (the same variable is then
required on every start).

Examples:
wdp server                                        # try it locally
wdp server --addr 0.0.0.0:7603 --advertise http://10.0.0.5:7603 --admin-pass-env WDP_ADMIN_PASS
wdp server --addr 0.0.0.0:7603 --tls-cert /etc/wdp/console.crt --tls-key /etc/wdp/console.key
`, `启动 Web 控制台（主机台账 · 探活 · 纳管）

数据默认落本地 SQLite（--data 目录下 wdp.db，纯 Go 驱动，零外部依赖，单文件
随数据目录整体备份/搬迁）。--db 也接受 PostgreSQL / MySQL 的连接地址：
  postgres://user:pass@host:5432/wdpdb?sslmode=require
  mysql://user:pass@host:3306/wdp
密码建议写 ${VAR} 由环境变量注入（wdp 自己在解析时插值），避免进命令行与
shell 历史；日志与报错里的地址一律遮掉密码。远程库下数据目录排他锁不生效，
多副本并发由数据库自身保证；
管理员账号（--admin-user，默认 admin）：显式给出 --admin-pass /
--admin-pass-env 时，启动即确保账号密码与之一致（改值后重启即改密，
幂等）；均未给出则仅首启随机生成并打印一次。首启同时自举纳管信任链
（<data>/ca：根 CA 默认 10 年 + 控制端客户端证书）。

主机纳管：控制台生成一次性 token（默认 30 分钟有效），在目标机执行
下发的一键命令即完成安装——脚本按目标机架构从 server 拉取 wdp 二进制
（server 须运行在 build.sh 产出的多架构 bin 目录中）、领取逐主机证书
并装配 systemd 常驻 agent，全程 mTLS。

应用库同时以 Helm 兼容 chart 仓库暴露（/charts/index.yaml 与
/charts/<name>-<version>.tgz，会话/Basic 认证）——CLI 侧 wdp repo
list/show/pull/push 与 wdp run --repo 消费。

监听默认 127.0.0.1:7603——目标机需要回连时用 --addr 0.0.0.0 并配
--advertise 指定外部可达基址；公网暴露请置于反向代理 TLS 之后，或用
--tls-cert/--tls-key 启用控制台原生 TLS（证书可用 wdp ca issue 签发）。
下发的纳管脚本以 root 执行，故**默认拒绝在明文 HTTP 下生成一键命令**
（脚本内嵌的 CA 指纹与脚本同源，挡不住主动中间人）；确需在可信内网
明文引导时显式加 --allow-plaintext-enroll。

--data 建议绝对路径（默认相对路径 wdp-data 随启动目录漂移）；
--admin-pass 经命令行传参对同机用户可见（ps）且会进 shell 历史，建议改用
--admin-pass-env。根 CA 私钥默认明文（0600）落盘；设 WDP_CA_PASS 环境变量
后首启自举的根 CA 私钥以口令加密（此后每次启动都需同一环境变量）。

示例：
wdp server                                        # 本机体验
wdp server --addr 0.0.0.0:7603 --advertise http://10.0.0.5:7603 --admin-pass-env WDP_ADMIN_PASS
wdp server --addr 0.0.0.0:7603 --tls-cert /etc/wdp/console.crt --tls-key /etc/wdp/console.key
`)
}

// newServerCmd 构造 `wdp server`。
func newServerCmd() *cobra.Command {
	var (
		addr        string
		dataDir     string
		adminUser   string
		adminPass   string
		adminPassEn string
		advertise   string
		caDays      int
		plainEnroll bool
		trustProxy  []string
		dbAddr      string
		tlsCert     string
		tlsKey      string
		runsDays    int
		auditDays   int
		draftsDays  int
	)
	cmd := &cobra.Command{
		Use: "server",
		Short: i18n.T("run the web console (host inventory, probing & enrollment)",
			"启动 Web 控制台（主机台账 · 探活 · 纳管）"),
		Long: serverHelp(),
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if adminPass != "" {
				// 保留兼容但醒目告警：命令行参数对同机所有用户可见（ps/argv）
				// 且进 shell 历史；--admin-pass-env 不落这两个面
				fmt.Fprintln(os.Stderr, "warning: --admin-pass is exposed via process listings and shell history; prefer --admin-pass-env WDP_ADMIN_PASS")
			}
			if err := os.MkdirAll(dataDir, 0o700); err != nil {
				return err
			}
			// 库地址（--db）：URL 形式按 scheme 识别方言（postgres:// / mysql:// /
			// sqlite:// / file:），裸路径与 ":memory:" 沿用历史语义 = SQLite 文件。
			// 密码可写 ${VAR} 由环境变量注入，避免进命令行与 shell 历史。
			dbRaw := dbAddr
			if dbRaw == "" {
				dbRaw = filepath.Join(dataDir, "wdp.db")
			}
			dsn, err := store.ParseDSN(dbRaw, nil)
			if err != nil {
				return fmt.Errorf("invalid --db: %w", err)
			}
			// 数据目录排他锁：本地 SQLite 的单实例保护（SQLite 是单写者，
			// 双开会静默互踩）。网络库不用它——多实例并发由数据库自身的
			// 事务与约束保证，锁本地目录既无意义又会挡住合法的多副本部署。
			// 句柄保持到进程退出，锁随进程由内核回收——不 defer 关闭。
			if dsn.IsSQLite() {
				if _, err := datalock.Lock(dataDir); err != nil {
					return err
				}
				// SQLite 只建文件不建目录（默认路径的父目录随 --data 的
				// MkdirAll 已就位；显式 --db 的父目录在此补建）
				if dbAddr != "" {
					if dir := filepath.Dir(dsn.Config.Path); dir != "" && dir != "." {
						if err := os.MkdirAll(dir, 0o700); err != nil {
							return fmt.Errorf("create db directory: %w", err)
						}
					}
				}
			} else if dbAddr == "" {
				return fmt.Errorf("internal: network database needs an explicit --db")
			}
			st, err := store.OpenDSN(dsn)
			if err != nil {
				// 错误信息里的地址已遮密码（DSN.String 返回遮罩值）
				return fmt.Errorf("open store %s: %w", dsn.Redacted, err)
			}
			defer st.Close()
			if !dsn.IsSQLite() {
				fmt.Fprintf(os.Stderr, "wdp server: using %s database %s\n", st.DialectName(), st.Address())
			}
			srv, err := web.New(st, web.Options{
				Addr:           addr,
				AdminUser:      adminUser,
				AdminPass:      model.Secret(adminPass, adminPassEn),
				DataDir:        dataDir,
				CADir:          filepath.Join(dataDir, "ca"),
				CADays:         caDays,
				AdvertiseURL:   advertise,
				TrustedProxies: trustProxy,
				// 明文引导默认关闭：脚本以 root 执行，明文通道下中间人
				// 替换脚本即等于目标机 root
				AllowPlaintextEnroll: plainEnroll,
				TLSCert:              tlsCert,
				TLSKey:               tlsKey,
				// 保留策略：长期运行的库膨胀与敏感数据残留收口（0 = 永久保留）
				RunsRetentionDays:   runsDays,
				AuditRetentionDays:  auditDays,
				DraftsRetentionDays: draftsDays,
			}, nil)
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()
			return srv.Run(ctx)
		},
	}
	f := cmd.Flags()
	f.StringVar(&addr, "addr", "127.0.0.1:7603", i18n.T(
		"listen address (keep on loopback or front with a TLS reverse proxy)",
		"监听地址（保持在回环地址，或置于 TLS 反向代理之后）"))
	f.StringVar(&dbAddr, "db", "", i18n.T(
		"database address (default: <data>/wdp.db, a local SQLite file). Also accepts postgres://user:pass@host:5432/db, mysql://user:pass@host:3306/db, sqlite:///path, or a bare file path. Use ${VAR} for secrets: postgres://wdp:${WDP_DB_PASS}@host/db — the password then never appears in the command line or shell history",
		"数据库地址（缺省 <data>/wdp.db，即本地 SQLite 文件）。也接受 postgres://user:pass@host:5432/db、mysql://user:pass@host:3306/db、sqlite:///path 或裸文件路径。密码建议写 ${VAR} 由环境变量注入：postgres://wdp:${WDP_DB_PASS}@host/db —— 这样密码不进命令行与 shell 历史"))
	f.StringVar(&dataDir, "data", "wdp-data", i18n.T(
		"data directory holding wdp.db and the enrollment CA (prefer an absolute path; the relative default drifts with the working directory)",
		"存放 wdp.db 与纳管 CA 的数据目录（建议绝对路径；相对默认值随工作目录漂移）"))
	f.StringVar(&adminUser, "admin-user", "admin", "console admin account name")
	f.StringVar(&adminPass, "admin-pass", "", i18n.T(
		"admin password: ensures the account matches this value on every start (prefer -env)",
		"管理员密码：每次启动都确保账号密码与该值一致（建议改用 -env）"))
	f.StringVar(&adminPassEn, "admin-pass-env", "", i18n.T(
		"environment variable holding the admin password",
		"存放管理员密码的环境变量名"))
	f.StringVar(&advertise, "advertise", "", i18n.T(
		"externally reachable base URL for enrollment scripts (empty = derive from the request Host; must be https://)",
		"纳管脚本使用的外部可达基址（留空 = 从请求 Host 推导；须为 https://）"))
	f.BoolVar(&plainEnroll, "allow-plaintext-enroll", false, i18n.T(
		"allow handing out enroll commands over plaintext HTTP (trusted networks only; the script runs as root on the target)",
		"允许在明文 HTTP 下生成纳管命令（仅限可信网络；脚本在目标机以 root 执行）"))
	f.IntVar(&caDays, "ca-days", 3650, i18n.T(
		"enrollment CA validity in days (first-start bootstrap only)",
		"纳管 CA 有效期（天，仅首启自举时生效）"))
	f.StringSliceVar(&trustProxy, "trust-proxy", nil, i18n.T(
		"reverse-proxy IPs/CIDRs whose X-Forwarded-For is honored for audit/client IP (loopback is always trusted; direct clients cannot spoof it)",
		"可信反向代理的 IP/CIDR：其 X-Forwarded-For 用于审计/客户端 IP（回环地址始终可信；直连客户端无法伪造）"))
	f.StringVar(&tlsCert, "tls-cert", "", i18n.T(
		"console TLS certificate file (native TLS instead of a reverse proxy; pair with --tls-key)",
		"控制台 TLS 证书文件（用原生 TLS 替代反向代理；与 --tls-key 配对）"))
	f.StringVar(&tlsKey, "tls-key", "", "console TLS private key file")
	f.IntVar(&runsDays, "runs-retention-days", 90, i18n.T(
		"prune finished runs (with task details and script evidence) older than this many days (0 = keep forever)",
		"清理超过此天数的已完成 run（含任务明细与脚本证据；0 = 永久保留）"))
	f.IntVar(&auditDays, "audit-retention-days", 180, i18n.T(
		"prune audit log entries older than this many days (0 = keep forever)",
		"清理超过此天数的审计日志条目（0 = 永久保留）"))
	f.IntVar(&draftsDays, "drafts-retention-days", 30, i18n.T(
		"prune abandoned editor drafts older than this many days (0 = keep forever)",
		"清理超过此天数的废弃编辑器草稿（0 = 永久保留）"))
	return cmd
}
