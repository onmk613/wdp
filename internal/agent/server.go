// Package agent 是部署在目标机上的常驻 HTTP 服务，
// 为控制端提供 exec / 上传 / 下载原语。
//
// 认证两种模式（启动参数选择）：
//   - mTLS：--ca/--cert/--key 双向证书认证
//   - 无认证：默认仅允许监听回环地址；对外（非回环）监听且未配置 mTLS 时
//     拒绝启动，除非显式 --allow-no-auth（仅限可信内网）
package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"time"

	"wdp/internal/ca"
	"wdp/internal/i18n"
	"wdp/internal/shellquote"
)

// Version 是 agent 协议版本。
const Version = "1"

// Server 是 agent HTTP 服务。
type Server struct {
	Listen      string
	middlewares []func(http.Handler) http.Handler

	tlsCAFile         string
	tlsCertFile       string
	tlsKeyFile        string
	clientCAs         *x509.CertPool
	clientPins        map[string]struct{} // 客户端证书 SHA256 指纹准许名单（空 = 不限制）
	allowNoAuth       bool                // 显式允许无认证对外监听（仅限可信内网）
	cleanupOnShutdown atomic.Bool
	httpSrv           *http.Server
}

// New 创建 agent 服务。
func New(listen string) *Server {
	return &Server{Listen: listen}
}

// Use 追加 middleware。
func (s *Server) Use(mw func(http.Handler) http.Handler) {
	s.middlewares = append(s.middlewares, mw)
}

// ConfigureAuth 配置 mTLS 认证：ca 校验客户端证书、cert/key 为服务端证书。
func (s *Server) ConfigureAuth(ca, cert, key string) error {
	if ca != "" || cert != "" || key != "" {
		if ca == "" || cert == "" || key == "" {
			return fmt.Errorf("mTLS 需要 --ca/--cert/--key 同时提供")
		}
		pool := x509.NewCertPool()
		caPEM, err := os.ReadFile(ca)
		if err != nil {
			return fmt.Errorf("读取 CA 证书失败: %w", err)
		}
		if !pool.AppendCertsFromPEM(caPEM) {
			return fmt.Errorf("解析 CA 证书失败: %s", ca)
		}
		s.clientCAs = pool
		s.tlsCAFile, s.tlsCertFile, s.tlsKeyFile = ca, cert, key
	}
	return nil
}

// CleanupOnShutdown 设置收到 /shutdown 时删除自身二进制与 mTLS 材料文件
// （证书/私钥启动时已载入内存，删除不影响运行；push 临时 agent 场景防残留）。
func (s *Server) CleanupOnShutdown(on bool) { s.cleanupOnShutdown.Store(on) }

// AllowNoAuth 显式允许无认证对外监听（仅限可信内网场景；
// 默认拒绝，防止 agent 的 root 命令执行原语被同网段任意访问）。
func (s *Server) AllowNoAuth(on bool) { s.allowNoAuth = on }

// PinClientFingerprints 设置客户端证书指纹准许名单（精确吊销：
// 从名单移除指纹并重启 agent 即可拒收某张证书，无需换 CA）。
// 空切片 = 不限制（任何 CA 签发的有效客户端证书均可）。指纹格式见 ca.ParsePin。
func (s *Server) PinClientFingerprints(pins []string) error {
	if len(pins) == 0 {
		s.clientPins = nil
		return nil
	}
	m := make(map[string]struct{}, len(pins))
	for _, p := range pins {
		norm, err := ca.ParsePin(p)
		if err != nil {
			return err
		}
		m[norm] = struct{}{}
	}
	s.clientPins = m
	return nil
}

// Handler 返回最终 HTTP 处理器。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("POST /exec", s.handleExec)
		mux.HandleFunc("PUT /file", s.handleUpload)
		mux.HandleFunc("GET /file", s.handleDownload)
		mux.HandleFunc("POST /archive", s.handleArchive)
		mux.HandleFunc("POST /shutdown", s.handleShutdown)

	var h http.Handler = mux
	if s.clientPins != nil {
		h = s.pinMiddleware(h)
	}
	for i := len(s.middlewares) - 1; i >= 0; i-- {
		h = s.middlewares[i](h)
	}
	return h
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
		if _, ok := s.clientPins[fp]; !ok && r.URL.Path != "/health" {
			http.Error(w, "client certificate not pinned", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ListenAndServe 启动服务（阻塞）。mTLS 配置后以 TLS 启动。
// 对外（非回环）监听且未配置 mTLS 时拒绝启动，除非 AllowNoAuth。
func (s *Server) ListenAndServe() error {
	if err := s.checkAuthSafety(); err != nil {
		return err
	}
	s.httpSrv = &http.Server{
		Addr:              s.Listen,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	if s.clientCAs != nil {
		s.httpSrv.TLSConfig = &tls.Config{
			ClientCAs:  s.clientCAs,
			ClientAuth: tls.RequireAndVerifyClientCert,
			MinVersion: tls.VersionTLS12,
		}
		return s.httpSrv.ListenAndServeTLS(s.tlsCertFile, s.tlsKeyFile)
	}
	return s.httpSrv.ListenAndServe()
}

// checkAuthSafety 拒绝不安全的启动配置：/exec、/file 提供的是 root 级
// 远程命令执行与任意文件读写，对外监听必须配置 mTLS。
// 回环监听视为仅本机可访问，允许无认证（便于本地调试）。
func (s *Server) checkAuthSafety() error {
	if s.clientCAs != nil || s.allowNoAuth || IsLoopbackListen(s.Listen) {
		return nil
	}
	return fmt.Errorf("%s",
		i18n.T("refusing to start: listening on "+s.Listen+" without auth exposes remote code execution; "+
			"configure mTLS (--ca/--cert/--key), or pass --allow-no-auth on a trusted network",
			"拒绝启动："+s.Listen+" 对外监听且未配置认证，等于暴露远程命令执行；"+
				"请配置 mTLS（--ca/--cert/--key），可信内网可显式 --allow-no-auth"))
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

type healthResp struct {
	Ok       bool   `json:"ok"`
	Version  string `json:"version"`
	Hostname string `json:"hostname"`
	Goos     string `json:"goos"`
	Arch     string `json:"arch"`
	Pid      int    `json:"pid"`
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	host, _ := os.Hostname()
	writeJSON(w, http.StatusOK, healthResp{
		Ok: true, Version: Version, Hostname: host,
		Goos: runtime.GOOS, Arch: runtime.GOARCH, Pid: os.Getpid(),
	})
}

type execReq struct {
	Script         string            `json:"script"`
	Stdin          string            `json:"stdin"`
	Env            map[string]string `json:"env"`
	TimeoutMs      int64             `json:"timeout_ms"`
	Cwd            string            `json:"cwd"`
	BecomeUser     string            `json:"become_user"`
	BecomePassword string            `json:"become_password"`
}

type execResp struct {
	Code     int    `json:"code"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	TimedOut bool   `json:"timed_out"`
}

func (s *Server) handleExec(w http.ResponseWriter, r *http.Request) {
	var req execReq
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, execResp{Code: -1, Stderr: "请求体解析失败: " + err.Error()})
		return
	}
	if req.Script == "" {
		writeJSON(w, http.StatusBadRequest, execResp{Code: -1, Stderr: "script 为空"})
		return
	}

	// 提权：sudo -u（-n 免密；-S 密码经 stdin 传递，不进命令行，ps 不可见）
	script, stdin := becomeScript(req.Script, req.BecomeUser, req.BecomePassword, req.Stdin)

	ctx := r.Context()
	var cancel context.CancelFunc
	if req.TimeoutMs > 0 {
		ctx, cancel = context.WithTimeout(ctx, time.Duration(req.TimeoutMs)*time.Millisecond)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", script)
	cmd.Dir = req.Cwd
	if cmd.Dir == "" {
		cmd.Dir = "/"
	}
	env := os.Environ()
	for k, v := range req.Env {
		env = append(env, k+"="+v)
	}
	cmd.Env = env
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()

	resp := execResp{Stdout: stdout.String(), Stderr: stderr.String()}
	if ctxErr := ctx.Err(); ctxErr != nil && err != nil {
		resp.TimedOut = true
		resp.Code = -1
		resp.Stderr += "\n[wdp-agent] 执行超时被终止"
	} else if err != nil {
		resp.Code = 1
		if ee, ok := err.(*exec.ExitError); ok {
			resp.Code = ee.ExitCode()
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" {
		http.Error(w, "缺少 path 参数", http.StatusBadRequest)
		return
	}
	mode := fs.FileMode(0o644)
	if m := r.URL.Query().Get("mode"); m != "" {
		var n int64
		if _, err := fmt.Sscanf(m, "%o", &n); err == nil {
			mode = fs.FileMode(n).Perm()
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		http.Error(w, "创建目录失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".wdp-agent-*")
	if err != nil {
		http.Error(w, "创建临时文件失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := io.Copy(tmp, r.Body); err != nil {
		tmp.Close()
		http.Error(w, "写入失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		http.Error(w, "设置权限失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := tmp.Close(); err != nil {
		http.Error(w, "关闭文件失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := os.Rename(tmpName, path); err != nil {
		http.Error(w, "落盘失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" {
		http.Error(w, "缺少 path 参数", http.StatusBadRequest)
		return
	}
	f, err := os.Open(path)
	if err != nil {
		http.Error(w, "打开文件失败: "+err.Error(), http.StatusNotFound)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	_, _ = io.Copy(w, f)
}

// handleArchive 原生解压归档（Go 实现，不依赖目标机 tar/unzip/xz；
// 旧版 agent 无此端点，控制端收到 404 后回退 shell 命令）。
func (s *Server) handleArchive(w http.ResponseWriter, r *http.Request) {
	src := r.URL.Query().Get("src")
	dest := r.URL.Query().Get("dest")
	if src == "" || dest == "" {
		http.Error(w, "缺少 src/dest 参数", http.StatusBadRequest)
		return
	}
	files, err := ExtractArchive(src, dest)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "files": files})
}

// handleShutdown 优雅退出；cleanupOnShutdown 时删除自身二进制。
func (s *Server) handleShutdown(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	go func() {
		time.Sleep(200 * time.Millisecond) // 等响应送达
		if s.cleanupOnShutdown.Load() {
			s.cleanupFiles()
		}
		if s.httpSrv != nil {
			_ = s.httpSrv.Shutdown(context.Background())
		}
		// httpSrv 为 nil（Handler 被外部嵌入测试）时仅作罢，不退出进程
	}()
}

// cleanupFiles 删除自身二进制与 mTLS 材料文件（cleanupOnShutdown 用；
// 证书/私钥启动时已载入内存，删除不影响运行）。
func (s *Server) cleanupFiles() {
	if len(os.Args) > 0 {
		_ = os.Remove(os.Args[0])
	}
	for _, f := range []string{s.tlsCAFile, s.tlsCertFile, s.tlsKeyFile} {
		if f != "" {
			_ = os.Remove(f)
		}
	}
}

// becomeScript 生成提权执行脚本与最终 stdin 内容。
// 密码经 stdin 传递（sudo -S 读首行，余下内容供 sudo 内命令继续读取），
// 不出现在脚本或命令行 argv 中，防同机用户 ps 窥探；req.Stdin 拼接在密码行之后。
func becomeScript(script, user, password, stdin string) (finalScript, finalStdin string) {
	if user == "" {
		return script, stdin
	}
	u := shellquote.Quote(user)
	if password != "" {
		return fmt.Sprintf("sudo -S -p '' -u %s -- /bin/sh -c %s", u, shellquote.Quote(script)),
			password + "\n" + stdin
	}
	return fmt.Sprintf("sudo -n -u %s -- /bin/sh -c %s", u, shellquote.Quote(script)), stdin
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
