package agent

import (
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"time"
)

// Handler 返回最终 HTTP 处理器。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	// 自检
	mux.HandleFunc("GET /health", s.handleHealth)
	// 执行命令
	mux.HandleFunc("POST /exec", s.handleExec)
	// 文件传输
	mux.HandleFunc("PUT /file", s.handleUpload)
	mux.HandleFunc("GET /file", s.handleDownload)
	// 解压
	mux.HandleFunc("POST /archive", s.handleArchive)
	// 近期日志拉取（控制端用文件记录）
	mux.HandleFunc("GET /logs", s.handleLogs)
	// 自治执行（docs/15 §7）：异步提交（立即返回 run_id）、进度查询、中止
	mux.HandleFunc("POST /plan", s.handlePlanSubmit)
	mux.HandleFunc("GET /plan/status", s.handlePlanStatus)
	mux.HandleFunc("POST /plan/cancel", s.handlePlanCancel)
	// 自清理
	mux.HandleFunc("POST /shutdown", s.handleShutdown)

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
	// CertNotAfter 服务端证书到期时刻（RFC3339；未启用 mTLS 为空）。
	// 控制端可批量巡检剩余有效期，快到期时经 /cert 远程换证。
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
		IdleLeftSec: -1,
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
	tmp, err := os.CreateTemp(filepath.Dir(path), ".wdp-agent-*")
	if err != nil {
		http.Error(w, "failed to create temp file: "+err.Error(), http.StatusInternalServerError)
		return
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	// 请求体上限（与 /exec 一致）：无上限 io.Copy 会被大 body 写满磁盘
	limit := s.maxRequestBodyLimit()
	n, err := io.Copy(tmp, io.LimitReader(r.Body, limit+1))
	if err != nil {
		_ = tmp.Close()
		http.Error(w, "write failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if n > limit {
		_ = tmp.Close()
		http.Error(w, "request body exceeds limit", http.StatusRequestEntityTooLarge)
		return
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		http.Error(w, "failed to set permissions: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := tmp.Close(); err != nil {
		http.Error(w, "failed to close file: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := os.Rename(tmpName, path); err != nil {
		http.Error(w, "failed to write to disk: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.logInfo("upload -> %s (%d bytes, mode=%#o)", path, n, mode.Perm())
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
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
	_, _ = io.Copy(w, f)
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
