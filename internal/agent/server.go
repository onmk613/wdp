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

	// 无认证启动, 一般纯内网安全环境
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
	// 整体原子换入换出：/cert 热更新时构造新对象 Store，握手中途不受影响
	material atomic.Pointer[tlsMaterial]

	// 服务实例（serve 写入、关停路径读取，原子避免竞态）
	httpSrv     atomic.Pointer[http.Server]
	selfBin     string
	systemdUnit string

	// 日志内核（initLogger 装配）：stderr + 可选文件 + 内存环形缓冲，
	// 控制端经 GET /logs 拉取近期日志（详见 log.go）
	logger   *slog.Logger
	logRing  *ringWriter
	sink     *logSink
	levelVar *slog.LevelVar
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
	s.initLogger()
	return s
}

// SetMaxRequestBody 设置请求体上限（MiB；<=0 回退内置默认 64MiB）
func (s *Server) SetMaxRequestBody(mb int64) {
	if mb <= 0 {
		mb = 64
	}
	s.maxRequestBody = mb << 20
}

// maxRequestBodyLimit 返回生效的请求体上限（字节）
func (s *Server) maxRequestBodyLimit() int64 {
	if s.maxRequestBody > 0 {
		return s.maxRequestBody
	}
	return 64 << 20
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

// SetIdleTimeout 设置空闲自动退出周期
func (s *Server) SetIdleTimeout(d time.Duration) {
	if d <= 0 {
		d = 0
	}
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
// 仅已通过外层认证的请求会走到这里（/health 直接放行不计时）。
func (s *Server) trackActivity(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.idleTimeout <= 0 || r.URL.Path == "/health" {
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
		// /health 仅含非敏感探测信息，放行
		if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
			if r.URL.Path == "/health" {
				next.ServeHTTP(w, r)
				return
			}
			http.Error(w, "client certificate required", http.StatusUnauthorized)
			return
		}
		sum := sha256.Sum256(r.TLS.PeerCertificates[0].Raw)
		fp := hex.EncodeToString(sum[:])
		if pins := s.material.Load().pins; pins != nil {
			if _, ok := pins[fp]; !ok && r.URL.Path != "/health" {
				http.Error(w, "client certificate not pinned", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// ListenAndServe 启动服务（阻塞）。mTLS 配置后以 TLS 启动（证书对经
// 回调读取当前材料快照，/cert 热更新无需重启）。
// 对外（非回环）监听且未配置 mTLS 时拒绝启动，除非 AllowNoAuth。
func (s *Server) ListenAndServe() error {
	ln, err := net.Listen("tcp", s.Listen)
	if err != nil {
		return err
	}
	return s.serve(ln)
}

// serve 在给定监听器上服务（阻塞；ListenAndServe 的可测内核：
// 测试可自建监听器断言空闲退出等行为）。返回 http.ErrServerClosed
// 表示被 /shutdown 或空闲看门狗正常关停。
func (s *Server) serve(ln net.Listener) error {
	if err := s.checkAuthSafety(); err != nil {
		return err
	}
	idle := "off"
	if s.idleTimeout > 0 {
		idle = s.idleTimeout.String()
	}
	s.logInfo("wdp agent %s listening on %s (mtls=%v, idle_timeout=%s)", Version, s.Listen, s.material.Load() != nil, idle)
	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
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
// 关停路径（push agent 带 --cleanup-on-shutdown，自然完成自删）。
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
// 在途保护——数小时的长任务执行期间不判空闲（否则击杀在途任务）。
func (s *Server) idleExpired(now time.Time) bool {
	if s.idleTimeout <= 0 {
		return false
	}
	if s.inflight.Load() > 0 {
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
