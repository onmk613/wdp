package agent

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// Version 是 agent 协议版本
const Version = "v1"

// Server 是 agent HTTP 服务配置
type Server struct {
	Listen string

	// CA file
	tlsCAFile   string
	tlsCertFile string
	tlsKeyFile  string

	// 无认证模式：仅适合隔离环境，见 serve() 内告警
	allowNoAuth bool
	// 是否自清理
	cleanupOnShutdown atomic.Bool
	// 空闲退出时长
	idleTimeout time.Duration
	// 最后一次已认证请求完成时刻（UnixNano）
	lastActive atomic.Int64
	// 当前正在请求的已认证请求数（空闲判定须为零，防长任务被误杀）
	inflight atomic.Int64

	// 请求体上限字节
	maxRequestBody int64
	// middlewares 是外部追加的中间件
	middlewares []func(http.Handler) http.Handler

	// material 是当前 mTLS 材料（服务端证书对 + 客户端 CA 池 + 指纹名单），
	// 整体原子换入换出：构造新对象 Store，握手中途不受影响。
	// 材料在启动时装配；运行期经 POST /cert 热更换（控制端 Renew 重签推送，
	// 见 cert.go——换证不再需要重装/重启 agent）。
	material atomic.Pointer[tlsMaterial]

	// certMu 串行化 /cert 换证：并发推送时避免落盘与材料换入交错
	certMu sync.Mutex

	// 服务实例（serve 写入、关停路径读取，原子避免竞态）
	httpSrv     atomic.Pointer[http.Server]
	selfBin     string
	systemdUnit string

	// 日志内核（initLogger 装配）：stderr + 可选文件 + 内存环形缓冲，
	// 控制端经 GET /logs 拉取近期日志（详见 log.go）
	logger  *slog.Logger
	logRing *ringWriter
	sink    *logSink
	// logFilePath 已挂载的日志文件路径（SetLogFile 防重挂：重复挂同一
	// 文件会让每行日志写两遍）
	logFilePath string
	levelVar    *slog.LevelVar

	// 自治执行（POST /plan）：同刻至多一个 running run；running 期间抑制
	// 空闲退出（没有请求到达不应导致收敛被杀，§7.5 必查项）
	plans      *planManager
	planActive atomic.Bool
	runsDir    string // 自治执行持久化根（空 = 内置默认 /var/lib/wdp/runs）
}

// tlsMaterial 是一次生效的 mTLS 材料快照
type tlsMaterial struct {
	cert      tls.Certificate     // 服务端证书对
	leaf      *x509.Certificate   // 服务端证书叶子（/health 到期时间用）
	clientCAs *x509.CertPool      // 客户端证书 CA 池
	pins      map[string]struct{} // 客户端指纹准许名单（nil = 不限制）
}

// New 创建 agent 服务（listen 为空时回环默认地址）
func New(listen string) *Server {
	s := &Server{}
	if listen == "" {
		listen = "127.0.0.1:7602"
	}
	s.Listen = listen
	s.cleanupOnShutdown.Store(true)
	exe, _ := os.Executable()
	s.selfBin = exe
	s.systemdUnit = "wdp-agent"
	s.plans = newPlanManager()
	s.initLogger()
	return s
}

// SetMaxRequestBody 设置请求体上限（MiB；<=0 回退内置默认 512MiB）。
// 默认档覆盖常见离线制品（docker 静态包 ~80MB、JDK ~200MB）——上传是
// 流式原子落盘（fsatomic），不整包进内存，上限防的是磁盘被无界 body
// 写满，512MiB 是「够大而有界」的折中
func (s *Server) SetMaxRequestBody(mb int64) {
	if mb <= 0 {
		mb = 512
	}
	s.maxRequestBody = mb << 20
}

// maxRequestBodyLimit 返回生效的请求体上限（字节）
func (s *Server) maxRequestBodyLimit() int64 {
	if s.maxRequestBody > 0 {
		return s.maxRequestBody
	}
	return 512 << 20
}

// Use 追加 middleware
func (s *Server) Use(mw func(http.Handler) http.Handler) {
	s.middlewares = append(s.middlewares, mw)
}

// CleanupOnShutdown 设置关停时是否自清理
func (s *Server) CleanupOnShutdown(on bool) {
	s.cleanupOnShutdown.Store(on)
}

// AllowNoAuth 设置是否允许无认证启动
func (s *Server) AllowNoAuth(on bool) {
	s.allowNoAuth = on
}

// SetIdleTimeout 设置空闲自动退出周期（<=0 表示关闭）。
func (s *Server) SetIdleTimeout(d time.Duration) {
	s.idleTimeout = d
}

// SetSystemdUnit 设置远程清理 all 时停用的 systemd 单元名
func (s *Server) SetSystemdUnit(unit string) {
	if unit == "" {
		unit = "wdp-agent"
	}
	s.systemdUnit = unit
}

// trackActivity 维护空闲判定的两个信号：请求开始/完成时间戳与在途计数。
// 仅已通过外层认证的请求会走到这里（探测端点直接放行不计时）。
func (s *Server) trackActivity(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.idleTimeout <= 0 || isProbePath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		s.lastActive.Store(time.Now().UnixNano())
		s.inflight.Add(1)
		defer func() {
			s.inflight.Add(-1)
			s.lastActive.Store(time.Now().UnixNano())
		}()
		next.ServeHTTP(w, r)
	})
}

// pinMiddleware 校验客户端证书指纹在准许名单内（mTLS 模式生效）。
func (s *Server) pinMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// /health 与 /info 仅含非敏感探测/能力信息，放行
		if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
			if isProbePath(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}
			http.Error(w, "client certificate required", http.StatusUnauthorized)
			return
		}
		leaf := r.TLS.PeerCertificates[0]
		sum := sha256.Sum256(leaf.Raw)
		fp := hex.EncodeToString(sum[:])
		// 公钥（SPKI）指纹同样接受：证书续期（保留密钥对）后 DER 变了、
		// SPKI 不变，控制台例行续证因此不会把整片 agent 打成不可达。
		spki := sha256.Sum256(leaf.RawSubjectPublicKeyInfo)
		spkiFP := hex.EncodeToString(spki[:])
		if pins := s.material.Load().pins; pins != nil {
			_, certOK := pins[fp]
			_, keyOK := pins[spkiFP]
			if !certOK && !keyOK && !isProbePath(r.URL.Path) {
				http.Error(w, "client certificate not pinned", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// isProbePath 报告路径是否为免认证探测端点（/health 存活探测与 /info
// 能力自描述——两者都只暴露版本/模块名级信息，且探活链路在明文回退
// 场景下无客户端证书可用）。
func isProbePath(p string) bool {
	return p == "/health" || p == "/info"
}

// ListenAndServe 启动服务（阻塞）。mTLS 配置后以 TLS 启动（证书对经
// 回调读取当前材料快照；运行期可经 POST /cert 热更换，无需重启）。
// 对外（非回环）监听且未配置 mTLS 时拒绝启动，除非 AllowNoAuth。
func (s *Server) ListenAndServe() error {
	ln, err := net.Listen("tcp", s.Listen)
	if err != nil {
		return err
	}
	return s.serve(ln)
}

// Serve 在给定监听器上服务（阻塞；外部编排场景与测试注入用）。
func (s *Server) Serve(ln net.Listener) error { return s.serve(ln) }

// serve 在给定监听器上服务（阻塞；ListenAndServe 的可测内核：
// 测试可自建监听器断言空闲退出等行为）。返回 http.ErrServerClosed
// 表示被 /shutdown 或空闲看门狗正常关停。
func (s *Server) serve(ln net.Listener) error {
	if err := s.checkAuthSafety(); err != nil {
		return err
	}
	// 僵尸 run 收割必须先于任何请求：此刻 planManager 为空，磁盘上的
	// running 状态必然没有进程支撑（上一进程崩溃/被杀留下的），此后
	// 新提交的 run 才是本进程真实持有的
	s.reapZombieRuns()
	idle := "off"
	if s.idleTimeout > 0 {
		idle = s.idleTimeout.String()
	}
	s.logInfo("wdp agent %s listening on %s (mtls=%v, idle_timeout=%s)", Version, s.Listen, s.material.Load() != nil, idle)
	if s.material.Load() == nil {
		// 无 mTLS 运行（回环默认或 --allow-no-auth）。回环不是安全边界：
		// 同机**所有用户**的进程都能连上 127.0.0.1，以 agent 身份（常为
		// root）执行任意命令——共享主机/多用户服务器上等于本地提权。
		// 高亮告警；文档口径为仅限单用户/专用主机。
		s.logWarn("WARNING: running WITHOUT authentication on %s — any local process can execute commands as this agent; use mTLS (--ca/--cert/--key) unless this is a single-user machine", s.Listen)
	}
	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// 读写超时保持不限：大文件上传（/upload）与长任务 exec 的响应都可能
		// 远超任何固定超时，超时策略由各 handler 的 LimitReader 与任务级
		// timeout 控制；空闲连接与请求头大小必须收紧（连接/内存资源耗尽）。
		IdleTimeout:    120 * time.Second,
		MaxHeaderBytes: 1 << 20,
	}
	s.httpSrv.Store(srv)
	go s.watchIdle()
	if s.material.Load() != nil {
		srv.TLSConfig = s.MTLSConfig()
		// 证书经 GetCertificate 回调提供，无需文件路径
		return srv.ServeTLS(ln, "", "")
	}
	return srv.Serve(ln)
}

// watchIdle 空闲看门狗：周期检查 idleExpired，触发即走 /shutdown 同款
// 关停路径（带 --cleanup-on-shutdown 的 agent 自然完成自删）。
func (s *Server) watchIdle() {
	if s.idleTimeout <= 0 {
		return
	}
	s.lastActive.Store(time.Now().UnixNano())
	tick := max(min(s.idleTimeout/4, 30*time.Second), 100*time.Millisecond)
	t := time.NewTicker(tick)
	defer t.Stop()
	for range t.C {
		if s.idleExpired(time.Now()) {
			s.logInfo("idle for over %s with no authenticated request, exiting (cleanup-on-shutdown=%v)", s.idleTimeout, s.cleanupOnShutdown.Load())
			s.initiateShutdown(shutdownReq{}, s.cleanupOnShutdown.Load())
			return
		}
	}
}

// idleExpired 空闲判定：无在途已认证请求，且距最后一次完成超过周期。
// 在途保护——数小时的长任务执行期间不判空闲（否则击杀在途任务）；
// 自治执行收敛期间同样不判空闲（后台执行没有请求到达，§7.5）。
func (s *Server) idleExpired(now time.Time) bool {
	if s.idleTimeout <= 0 {
		return false
	}
	if s.inflight.Load() > 0 {
		return false
	}
	if s.planIdleActive() {
		return false
	}
	return now.Sub(time.Unix(0, s.lastActive.Load())) >= s.idleTimeout
}

// IsLoopbackListen 判断监听地址是否仅绑定回环（localhost / 127.0.0.1 / ::1）。
// 空主机名（":7602"）与 "0.0.0.0:7602" 监听全部网卡，返回 false。
func IsLoopbackListen(listen string) bool {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// checkAuthSafety 拒绝不安全的启动配置：/exec、/file 提供的是 root 级
// 远程命令执行与任意文件读写，对外监听必须配置 mTLS。
// 回环监听视为仅本机可访问，允许无认证（便于本地调试）。
func (s *Server) checkAuthSafety() error {
	if s.material.Load() != nil || s.allowNoAuth || IsLoopbackListen(s.Listen) {
		return nil
	}
	return fmt.Errorf("%s",
		"refusing to start: listening on "+s.Listen+" without auth exposes remote code execution; "+
			"configure mTLS (--ca/--cert/--key), or pass --allow-no-auth on a trusted network")
}
