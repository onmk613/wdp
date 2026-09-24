package web

// 主机监控的传输层：/metrics 代理与查询端点。解析与周期采样（scrapeLoop/
// 派生/告警评估）已迁 internal/worker.Monitor——本文件只剩 HTTP 面与
// agent 拉取的 scheme 选择（mTLS/明文，依赖 server 的 CA 材料）。

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"wdp/internal/store"
	"wdp/internal/worker"
)

// MetricSample / parsePromText 是 worker 包解析器的别名（?format=json
// 端点与既有测试的引用面不变）。
type MetricSample = worker.MetricSample

var parsePromText = worker.ParsePromText

// ---- 代理端点：GET /api/hosts/{id}/metrics ----

// handleHostMetrics 代理 agent 的 /metrics：默认原文（text/plain，
// Prometheus 抓取口），?format=json 返回解析后的数组（控制台实时页）。
// 认证走 requireAuth（会话 cookie 或 basic auth——Prometheus 用后者）。
func (s *Server) handleHostMetrics(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	h, ok := s.checkHost(w, r, verbHostView, id)
	if !ok {
		return
	}
	body, err := s.fetchAgentMetrics(r.Context(), h)
	if err != nil {
		// 错误原文回传（运维定位 agent 不可达的现场需要），同时落日志
		s.logger.Warn("fetch agent metrics failed", "host", h.Name, "err", err)
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	if r.URL.Query().Get("format") == "json" {
		writeJSON(w, http.StatusOK, parsePromText(body))
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = w.Write([]byte(body))
}

// fetchAgentMetrics 从 agent 拉取 /metrics 原文（通道与远程执行同一条路：
// 已纳管走 mTLS，未纳管走明文，不探活、不降级）。
func (s *Server) fetchAgentMetrics(ctx context.Context, h *store.Host) (string, error) {
	scheme, client := s.agentScheme(h), plainProbeClient()
	if scheme == "https" {
		client = s.cam.tlsClientFor(hostNameOf(h.Address))
	}
	url := fmt.Sprintf("%s://%s/metrics", scheme, net.JoinHostPort(h.Address, fmt.Sprint(h.AgentPort)))
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("agent metrics: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("agent metrics: HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// plainProbeClient 明文探活客户端（探活实现迁 worker 后的传输侧入口）。
func plainProbeClient() *http.Client { return worker.ProbeClient() }

// startMonitor 启动采样循环（Run 时调用；Fetch 注入 scheme 选择与
// mTLS 客户端）。Monitor 实例在 New 构造——主机删除路径会 Forget 差分
// 快照，测试直连 Handler 时也必须可用。
func (s *Server) startMonitor(ctx context.Context) {
	go s.monitor.Run(ctx)
}

// ---- 查询端点 ----

// handleListAlerts 当前全部主机告警（列表页健康标记轮询）。
func (s *Server) handleListAlerts(w http.ResponseWriter, r *http.Request) {
	list, err := s.st.ListHostAlerts()
	if err != nil {
		s.writeInternal(w, err)
		return
	}
	// 作用域裁剪：无 host:view 全局权限的用户只看得到自己范围内主机的告警
	if allowed := s.hostScopeSet(r, verbHostView); allowed != nil {
		out := make([]*store.HostAlert, 0, len(list))
		for _, a := range list {
			if allowed[a.HostID] {
				out = append(out, a)
			}
		}
		list = out
	}
	writeJSON(w, http.StatusOK, list)
}

// handleHostSeries 趋势查询：?metric=&labels=&hours=24（自现在起回溯），
// 或自定义窗口 ?from=（Unix 秒——不早于 30 天前，聚合保留期内）。
func (s *Server) handleHostSeries(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if _, ok := s.checkHost(w, r, verbHostView, id); !ok {
		return
	}
	metric := r.URL.Query().Get("metric")
	if metric == "" {
		writeError(w, http.StatusBadRequest, "metric is required")
		return
	}
	q := r.URL.Query()
	from := int64(0)
	if v := q.Get("from"); v != "" {
		_, _ = fmt.Sscanf(v, "%d", &from)
	}
	if from <= 0 {
		hours := 24
		if v := q.Get("hours"); v != "" {
			_, _ = fmt.Sscanf(v, "%d", &hours)
		}
		if hours <= 0 || hours > 24*30 {
			hours = 24
		}
		from = time.Now().Add(-time.Duration(hours) * time.Hour).Unix()
	}
	// from 钳制到聚合保留期（30 天）：更早的窗口查不到数据，也防任意回溯
	if minFrom := time.Now().Add(-30 * 24 * time.Hour).Unix(); from < minFrom {
		from = minFrom
	}
	pts, err := s.st.QuerySeries(id, metric, q.Get("labels"), from)
	if err != nil {
		s.writeInternal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, pts)
}

// handleHostTasks 该主机最近的执行任务。
func (s *Server) handleHostTasks(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	h, ok := s.checkHost(w, r, verbHostView, id)
	if !ok {
		return
	}
	tasks, err := s.st.HostTasksByName(h.Name, 30)
	if err != nil {
		s.writeInternal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, tasks)
}
