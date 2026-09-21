package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"wdp/internal/dumphttp"
)

// LevelTrace 是最详细的自定义日志级别（在 debug 的逐操作记录之外，
// 再对每个已认证 HTTP 请求/响应做 httpdump）。
const LevelTrace = slog.LevelDebug - 4

// logRingBytes 是 /logs 可拉取的内存环形缓冲上限（近期日志约 512KiB；
// 完整历史走 --log-file 落盘再 GET /file 拉取）。
const logRingBytes = 512 << 10

// ringWriter 有界内存缓冲：写满后丢弃前半段只保近期（供控制端
// GET /logs 拉取；未配 --log-file 时这是唯一可远程读取的来源）。
type ringWriter struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func newRingWriter(max int) *ringWriter { return &ringWriter{max: max} }

func (r *ringWriter) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf = append(r.buf, p...)
	if len(r.buf) > r.max {
		r.buf = append(r.buf[:0], r.buf[len(r.buf)-r.max/2:]...)
	}
	return len(p), nil
}

func (r *ringWriter) snapshot() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]byte(nil), r.buf...)
}

// logSink 把日志扇出到多个目的地（stderr、可选文件、环形缓冲）。
// 文件可在启动后追加，无需重建 logger（在途请求无感）。
type logSink struct {
	mu      sync.Mutex
	writers []io.Writer
}

func (s *logSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	ws := append([]io.Writer(nil), s.writers...)
	s.mu.Unlock()
	for _, w := range ws {
		_, _ = w.Write(p)
	}
	return len(p), nil
}

func (s *logSink) add(w io.Writer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writers = append(s.writers, w)
}

// initLogger 装配日志内核：stderr（systemd 部署进 journal）+ 内存环形
// 缓冲（/logs 拉取），--log-file 时再追加文件。级别经 LevelVar 热改。
func (s *Server) initLogger() {
	s.logRing = newRingWriter(logRingBytes)
	s.sink = &logSink{writers: []io.Writer{os.Stderr, s.logRing}}
	s.levelVar = new(slog.LevelVar)
	s.levelVar.Set(slog.LevelInfo)
	s.logger = slog.New(slog.NewTextHandler(s.sink, &slog.HandlerOptions{
		Level: s.levelVar,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			// 自定义级别默认渲染为 "DEBUG-4"，换成可读的 TRACE
			if a.Key == slog.LevelKey && len(groups) == 0 {
				if lv, ok := a.Value.Any().(slog.Level); ok && lv == LevelTrace {
					a.Value = slog.StringValue("TRACE")
				}
			}
			return a
		},
	}))
}

// SetLogLevel 设置日志级别（trace|debug|info|warn|error；默认 info）。
func (s *Server) SetLogLevel(name string) error {
	lv, err := ParseLogLevel(name)
	if err != nil {
		return err
	}
	s.levelVar.Set(lv)
	return nil
}

// ParseLogLevel 解析日志级别名（空串 = 默认 info）。
func ParseLogLevel(name string) (slog.Level, error) {
	switch normalizeLevel(name) {
	case "trace":
		return LevelTrace, nil
	case "debug":
		return slog.LevelDebug, nil
	case "info", "":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	return slog.LevelInfo, fmt.Errorf("%s", "unknown log level "+name+" (trace|debug|info|warn|error)")
}

func normalizeLevel(name string) string {
	n := strings.ToLower(strings.TrimSpace(name))
	if n == "warning" {
		n = "warn"
	}
	return n
}

// SetLogFile 追加日志文件输出（自动建父目录，0600，追加写）。
// 日志文件不属于自清理范围：agent 退役后保留供审计，确要删除时显式
// 列入 POST /shutdown 的 files。
func (s *Server) SetLogFile(path string) error {
	if path == "" {
		return nil
	}
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	s.sink.add(f)
	s.logInfo("log file attached: %s", path)
	return nil
}

// logAt 记录一条事件日志（args 按序填充模板），经 slog 扇出到 stderr /
// 日志文件 / 内存缓冲（GET /logs 可拉取）。日志为机器产物，固定英文
// （grep 友好，多目标机拉回控制端语言一致）；本地化只覆盖帮助文案。
func (s *Server) logAt(level slog.Level, msg string, args ...any) {
	if !s.logger.Enabled(context.Background(), level) {
		return
	}
	s.logger.Log(context.Background(), level, fmt.Sprintf(msg, args...))
}

func (s *Server) logInfo(msg string, args ...any)  { s.logAt(slog.LevelInfo, msg, args...) }
func (s *Server) logWarn(msg string, args ...any)  { s.logAt(slog.LevelWarn, msg, args...) }
func (s *Server) logDebug(msg string, args ...any) { s.logAt(slog.LevelDebug, msg, args...) }
func (s *Server) logTrace(msg string, args ...any) { s.logAt(LevelTrace, msg, args...) }

// scriptDigest 返回脚本内容的 sha256 前 8 位（日志用摘要替代明文：脚本
// 常含密码/令牌，日志会落盘并可经 /logs 拉取）。
func scriptDigest(script string) string {
	sum := sha256.Sum256([]byte(script))
	return hex.EncodeToString(sum[:])[:8]
}

// logRequest 访问日志（debug 起）与 httpdump（trace 起）中间件，位于
// pin 之内——只记录已认证请求。/health 探测不记（防巡检刷屏）；/logs
// 响应不转储（拉日志的响应体即日志本身，转储只会制造反馈噪声）。
func (s *Server) logRequest(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.logger.Enabled(r.Context(), slog.LevelDebug) || r.URL.Path == "/health" {
			next.ServeHTTP(w, r)
			return
		}
		trace := s.logger.Enabled(r.Context(), LevelTrace)
		if trace && r.URL.Path != "/logs" {
			s.logTrace("httpdump request:\n%s", dumphttp.Request(r))
		}
		rec := dumphttp.NewRecorder(w)
		start := time.Now()
		next.ServeHTTP(rec, r)
		s.logDebug("http %s %s -> %d in %s", r.Method, r.URL.RequestURI(), rec.Status(), time.Since(start).Round(time.Millisecond))
		if trace && r.URL.Path != "/logs" {
			s.logTrace("httpdump response:\n%s", dumphttp.Response(rec.Status(), rec.Header(), rec.Body(), rec.Truncated))
		}
	})
}

// handleLogs 返回近期日志（内存环形缓冲，约最近 512KiB；完整历史用
// --log-file 落盘后经 GET /file 拉取文件）。与其它业务端点同认证。
func (s *Server) handleLogs(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write(s.logRing.snapshot())
}
