package web

// exec run 的逐主机结果流（SSE）：POST /api/exec 立即返回 run_id，执行在
// 后台推进；GET /api/exec/stream?run_id=N 订阅——每台主机完成即推送一条
// host 事件（完整 stdout/stderr，与旧同步响应同载荷），run 收尾推送 done
// 后关流。大批量执行从「全部跑完才见结果」变为逐台实时可见。
//
// 与 runs 全局 SSE（runevents.go 的 poke-再拉取模型）不同：exec 事件载荷
// 就是结果本身。逐 run 缓冲全部事件：订阅晚会完整重放（含 run 已结束的
// 情况，前端刷新页面后可续看）；订阅通道满丢事件由前端 done 后的一次
// 详情拉取兜底对齐。缓冲在 run 收尾 10 分钟后回收。

import (
	"encoding/json"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// execEvent 一条流事件（host：单主机完成；done：run 收尾汇总）。
type execEvent struct {
	Event  string          `json:"event"`
	Result *ExecHostResult `json:"result,omitempty"` // host 事件的执行结果
	Status string          `json:"status,omitempty"` // host: ok/failed/unreachable
	OK     int             `json:"ok,omitempty"`     // done: 成功台数
	Failed int             `json:"failed,omitempty"` // done: 失败台数（含不可达）
}

// execRunStream 单个 exec run 的事件缓冲与订阅者。
type execRunStream struct {
	events []execEvent
	subs   []chan execEvent
	done   bool
}

// execHub 运行中（与近期完成）exec run 的流注册表。
type execHub struct {
	mu   sync.Mutex
	runs map[int64]*execRunStream
}

func newExecHub() *execHub {
	return &execHub{runs: map[int64]*execRunStream{}}
}

// start 注册即将开始的 run（先注册后执行再返回 run_id：POST 一返回订阅者
// 即可挂上，不存在丢事件的窗口）。
func (h *execHub) start(runID int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.runs[runID] = &execRunStream{}
}

// host 推送单主机完成事件。
func (h *execHub) host(runID int64, res ExecHostResult, status string) {
	h.publish(runID, execEvent{Event: "host", Result: &res, Status: status})
}

// finish 推送收尾事件并进入终态；缓冲保留 10 分钟供迟到订阅者重放。
func (h *execHub) finish(runID int64, ok, failed int) {
	h.publish(runID, execEvent{Event: "done", OK: ok, Failed: failed})
	h.mu.Lock()
	if st := h.runs[runID]; st != nil {
		st.done = true
	}
	h.mu.Unlock()
	time.AfterFunc(10*time.Minute, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		delete(h.runs, runID)
	})
}

// publish 追加进缓冲并广播（非阻塞：订阅通道满即丢，前端 done 后对齐兜底）。
func (h *execHub) publish(runID int64, ev execEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	st := h.runs[runID]
	if st == nil || st.done {
		return
	}
	st.events = append(st.events, ev)
	for _, ch := range st.subs {
		select {
		case ch <- ev:
		default:
		}
	}
}

// subscribe 返回（重放事件，实时通道，是否已收尾）。run 不在本进程（服务
// 重启/过期回收）时通道为 nil 且未收尾——调用方按落库状态兜底。
func (h *execHub) subscribe(runID int64) ([]execEvent, chan execEvent, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	st := h.runs[runID]
	if st == nil {
		return nil, nil, false
	}
	if st.done {
		return append([]execEvent{}, st.events...), nil, true
	}
	ch := make(chan execEvent, 256)
	st.subs = append(st.subs, ch)
	return append([]execEvent{}, st.events...), ch, false
}

func (h *execHub) unsubscribe(runID int64, ch chan execEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	st := h.runs[runID]
	if st == nil {
		return
	}
	for i, c := range st.subs {
		if c == ch {
			st.subs = append(st.subs[:i], st.subs[i+1:]...)
			return
		}
	}
}

// handleExecStream GET /api/exec/stream?run_id=N：exec 结果实时流。
func (s *Server) handleExecStream(w http.ResponseWriter, r *http.Request) {
	runID, err := strconv.ParseInt(r.URL.Query().Get("run_id"), 10, 64)
	if err != nil || runID <= 0 {
		writeError(w, http.StatusBadRequest, "run_id required")
		return
	}
	run, err := s.st.GetRun(runID)
	if err != nil {
		s.writeStoreErr(w, err)
		return
	}
	if run.Kind != "exec" {
		writeError(w, http.StatusNotFound, "not an exec run")
		return
	}
	// 可见性与创建时同口径：run:execute 作用域对 run 实际触达的主机集合
	//（selector 记录的就是裁剪后的执行集合）
	if allowed := s.hostScopeSet(r, verbRunExec); allowed != nil {
		vc := &runViewCtx{allowedIDs: allowed}
		if !vc.runVisible(run) {
			permRun403(w)
			return
		}
	}
	fl, canFlush := w.(http.Flusher)
	if !canFlush {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	enc := json.NewEncoder(w)
	send := func(ev execEvent) bool {
		if _, err := w.Write([]byte("event: " + ev.Event + "\ndata: ")); err != nil {
			return false
		}
		if err := enc.Encode(ev); err != nil {
			return false
		}
		if _, err := w.Write([]byte("\n")); err != nil {
			return false
		}
		fl.Flush()
		return true
	}

	replay, ch, done := s.execStreams.subscribe(runID)
	for _, ev := range replay {
		if !send(ev) {
			return
		}
	}
	if done {
		return
	}
	if ch == nil {
		// run 不在本进程（服务重启丢缓冲 / 缓冲已回收）：落库任务兜底
		// 重放 + done，前端随后可从详情端点拿完整记录
		s.replayExecFromStore(w, enc, fl, runID)
		return
	}
	defer s.execStreams.unsubscribe(runID, ch)
	if _, err := w.Write([]byte(": connected\n\n")); err != nil {
		return
	}
	fl.Flush()
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case ev := <-ch:
			if !send(ev) {
				return
			}
			if ev.Event == "done" {
				return
			}
		case <-heartbeat.C:
			if _, err := w.Write([]byte(": ping\n\n")); err != nil {
				return
			}
			fl.Flush()
		}
	}
}

// replayExecFromStore 无内存缓冲时的降级重放：run_tasks 落库行拼 host
// 事件（detail 是截断快照，完整输出走详情端点）+ done 收尾。
func (s *Server) replayExecFromStore(w http.ResponseWriter, enc *json.Encoder, fl http.Flusher, runID int64) {
	tasks, err := s.st.RunTasks(runID)
	if err != nil {
		s.writeInternal(w, err)
		return
	}
	ok, failed := 0, 0
	for _, t := range tasks {
		if t.Status == "ok" {
			ok++
		} else {
			failed++
		}
		res := ExecHostResult{Name: t.Host, Stdout: t.Detail}
		ev := execEvent{Event: "host", Result: &res, Status: t.Status}
		if _, err := w.Write([]byte("event: host\ndata: ")); err != nil {
			return
		}
		if err := enc.Encode(ev); err != nil {
			return
		}
		if _, err := w.Write([]byte("\n")); err != nil {
			return
		}
	}
	if _, err := w.Write([]byte("event: done\ndata: ")); err != nil {
		return
	}
	_ = enc.Encode(execEvent{Event: "done", OK: ok, Failed: failed})
	_, _ = w.Write([]byte("\n"))
	fl.Flush()
}
