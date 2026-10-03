package web

// run 的用户取消：POST /api/runs/{id}/cancel。执行器在任务/批次/play
// 边界响应 ctx 取消（在途命令会执行完），已执行任务的结果保留在
// run_tasks——幂等模块下重新发起即续跑，这是 web 路径「暂停/继续」的
// 落地形态（executor 无中途暂停原语，取消+重发等价于断点续跑）。

import (
	"context"
	"fmt"
	"net/http"
	"sync"
)

// runCancelState 单个 run 的取消状态。cancel 在排队期为 nil（此时只有
// requested 生效：轮到该 run 时直接跳过），进入执行期后替换为 item ctx
// 的 CancelFunc。
type runCancelState struct {
	mu        sync.Mutex
	cancel    context.CancelFunc
	requested bool
}

// registerRunCancels 为一次提交的全部 run 登记取消状态（goroutine 起点，
// 排队期即可被请求取消）。
func (s *Server) registerRunCancels(ids []int64) {
	s.runCancelsMu.Lock()
	defer s.runCancelsMu.Unlock()
	for _, id := range ids {
		s.runCancels[id] = &runCancelState{}
	}
}

// armRunCancel 进入执行期：换上真正的取消句柄。已请求取消的 run 返回
// false（调用方不应再启动执行）。
func (s *Server) armRunCancel(id int64, cancel context.CancelFunc) bool {
	s.runCancelsMu.Lock()
	st := s.runCancels[id]
	s.runCancelsMu.Unlock()
	if st == nil {
		return true // 未登记（异常路径）：不拦执行
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.requested {
		return false // 排队期已请求取消：跳过执行
	}
	st.cancel = cancel
	return true
}

// unregisterRunCancel run 终态后注销。
func (s *Server) unregisterRunCancel(id int64) {
	s.runCancelsMu.Lock()
	defer s.runCancelsMu.Unlock()
	delete(s.runCancels, id)
}

// requestRunCancel 请求取消：置位 requested 并调用当前取消句柄。返回
// false = run 不在执行注册表（已结束或不在本进程）。
func (s *Server) requestRunCancel(id int64) bool {
	s.runCancelsMu.Lock()
	st := s.runCancels[id]
	s.runCancelsMu.Unlock()
	if st == nil {
		return false
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.requested = true
	if st.cancel != nil {
		st.cancel()
	}
	return true
}

// runCancelRequested 查询取消请求是否已发出。
func (s *Server) runCancelRequested(id int64) bool {
	s.runCancelsMu.Lock()
	st := s.runCancels[id]
	s.runCancelsMu.Unlock()
	if st == nil {
		return false
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.requested
}

// handleCancelRun POST /api/runs/{id}/cancel：取消排队中/执行中的 run。
func (s *Server) handleCancelRun(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	run, err := s.st.GetRun(id)
	if err != nil {
		s.writeStoreErr(w, err)
		return
	}
	// 越权取消守卫：与详情（run:view 裁剪）和 exec 流（run:execute 交集）
	// 同口径——此前路由只要求全局 run:execute，持作用域权限的用户可取消
	// 任意用户对任意主机的在途 run（干扰级越权），是权限模型里唯一漏掉
	// 可见性判定的变更类端点。
	if allowed := s.hostScopeSet(r, verbRunExec); allowed != nil {
		vc := &runViewCtx{allowedIDs: allowed}
		if !vc.runVisible(run) {
			permRun403(w)
			return
		}
	}
	if run.Status != "queued" && run.Status != "running" {
		writeError(w, http.StatusConflict, fmt.Sprintf("run 已结束（%s），无需取消", run.Status))
		return
	}
	if !s.requestRunCancel(id) {
		writeError(w, http.StatusConflict, "run 不在本进程（服务可能已重启），请刷新后确认状态")
		return
	}
	s.audit(r, "cancel", "run", fmt.Sprintf("#%d", id), fmt.Sprintf("%s@%s", run.AppName, run.Version))
	// 中间态先行广播：取消在执行器边界生效（在途任务跑完），DB 终态
	// 由执行 goroutine 落 cancelled
	s.runs.notify(runEvent{ID: id, Status: "cancelling"})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": "cancelling"})
}
