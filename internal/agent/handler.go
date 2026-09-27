package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"time"

	"wdp/internal/fsatomic"
)

// Handler 返回最终 HTTP 处理器。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.handleHealth)
	// 主机指标（Prometheus 文本格式，对齐 node_exporter 命名）
	mux.HandleFunc("GET /metrics", s.handleMetrics)
	mux.HandleFunc("POST /exec", s.handleExec)
	mux.HandleFunc("PUT /file", s.handleUpload)
	mux.HandleFunc("GET /file", s.handleDownload)
	mux.HandleFunc("POST /archive", s.handleArchive)
	// 近期日志拉取（控制端用文件记录）
	mux.HandleFunc("GET /logs", s.handleLogs)
	// 自治执行（docs/15 §7）：异步提交（立即返回 run_id）、进度查询、中止
	mux.HandleFunc("POST /plan", s.handlePlanSubmit)
	mux.HandleFunc("GET /plan/status", s.handlePlanStatus)
	mux.HandleFunc("POST /plan/cancel", s.handlePlanCancel)
	mux.HandleFunc("POST /shutdown", s.handleShutdown)
	// 证书热更换（控制端临期重签推送；mTLS 模式专用，见 cert.go）
	mux.HandleFunc("POST /cert", s.handleCert)

	// 空闲计数置于最内层：只统计真正到达业务端点的请求（/health 除外——
	// 免认证探测不重置空闲计时，防同网段任意主机给残留 agent 续命）。
	// logRequest 位于其外、pin 之内：访问日志/httpdump 只记已认证请求。
	h := s.trackActivity(mux)
	h = s.logRequest(h)
	if s.material.Load() != nil {
		h = s.pinMiddleware(h)
	}
	for _, v := range slices.Backward(s.middlewares) {
		h = v(h)
	}
	return h
}

type healthResp struct {
	Ok       bool   `json:"ok"`
	Version  string `json:"version"`
	Hostname string `json:"hostname"`
	Goos     string `json:"goos"`
	Arch     string `json:"arch"`
	Pid      int    `json:"pid"`
	// Build 是二进制发布版本（cli.Version-commit，ldflags 注入），远程
	// 升级用它判断 agent 新旧；空 = ldflags 未注入的旧版二进制。
	Build string `json:"build,omitempty"`
	// BinPath 是 agent 自身二进制路径（os.Executable）。远程升级把新
	// 二进制推到同目录做原子替换；旧版 agent 无此字段时由脚本探测兜底。
	BinPath string `json:"bin_path,omitempty"`
	// CertNotAfter 服务端证书到期时刻（RFC3339；未启用 mTLS 为空）。
	// 控制端（agentctl status）批量巡检剩余有效期；快到期时经 web 控制台
	// POST /api/hosts/{id}/renew-cert 重签并推送（POST /cert 热更换）。
	CertNotAfter string `json:"cert_not_after,omitempty"`
	// IdleTimeoutSec 空闲自动退出周期秒（0 = 永不）；IdleLeftSec 剩余秒（-1 = 永不）
	IdleTimeoutSec int64 `json:"idle_timeout_sec"`
	IdleLeftSec    int64 `json:"idle_left_sec"`
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	host, _ := os.Hostname()
	resp := healthResp{
		Ok: true, Version: Version, Hostname: host,
		Goos: runtime.GOOS, Arch: runtime.GOARCH, Pid: os.Getpid(),
		Build:       BuildVersion(),
		IdleLeftSec: -1,
	}
	if exe, err := os.Executable(); err == nil {
		resp.BinPath = exe
	}
	if m := s.material.Load(); m != nil && m.leaf != nil {
		resp.CertNotAfter = m.leaf.NotAfter.Format(time.RFC3339)
	}
	if s.idleTimeout > 0 {
		resp.IdleTimeoutSec = int64(s.idleTimeout / time.Second)
		left := max(s.idleTimeout-time.Since(time.Unix(0, s.lastActive.Load())), 0)
		resp.IdleLeftSec = int64(left / time.Second)
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleExec(w http.ResponseWriter, r *http.Request) {
	var req ExecReq
	if err := json.NewDecoder(io.LimitReader(r.Body, s.maxRequestBodyLimit())).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, ExecResp{Code: -1, Stderr: "request body parse failed: " + err.Error()})
		return
	}
	if req.Script == "" {
		writeJSON(w, http.StatusBadRequest, ExecResp{Code: -1, Stderr: "script is empty"})
		return
	}

	// info 只记脚本摘要（字节数 + sha256 前 8 位）：脚本内容常含密码/令牌
	//（no_log 只遮蔽控制端结果输出，不影响下发脚本），日志会落 stderr /
	// --log-file / 环形缓冲并可经 /logs 拉取，不得记明文。
	s.logInfo("exec: %d bytes sha256=%s", len(req.Script), scriptDigest(req.Script))
	s.logDebug("exec detail: user=%q cwd=%q timeout_ms=%d env=%d", req.BecomeUser, req.Cwd, req.TimeoutMs, len(req.Env))

	resp := RunScript(r.Context(), req)
	s.logDebug("exec done: code=%d timed_out=%v cancelled=%v stdout=%dB stderr=%dB", resp.Code, resp.TimedOut, resp.Cancelled, len(resp.Stdout), len(resp.Stderr))
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" {
		http.Error(w, "missing path parameter", http.StatusBadRequest)
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
		http.Error(w, "failed to create directory: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// 请求体上限（与 /exec 一致）：无上限 io.Copy 会被大 body 写满磁盘；
	// 读满上限后仍有多余字节即报 errBodyTooLarge（413），由 fsatomic 的
	// 失败清理路径删除临时文件。原子落盘细节（fsync/chmod/rename/目录
	// 同步）收敛在 fsatomic.WriteFile
	lr := &limitedReader{r: r.Body, remain: s.maxRequestBodyLimit()}
	if err := fsatomic.WriteFile(path, lr, mode); err != nil {
		if errors.Is(err, errBodyTooLarge) {
			http.Error(w, "request body exceeds limit", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "write failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.logInfo("upload -> %s (%d bytes, mode=%#o)", path, lr.n, mode.Perm())
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// errBodyTooLarge 表示请求体超过上限（handleUpload 的 413 判定哨兵）。
var errBodyTooLarge = errors.New("request body exceeds limit")

// limitedReader 读满 remain 字节后仍有多余数据时报 errBodyTooLarge：
// 替代旧实现的 LimitReader(limit+1)+计数写法（恰好等于上限的 body 仍被
// 接受，超出即失败），使错误经 fsatomic 的统一失败路径清理临时文件。
// n 记录实际读取字节数（上传日志用）。
type limitedReader struct {
	r      io.Reader
	remain int64
	n      int64
}

func (l *limitedReader) Read(p []byte) (int, error) {
	if l.remain <= 0 {
		// 恰好读满还不能判超限：再探一字节区分"等于上限"（底层 EOF，
		// 透传接受）与"超出上限"（errBodyTooLarge → 413），与旧实现
		// LimitReader(limit+1)+计数等价
		var probe [1]byte
		if n, err := l.r.Read(probe[:]); n > 0 {
			return 0, errBodyTooLarge
		} else {
			return 0, err
		}
	}
	if int64(len(p)) > l.remain {
		p = p[:l.remain]
	}
	n, err := l.r.Read(p)
	l.remain -= int64(n)
	l.n += int64(n)
	return n, err
}

func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" {
		http.Error(w, "missing path parameter", http.StatusBadRequest)
		return
	}
	f, err := os.Open(path)
	if err != nil {
		http.Error(w, "failed to open file: "+err.Error(), http.StatusNotFound)
		return
	}
	defer f.Close()
	if st, err := f.Stat(); err == nil {
		s.logInfo("download <- %s (%d bytes)", path, st.Size())
	} else {
		s.logInfo("download <- %s", path)
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	if _, err := io.Copy(w, f); err != nil {
		// 客户端中断同样留痕：上传路径有错误处理，下载静默吞掉会不对称
		//（大文件拉取中断无迹可查）
		s.logWarn("download %s: client write failed: %v", path, err)
	}
}

// handleArchive 原生解压归档（Go 实现，不依赖目标机 tar/unzip/xz；
// 旧版 agent 无此端点，控制端收到 404 后回退 shell 命令）。
func (s *Server) handleArchive(w http.ResponseWriter, r *http.Request) {
	src := r.URL.Query().Get("src")
	dest := r.URL.Query().Get("dest")
	if src == "" || dest == "" {
		http.Error(w, "missing src/dest parameter", http.StatusBadRequest)
		return
	}
	s.logInfo("extract %s -> %s", src, dest)
	files, err := ExtractArchive(src, dest)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.logInfo("extract done: %d files", files)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "files": files})
}
