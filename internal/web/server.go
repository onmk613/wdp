package web

// Server 的构造与生命周期：Options/Server 类型、New（启动对账/服务装配/
// 路由）、Run（监听与关停）。会话见 session.go，鉴权见 authmw.go，路由
// 表见 routes.go，HTTP 工具见 httpx.go。

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"wdp/internal/agentbin"
	"wdp/internal/console"
	"wdp/internal/store"
	"wdp/internal/worker"

	"golang.org/x/crypto/bcrypt"
)

type Options struct {
	Addr         string        // 监听地址（默认 127.0.0.1:7603）
	AdminUser    string        // 首启引导的管理员名（默认 admin）
	AdminPass    string        // 首启引导的管理员密码（空则随机生成并打印一次）
	ProbeEvery   time.Duration // 探活周期（默认 30s）
	DataDir      string        // 数据目录（应用制品存 <data>/apps）
	CADir        string        // server 信任链目录（空 = 不启用纳管，仅手工台账）
	CADays       int           // 首启自举 CA 有效期天（默认 3650）
	AdvertiseURL string        // 纳管脚本回连 server 的外部基址（空 = 按请求 Host 推导）
	// TLSCert/TLSKey 是控制台原生 TLS 的证书对（两者须同时提供；空 = 明文
	// HTTP，默认仍面向"回环监听或前置 TLS 反代"的部署形态）。证书可用
	// `wdp ca issue` 签发（SAN 填对外域名/IP）。
	TLSCert string
	TLSKey  string
	// TrustedProxies 是可采信 X-Forwarded-For 的反代地址（IP 或 CIDR）。
	// 回环恒在信任列表（本机反代是常见拓扑）；列表之外的直连请求一律取
	// 真实对端地址——XFF 可被客户端任意伪造，无条件采信会让审计 IP 与
	// 纳管 ClaimAddress/证书 SAN 全部可伪造。
	TrustedProxies []string
	// AllowPlaintextEnroll 允许在明文 HTTP 下下发纳管脚本。默认关闭：
	// 脚本以 root 执行，明文通道下中间人替换脚本即等于目标机 root，
	// 脚本内嵌的 CA 指纹与脚本同源同通道、对主动 MITM 无价值。仅在
	// 可信内网/离线演示时显式打开。
	AllowPlaintextEnroll bool
}

// Server 是控制台 HTTP 服务。
type Server struct {
	st             *store.Store
	opts           Options
	sessions       sessionTable
	mux            *http.ServeMux
	logger         *slog.Logger
	cam            *caMaterial
	binResolver    func(platform string) (string, bool)
	gate           *hostGate            // per-host 执行闸门（run/exec/升级互斥）
	monitor        *worker.Monitor      // 指标采样器（差分快照归它持有；主机删除时 Forget）
	apps           *console.AppService  // 应用领域服务（spec 物化/打包；见 internal/console）
	runs           *runHub              // 执行事件扇出（SSE 订阅端见 runevents.go）
	runsvc         *console.RunService  // 应用执行编排（见 internal/console/run.go）
	execsvc        *console.ExecService // 远程命令执行（见 internal/console/exec.go）
	bgCtx          context.Context      // Run 注入的生命周期 ctx（后台 run goroutine 挂钩关停；测试直连 Handler 时为 nil）
	permMu         sync.RWMutex         // 权限视图缓存
	permCache      map[string]*userPerms
	loginMu        sync.Mutex               // 登录失败限速表
	loginFails     map[string]*loginAttempt // 限速键 → 失败计数/锁定截止；键族含 ip|、user|、basic| 前缀（构造见 authmw.go handleLogin/basicAuthUser）
	uploadMu       sync.Mutex               // 上传互斥：版本预检→制品归位→入库整体临界区（防 TOCTOU 覆盖/误删）
	trustedProxies []*net.IPNet             // 可采信 XFF 的对端（含回环；见 Options.TrustedProxies）
	metrics        httpMetrics              // 请求计数/耗时观测（observability.go）
}

// sessionTable 内存会话表：token → {用户, 过期时刻}（滑动窗口，空闲 2h
// 失效）。server 单进程运行，重启即全量失效（可接受：重新登录）。
type sessionTable struct {
	mu     sync.Mutex
	sessns map[string]session
}

type session struct {
	user   string
	exp    time.Time
	issued time.Time // 签发时刻：绝对生命周期上限的锚点
}

const sessionTTL = 2 * time.Hour

// sessionMaxTTL 是会话的绝对生命周期上限：滑动续期只延长空闲过期，
// 无绝对上限时 token 泄露后只要被持续使用就永不过期（有管理端可踢，
// 但不应依赖人工发现）。
const sessionMaxTTL = 12 * time.Hour

func New(st *store.Store, opts Options, logger *slog.Logger) (*Server, error) {
	if opts.Addr == "" {
		opts.Addr = "127.0.0.1:7603"
	}
	if opts.AdminUser == "" {
		opts.AdminUser = "admin"
	}
	if opts.ProbeEvery <= 0 {
		opts.ProbeEvery = 30 * time.Second
	}
	if logger == nil {
		logger = slog.Default()
	}
	trusted, err := parseTrustedProxies(opts.TrustedProxies)
	if err != nil {
		return nil, err
	}
	if (opts.TLSCert == "") != (opts.TLSKey == "") {
		return nil, errors.New("TLS requires both cert and key files")
	}
	if err := bootstrapAdmin(st, &opts, logger); err != nil {
		return nil, err
	}
	// 管理员账号显式标记角色（迁移前旧行 role=''，首次启动补齐）
	if u, err := st.UserByName(opts.AdminUser); err == nil && u.Role != "admin" {
		_ = st.SetUserRole(opts.AdminUser, "admin")
	}
	// 启动对账：上个进程留下的 queued/running run 已没有任何执行者，
	// 不收口就会永久悬空（曾出现重启后执行记录永远停在 running）。
	// 执行是至多一次语义：重启即标 failed 并注明原因，由人重新发起。
	if n, err := st.ReconcileStaleRuns("server 重启：执行中断（结果未知，请核对目标状态后重新发起）"); err != nil {
		return nil, fmt.Errorf("reconcile stale runs: %w", err)
	} else if n > 0 {
		logger.Warn("reconciled stale runs from previous process", "count", n)
	}
	s := &Server{st: st, opts: opts, sessions: sessionTable{sessns: map[string]session{}},
		mux: http.NewServeMux(), logger: logger, binResolver: agentbin.SiblingPath,
		gate: newHostGate(), permCache: map[string]*userPerms{}, loginFails: map[string]*loginAttempt{},
		trustedProxies: trusted}
	s.routes()
	s.monitor = &worker.Monitor{Store: st, Logger: logger, Fetch: s.fetchAgentMetrics}
	s.apps = &console.AppService{Store: st, DataDir: opts.DataDir, Logger: logger}
	s.runs = newRunHub()
	s.runsvc = &console.RunService{Store: st, Logger: logger}
	s.execsvc = &console.ExecService{Store: st, HostModel: s.agentHostModelWithScheme}
	if opts.CADir != "" {
		cam, err := bootstrapCA(opts.CADir, opts.CADays)
		if err != nil {
			return nil, err
		}
		s.cam = cam
		s.routesEnroll()
	}
	return s, nil
}

// parseTrustedProxies 解析信任代理配置为 CIDR 列表：回环恒在（本机反代
// 常见拓扑），显式条目追加其后；裸 IP 自动补 /32 或 /128。
func parseTrustedProxies(extra []string) ([]*net.IPNet, error) {
	entries := append([]string{"127.0.0.0/8", "::1/128"}, extra...)
	var out []*net.IPNet
	for _, e := range entries {
		if !strings.Contains(e, "/") {
			if ip := net.ParseIP(e); ip != nil {
				if ip.To4() != nil {
					e += "/32"
				} else {
					e += "/128"
				}
			} else {
				return nil, fmt.Errorf("invalid trusted proxy %q (IP or CIDR expected)", e)
			}
		}
		_, n, err := net.ParseCIDR(e)
		if err != nil {
			return nil, fmt.Errorf("invalid trusted proxy %q: %w", e, err)
		}
		out = append(out, n)
	}
	return out, nil
}

// bootstrapAdmin 管理员账号引导：
//   - 显式配置密码（--admin-pass/-env）：确保该账号存在且密码与之一致
//     （幂等：已匹配则不动；改值后重启即改密）
//   - 未配置：仅首启（库中无账号）创建，密码随机生成并打印一次
func bootstrapAdmin(st *store.Store, opts *Options, logger *slog.Logger) error {
	if opts.AdminPass != "" {
		if u, err := st.UserByName(opts.AdminUser); err == nil {
			if bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(opts.AdminPass)) == nil {
				logger.Info("console admin account", "user", opts.AdminUser, "password", "matches the configured value (unchanged)")
				return nil
			}
			hash, err := bcrypt.GenerateFromPassword([]byte(opts.AdminPass), bcryptCost)
			if err != nil {
				return err
			}
			if err := st.SetUserPassword(opts.AdminUser, string(hash)); err != nil {
				return err
			}
			logger.Info("console admin password reset from the configured value", "user", opts.AdminUser)
			return nil
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		hash, err := bcrypt.GenerateFromPassword([]byte(opts.AdminPass), bcryptCost)
		if err != nil {
			return err
		}
		return st.SetUserPassword(opts.AdminUser, string(hash))
	}
	// 未配置：仅首启生成
	n, err := st.CountUsers()
	if err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		return err
	}
	pass := hex.EncodeToString(buf)
	fmt.Printf("[wdp server] created admin %q with generated password: %s (save it now; it is not shown again)\n", opts.AdminUser, pass)
	hash, err := bcrypt.GenerateFromPassword([]byte(pass), bcryptCost)
	if err != nil {
		return err
	}
	return st.CreateUser(opts.AdminUser, string(hash), "admin")
}

// Handler 返回对外的完整处理器链（含指标中间件），与 Run 监听的完全一致。
// 测试必须走这里——曾因测试直连裸 mux、生产包了中间件，导致 SSE 依赖的
// http.Flusher 在生产被中间件吞掉却全量测试绿灯。
func (s *Server) Handler() http.Handler { return s.securityHeaders(s.metricsMiddleware(s.mux)) }

// muxOnly 返回未包中间件的裸路由（仅供需要观察原始 ResponseWriter 的
// 极端用例；业务测试一律用 Handler）。
func (s *Server) muxOnly() http.Handler { return s.mux }

// Addr 返回监听地址。
func (s *Server) Addr() string { return s.opts.Addr }

// Run 启动探活循环与 HTTP 监听（阻塞至 ctx 取消）。
func (s *Server) Run(ctx context.Context) error {
	s.bgCtx = ctx
	s.startProber(ctx)
	s.startMonitor(ctx)
	go s.sessionSweepLoop(ctx)
	// 超时只设读头与空闲：WriteTimeout 必须留 0——远程命令/升级是同步
	// 长请求（脚本可跑数分钟），写超时会掐断它们；ReadHeaderTimeout 防
	// 慢速连接攻击（slowloris），IdleTimeout 及时回收空闲连接
	srv := &http.Server{
		Addr:              s.opts.Addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	ln, err := net.Listen("tcp", s.opts.Addr)
	if err != nil {
		return err
	}
	s.logger.Info("wdp server listening", "addr", ln.Addr().String(), "tls", s.opts.TLSCert != "")
	errCh := make(chan error, 1)
	if s.opts.TLSCert != "" {
		// 控制台原生 TLS（--tls-cert/--tls-key）：无反代部署时的明文消除
		// 项。证书对路径为空两项校验已在 New 完成。
		go func() { errCh <- srv.ServeTLS(ln, s.opts.TLSCert, s.opts.TLSKey) }()
	} else {
		go func() { errCh <- srv.Serve(ln) }()
	}
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	case err := <-errCh:
		return err
	}
}
