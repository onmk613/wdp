package web

// server 自身的可观测性（此前只有 agent 有 /metrics，server 一个端点都
// 没有——排障全靠日志猜）：
//   - GET /metrics：Prometheus 文本格式的基础指标（HTTP 请求计数/耗时
//     直方图/在途数、go 运行时）。
//   - GET /debug/pprof/*：标准库 pprof 索引与 profile 端点。
// 两族端点都要求 admin 会话（性能数据与堆 profile 不给普通账号），
// 都不进 /api 前缀，避免与业务路由、审计语义混淆。

import (
	"fmt"
	"net/http"
	_ "net/http/pprof" // 注册到 DefaultServeMux，本文件桥接进 server 路由
	"runtime"
	"strings"
	"sync/atomic"
	"time"
)

// httpMetrics 是中间件收集的请求指标（原子计数，无锁热路径）。
type httpMetrics struct {
	requests atomic.Int64 // 总请求数
	errors   atomic.Int64 // 5xx 响应数
	inFlight atomic.Int64 // 当前在途请求
	// bucketed 按毫秒对数桶统计耗时（粗粒度足够排障，不引依赖）
	bucketed [len(durationBucketsMS)]atomic.Int64
}

var durationBucketsMS = [10]int64{1, 5, 10, 25, 50, 100, 250, 500, 1000, 1 << 40}

// observe 记一次请求（method/路径不分桶：路由基数小，总量+耗时+错误已
// 足够定位「慢在哪一族」；细分用 pprof）。
func (m *httpMetrics) observe(d time.Duration, isErr bool) {
	m.requests.Add(1)
	if isErr {
		m.errors.Add(1)
	}
	ms := d.Milliseconds()
	for i, b := range durationBucketsMS {
		if ms <= b {
			m.bucketed[i].Add(1)
			break
		}
	}
}

// metricsMiddleware 包一层请求计数与状态码观测。不改变任何行为；包在
// 鉴权之外（登录页/健康检查也计数，401 也是正常流量）。
func (s *Server) metricsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.metrics.inFlight.Add(1)
		defer s.metrics.inFlight.Add(-1)
		t0 := time.Now()
		sw := &statusWriter{ResponseWriter: w, code: http.StatusOK}
		next.ServeHTTP(sw, r)
		s.metrics.observe(time.Since(t0), sw.code >= 500)
	})
}

type statusWriter struct {
	http.ResponseWriter
	code     int
	wroteHdr bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.wroteHdr {
		w.code = code
		w.wroteHdr = true
	}
	w.ResponseWriter.WriteHeader(code)
}

// Flush 透传 http.Flusher：SSE（/api/runs/stream）等流式响应依赖它。
// 少了这个方法，包装后的 ResponseWriter 类型断言 http.Flusher 必然失败
// ——接口在生产直接 500，而测试直连裸 mux 时全绿（曾真实发生过）。
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap 供 http.ResponseController 穿透本包装器，让 Flush/SetWriteDeadline
// 这类可选能力不被中间件截断。
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// renderMetrics 输出 Prometheus 文本格式（指标面很小，手写格式三行一个
// 指标，可控且零依赖；后续指标多了再考虑 client_golang）。
func (s *Server) renderMetrics(w http.ResponseWriter) {
	var b strings.Builder
	p := func(name, help, typ string, samples ...string) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
		for _, sp := range samples {
			b.WriteString(sp)
			b.WriteByte('\n')
		}
	}
	p("wdp_http_requests_total", "Total HTTP requests handled by the console server.", "counter",
		fmt.Sprintf("wdp_http_requests_total %d", s.metrics.requests.Load()))
	p("wdp_http_errors_total", "HTTP responses with status >= 500.", "counter",
		fmt.Sprintf("wdp_http_errors_total %d", s.metrics.errors.Load()))
	p("wdp_http_in_flight", "Requests currently being handled.", "gauge",
		fmt.Sprintf("wdp_http_in_flight %d", s.metrics.inFlight.Load()))

	// 耗时直方图（累计桶：le 毫秒 → 累计计数）
	var lines []string
	var cum int64
	for i, le := range durationBucketsMS {
		cum += s.metrics.bucketed[i].Load()
		label := "+Inf"
		if le < 1<<39 {
			label = fmt.Sprintf("%d", le)
		}
		lines = append(lines, fmt.Sprintf("wdp_http_request_duration_milliseconds_bucket{le=%q} %d", label, cum))
	}
	lines = append(lines, fmt.Sprintf("wdp_http_request_duration_milliseconds_count %d", s.metrics.requests.Load()))
	p("wdp_http_request_duration_milliseconds", "HTTP request duration (coarse cumulative log buckets).", "histogram", lines...)

	p("wdp_go_goroutines", "Number of goroutines.", "gauge",
		fmt.Sprintf("wdp_go_goroutines %d", runtime.NumGoroutine()))
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	p("wdp_go_heap_alloc_bytes", "Heap bytes in use.", "gauge",
		fmt.Sprintf("wdp_go_heap_alloc_bytes %d", ms.HeapAlloc))
	p("wdp_go_gc_pause_total_seconds", "Total GC pause in seconds.", "gauge",
		fmt.Sprintf("wdp_go_gc_pause_total_seconds %d", ms.PauseTotalNs/1e9))

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = w.Write([]byte(b.String()))
}

// requireAdmin 中间件：非 admin 会话一律 403（观测端点含性能数据与堆
// profile；Prometheus 抓取经反代认证后回环转发，或 basic auth 抓
// /api/hosts/{id}/metrics 的代理路径族）。
func (s *Server) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user, _ := r.Context().Value(ctxUser{}).(string)
		if !s.permsOf(user).isAdmin() {
			writeError(w, http.StatusForbidden, "forbidden: admin only")
			return
		}
		next(w, r)
	}
}

// routesObservability 注册观测端点（routes() 末尾调用）。pprof 借标准库
// DefaultServeMux（blank import 注册），这里只做桥接与 admin 门——索引
// /debug/pprof/ 与全部子路径一次接全。按方法注册：无方法的 "/debug/
// pprof/" 会与 SPA 兜底 "GET /" 冲突（mux 拒绝更宽方法集的更具体路径）。
func (s *Server) routesObservability() {
	bridge := func(w http.ResponseWriter, r *http.Request) {
		user, _ := r.Context().Value(ctxUser{}).(string)
		if !s.permsOf(user).isAdmin() {
			writeError(w, http.StatusForbidden, "forbidden: admin only")
			return
		}
		http.DefaultServeMux.ServeHTTP(w, r)
	}
	s.mux.HandleFunc("GET /metrics", s.requireAuth(s.requireAdmin(s.handleServerMetrics)))
	s.mux.HandleFunc("GET /debug/pprof/", s.requireAuth(bridge))
	s.mux.HandleFunc("POST /debug/pprof/", s.requireAuth(bridge))
}

func (s *Server) handleServerMetrics(w http.ResponseWriter, _ *http.Request) {
	s.renderMetrics(w)
}
