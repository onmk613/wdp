package web

// run 事件总线 + SSE 端点：执行状态变化即时推送（GET /api/runs/stream）。
// 此前 RunsPage 5s / AppRunPage 2s 的全量轮询是读放大主源——每个打开的
// 控制台持续全表读。现在 run 的每次状态/任务落库都发一条窄事件（run id
// + 状态快照），订阅方按需拉取该 run 的详情；轮询降级为兜底。
//
// 事件只带 id/status/summary：不内嵌任务明细（明细体积大且随执行增长，
// 订阅方收到 poke 后用既有详情端点按需拉取，一次执行多次任务也只触发
// 前端自己的增量刷新）。

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

// runEvent 一条执行状态事件（SSE data 载荷）。
type runEvent struct {
	ID      int64  `json:"id"`
	Status  string `json:"status"`
	Summary string `json:"summary,omitempty"`
}

// runHub 扇出：订阅者各自一条带缓冲的通道；notify 非阻塞（慢消费者丢事件
// ——SSE 客户端还有兜底轮询补齐，总线绝不反压执行路径）。
type runHub struct {
	mu   sync.Mutex
	subs map[chan runEvent]struct{}
}

func newRunHub() *runHub { return &runHub{subs: map[chan runEvent]struct{}{}} }

const runEventBuffer = 32

func (h *runHub) subscribe() chan runEvent {
	ch := make(chan runEvent, runEventBuffer)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch
}

func (h *runHub) unsubscribe(ch chan runEvent) {
	h.mu.Lock()
	delete(h.subs, ch)
	h.mu.Unlock()
}

func (h *runHub) notify(e runEvent) {
	h.mu.Lock()
	for ch := range h.subs {
		select {
		case ch <- e:
		default: // 满则丢：订阅方有轮询兜底
		}
	}
	h.mu.Unlock()
}

// handleRunStream SSE 端点：text/event-stream，15s 心跳注释行防中间盒掐
// 空闲连接；客户端断开（r.Context 取消）即退订。
func (s *Server) handleRunStream(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no") // nginx 反代不缓冲
	w.WriteHeader(http.StatusOK)
	fl.Flush()

	ch := s.runs.subscribe()
	defer s.runs.unsubscribe(ch)
	// 可见性上下文在订阅期解析一次（run:view 作用域裁剪，与列表/详情
	// 同口径）；授权变更在客户端重连后生效——SSE 事件只带 id/status/
	// summary，粒度上没有比详情端点更多的泄露面
	vc := s.runViewCtxOf(r)
	enc := json.NewEncoder(w)
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	// 开场注释：代理/客户端立刻拿到 200 + 流头，不等首个事件
	if _, err := w.Write([]byte(": connected\n\n")); err != nil {
		return
	}
	fl.Flush()
	for {
		select {
		case <-r.Context().Done():
			return
		case e := <-ch:
			if !vc.unrestricted {
				run, err := s.st.GetRun(e.ID)
				if err != nil || !vc.runVisible(run) {
					continue // 作用域外的 run 不推送（拉取兜底同受裁剪）
				}
			}
			if _, err := w.Write([]byte("event: run\ndata: ")); err != nil {
				return
			}
			if err := enc.Encode(e); err != nil {
				return
			}
			if _, err := w.Write([]byte("\n")); err != nil {
				return
			}
			fl.Flush()
		case <-heartbeat.C:
			if _, err := w.Write([]byte(": ping\n\n")); err != nil {
				return
			}
			fl.Flush()
		}
	}
}
