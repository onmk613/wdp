package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"
	"wdp/internal/shellquote"
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
	Code      int    `json:"code"`
	Stdout    string `json:"stdout"`
	Stderr    string `json:"stderr"`
	TimedOut  bool   `json:"timed_out"`
	Cancelled bool   `json:"cancelled"` // 控制端取消/断开（区别于超时）
}

func (s *Server) handleExec(w http.ResponseWriter, r *http.Request) {
	var req execReq
	if err := json.NewDecoder(io.LimitReader(r.Body, s.maxRequestBodyLimit())).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, execResp{Code: -1, Stderr: "request body parse failed: " + err.Error()})
		return
	}
	if req.Script == "" {
		writeJSON(w, http.StatusBadRequest, execResp{Code: -1, Stderr: "script is empty"})
		return
	}

	// info 记命令概要（运行基本记录）；debug 补执行参数全貌
	s.logInfo("exec: %s", truncateStr(req.Script, 256))
	s.logDebug("exec detail: user=%q cwd=%q timeout_ms=%d env=%d", req.BecomeUser, req.Cwd, req.TimeoutMs, len(req.Env))

	// 提权：sudo -u（-n 免密；-S 密码经 stdin 传递，不进命令行，ps 不可见）
	script, stdin := becomeScript(req.Script, req.BecomeUser, req.BecomePassword, req.Stdin)

	ctx := r.Context()
	var cancel context.CancelFunc
	if req.TimeoutMs > 0 {
		ctx, cancel = context.WithTimeout(ctx, time.Duration(req.TimeoutMs)*time.Millisecond)
		defer cancel()
	}
	// become 时环境变量写进脚本内部（sudo 默认 env_reset 会剥夺外层注入的
	// 变量；脚本内 export 在 sudo 之后执行不受影响），非 become 走进程环境
	env := os.Environ()
	if req.BecomeUser != "" && len(req.Env) > 0 {
		var sb strings.Builder
		for k, v := range req.Env {
			if envKeyRe.MatchString(k) {
				fmt.Fprintf(&sb, "export %s=%s\n", k, shellquote.Quote(v))
			}
		}
		script = sb.String() + script
	} else {
		for k, v := range req.Env {
			env = append(env, k+"="+v)
		}
	}
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", script)
	setPgrp(cmd) // 独立进程组：超时整组击杀（sudo 提权的 root 子进程不残留）
	cmd.Cancel = func() error { return killGroup(cmd) }
	cmd.WaitDelay = 3 * time.Second
	cmd.Dir = req.Cwd
	if cmd.Dir == "" {
		cmd.Dir = "/"
	}
	cmd.Env = env
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	// 输出上限（每流 1MiB，与控制端截断对齐）：防高输出命令把常驻 agent 撑爆
	var stdout, stderr capWriter
	stdout.limit, stderr.limit = maxExecOutputBytes, maxExecOutputBytes
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()

	resp := execResp{Stdout: stdout.String(), Stderr: stderr.String()}
	if stdout.truncated {
		resp.Stdout += "\n[wdp-agent] " + "stdout exceeded 1MiB and was truncated"
	}
	if stderr.truncated {
		resp.Stderr += "\n[wdp-agent] " + "stderr exceeded 1MiB and was truncated"
	}
	if ctxErr := ctx.Err(); ctxErr != nil && err != nil {
		resp.Code = -1
		if errors.Is(ctxErr, context.DeadlineExceeded) {
			resp.TimedOut = true
			resp.Stderr += "\n[wdp-agent] " + "execution timed out and was terminated"
		} else {
			// 控制端主动取消/断开不是超时，错误归因不能混为一谈
			resp.Cancelled = true
			resp.Stderr += "\n[wdp-agent] " + "client cancelled or disconnected"
		}
	} else if err != nil {
		resp.Code = 1
		if ee, ok := err.(*exec.ExitError); ok {
			resp.Code = ee.ExitCode()
		}
	}
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
