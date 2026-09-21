package cli

// wdp server：Web 控制台（web-console 方向的组合根）。数据落本地 SQLite
// （默认 ./wdp-data/wdp.db），首启自动引导管理员账号；主机纳管、远程
// 执行与 chart 管理随后续版本在此命令上扩展。

import (
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/spf13/cobra"

	"wdp/internal/datalock"
	"wdp/internal/model"
	"wdp/internal/store"
	"wdp/internal/web"
)

const serverHelp = `
启动 Web 控制台（主机台账 · 探活 · 纳管）

数据落本地 SQLite（--data 目录下 wdp.db，纯 Go 驱动，零外部依赖）；
管理员账号（--admin-user，默认 admin）：显式给出 --admin-pass /
--admin-pass-env 时，启动即确保账号密码与之一致（改值后重启即改密，
幂等）；均未给出则仅首启随机生成并打印一次。首启同时自举纳管信任链
（<data>/ca：根 CA 默认 10 年 + 控制端客户端证书）。

主机纳管：控制台生成一次性 token（默认 30 分钟有效），在目标机执行
下发的一键命令即完成安装——脚本按目标机架构从 server 拉取 wdp 二进制
（server 须运行在 build.sh 产出的多架构 bin 目录中）、领取逐主机证书
并装配 systemd 常驻 agent，全程 mTLS。

监听默认 127.0.0.1:7603——目标机需要回连时用 --addr 0.0.0.0 并配
--advertise 指定外部可达基址；公网暴露请置于反向代理 TLS 之后，或用
--tls-cert/--tls-key 启用控制台原生 TLS（证书可用 wdp ca issue 签发）。

--data 建议绝对路径（默认相对路径 wdp-data 随启动目录漂移）；
--admin-pass 经命令行传参对同机用户可见（ps）且会进 shell 历史，建议改用
--admin-pass-env。根 CA 私钥默认明文（0600）落盘；设 WDP_CA_PASS 环境变量
后首启自举的根 CA 私钥以口令加密（此后每次启动都需同一环境变量）。

示例：
wdp server                                        # 本机体验
wdp server --addr 0.0.0.0:7603 --advertise http://10.0.0.5:7603 --admin-pass-env WDP_ADMIN_PASS
wdp server --addr 0.0.0.0:7603 --tls-cert /etc/wdp/console.crt --tls-key /etc/wdp/console.key
`

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
		trustProxy  []string
		tlsCert     string
		tlsKey      string
	)
	cmd := &cobra.Command{
		Use:   "server",
		Short: "run the web console (host inventory, probing & enrollment)",
		Long:  serverHelp,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if adminPass != "" {
				// 保留兼容但醒目告警：命令行参数对同机所有用户可见（ps/argv）
				// 且进 shell 历史；--admin-pass-env 不落这两个面
				fmt.Fprintln(os.Stderr, "warning: --admin-pass is exposed via process listings and shell history; prefer --admin-pass-env WDP_ADMIN_PASS")
			}
			if err := os.MkdirAll(dataDir, 0o700); err != nil {
				return err
			}
			// 数据目录排他锁：server 的全部状态（SQLite/CA/制品/内存会话）
			// 都是单实例假设，双开会静默互踩。句柄保持到进程退出，锁随
			// 进程由内核回收——不 defer 关闭
			if _, err := datalock.Lock(dataDir); err != nil {
				return err
			}
			st, err := store.Open(filepath.Join(dataDir, "wdp.db"))
			if err != nil {
				return fmt.Errorf("open store: %w", err)
			}
			defer st.Close()
			srv, err := web.New(st, web.Options{
				Addr:           addr,
				AdminUser:      adminUser,
				AdminPass:      model.Secret(adminPass, adminPassEn),
				DataDir:        dataDir,
				CADir:          filepath.Join(dataDir, "ca"),
				CADays:         caDays,
				AdvertiseURL:   advertise,
				TrustedProxies: trustProxy,
				TLSCert:        tlsCert,
				TLSKey:         tlsKey,
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
	f.StringVar(&addr, "addr", "127.0.0.1:7603", "listen address (keep on loopback or front with a TLS reverse proxy)")
	f.StringVar(&dataDir, "data", "wdp-data", "data directory holding wdp.db and the enrollment CA (prefer an absolute path; the relative default drifts with the working directory)")
	f.StringVar(&adminUser, "admin-user", "admin", "console admin account name")
	f.StringVar(&adminPass, "admin-pass", "", "admin password: ensures the account matches this value on every start (prefer -env)")
	f.StringVar(&adminPassEn, "admin-pass-env", "", "environment variable holding the admin password")
	f.StringVar(&advertise, "advertise", "", "externally reachable base URL for enrollment scripts (empty = derive from the request Host)")
	f.IntVar(&caDays, "ca-days", 3650, "enrollment CA validity in days (first-start bootstrap only)")
	f.StringSliceVar(&trustProxy, "trust-proxy", nil, "reverse-proxy IPs/CIDRs whose X-Forwarded-For is honored for audit/client IP (loopback is always trusted; direct clients cannot spoof it)")
	f.StringVar(&tlsCert, "tls-cert", "", "console TLS certificate file (native TLS instead of a reverse proxy; pair with --tls-key)")
	f.StringVar(&tlsKey, "tls-key", "", "console TLS private key file")
	return cmd
}
