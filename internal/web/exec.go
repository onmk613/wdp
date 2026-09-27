package web

// 远程执行：对选中的主机经 agent 通道执行命令/脚本，结果与审计入库
// （runs 表 kind=exec）。并发 5，单主机默认超时 120s。

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"wdp/internal/console"
	"wdp/internal/fmtutil"
	"wdp/internal/model"
	"wdp/internal/store"
)

// handleExecTargets 执行目标资源：当前用户 run:execute 范围内的主机
// （全局权限 = 全部）。执行页（远程命令/应用执行）的目标选择数据源——
// 资源显示与执行权限同口径，作用域用户不会再"选得到、跑不了"。
func (s *Server) handleExecTargets(w http.ResponseWriter, r *http.Request) {
	hosts, err := s.st.ListHosts("")
	if err != nil {
		s.writeInternal(w, err)
		return
	}
	allowed := s.hostScopeSet(r, verbRunExec)
	restricted := allowed != nil
	if restricted {
		hosts = intersectHosts(hosts, allowed)
	}
	writeJSON(w, http.StatusOK, map[string]any{"hosts": hosts, "restricted": restricted})
}

// ExecRequest 远程执行请求。
type ExecRequest struct {
	HostIDs    []int64 `json:"host_ids"`
	Script     string  `json:"script"`
	TimeoutSec int     `json:"timeout_sec"` // 单主机超时（默认 120）
}

// maxExecTimeoutSec 单主机超时上限（与前端一致）：无上限采纳会让单个
// 请求长期占住执行闸门与连接
const maxExecTimeoutSec = 3600

// ExecHostResult 一台主机的执行结果（console 实现的别名，API 契约不变）。
type ExecHostResult = console.ExecHostResult

func (s *Server) handleExec(w http.ResponseWriter, r *http.Request) {
	var req ExecRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if len(req.HostIDs) == 0 {
		writeError(w, http.StatusBadRequest, "host_ids is empty")
		return
	}
	if req.Script == "" {
		writeError(w, http.StatusBadRequest, "script is empty")
		return
	}
	timeout := 120
	if req.TimeoutSec > maxExecTimeoutSec {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("timeout_sec too large (max %d)", maxExecTimeoutSec))
		return
	}
	if req.TimeoutSec > 0 {
		timeout = req.TimeoutSec
	}
	// 保序去重：重复 ID 会让闸门对同一把锁二次 TryLock 恒失败，3 秒后
	// 以"被其它执行占用"409 误导用户（实际只是自己重复提交了同一台）
	seen := make(map[int64]bool, len(req.HostIDs))
	uniqIDs := make([]int64, 0, len(req.HostIDs))
	for _, id := range req.HostIDs {
		if !seen[id] {
			seen[id] = true
			uniqIDs = append(uniqIDs, id)
		}
	}
	hosts := make([]*store.Host, 0, len(uniqIDs))
	for _, id := range uniqIDs {
		if h, err := s.st.GetHost(id); err == nil {
			hosts = append(hosts, h)
		}
	}
	if len(hosts) == 0 {
		writeError(w, http.StatusBadRequest, "no valid hosts")
		return
	}
	// 执行交集：无权执行的主机剔除；一台都不剩则拒绝。部分裁剪必须
	// 审计留痕（应用执行同口径），且 run 的 selector 记实际执行集合
	// （裁前集合会让事后审计无法还原真实触达范围）
	if allowed := s.hostScopeSet(r, verbRunExec); allowed != nil {
		trimmed := intersectHosts(hosts, allowed)
		if len(trimmed) == 0 {
			writeError(w, http.StatusForbidden, "forbidden: no target host inside your run:execute scope")
			return
		}
		if len(trimmed) < len(hosts) {
			s.audit(r, "run", "exec", "", fmt.Sprintf("执行范围被权限裁剪：%d → %d 台", len(hosts), len(trimmed)))
		}
		hosts = trimmed
	}

	// per-host 执行闸门：同步请求限时获取（3s），忙则 409 让用户稍后重试
	// ——命令执行本身是同步返回结果的，不适合无限排队
	hostIDs := make([]int64, 0, len(hosts))
	for _, h := range hosts {
		hostIDs = append(hostIDs, h.ID)
	}
	release, ok := s.gate.TryAcquire(hostIDs, 3*time.Second)
	if !ok {
		writeError(w, http.StatusConflict, "目标主机正被其它执行占用（应用执行/升级），稍后重试")
		return
	}
	defer release()

	sel, _ := json.Marshal(map[string]any{"kind": "hosts", "ids": hostIDs})
	user, _ := r.Context().Value(ctxUser{}).(string)
	runID, err := s.st.CreateRun("exec", 0, "", "", "", 0, string(sel), user, "running")
	if err != nil {
		s.writeInternal(w, err)
		return
	}

	// 执行脱离请求生命周期（挂后台 ctx 而非 r.Context()），动机见 background()
	execCtx, cancelExec := context.WithCancel(s.background())
	defer cancelExec()
	results := s.execOnHosts(execCtx, hosts, req.Script, timeout, runID)
	okN := 0
	for _, res := range results {
		if res.Err == "" && res.Code == 0 {
			okN++
		}
	}
	status := "succeeded"
	if okN < len(results) {
		status = "failed"
	}
	summary := fmt.Sprintf("%d/%d host(s) ok", okN, len(results))
	_ = s.st.FinishRun(runID, status, summary)
	s.runs.notify(runEvent{ID: runID, Status: status, Summary: summary})
	writeJSON(w, http.StatusOK, map[string]any{"run_id": runID, "ok": okN, "failed": len(results) - okN, "results": results})
}

// execOnHosts 委托 console.ExecService（并发执行/逐主机落库）；SSE 通知
// 与 host 模型构建（mTLS scheme 探活）留在传输层注入。
func (s *Server) execOnHosts(ctx context.Context, hosts []*store.Host, script string, timeoutSec int, runID int64) []ExecHostResult {
	return s.execsvc.ExecOnHosts(ctx, hosts, script, timeoutSec, runID, func() {
		s.runs.notify(runEvent{ID: runID, Status: "running"})
	})
}

// ---- runs 查询 ----

func (s *Server) handleListRuns(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		_, _ = fmt.Sscanf(v, "%d", &limit)
	}
	runs, err := s.st.ListRuns(limit)
	if err != nil {
		s.writeInternal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, runs)
}

func (s *Server) handleGetRun(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	run, err := s.st.GetRun(id)
	if err != nil {
		// 此前一切错误当 404：DB 故障会被伪装成"记录不存在"误导运维
		s.writeStoreErr(w, err)
		return
	}
	tasks, err := s.st.RunTasks(id)
	if err != nil {
		s.writeInternal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"run": run, "tasks": tasks})
}

// handleDeleteRun 删除一条执行记录。
func (s *Server) handleDeleteRun(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := s.st.DeleteRun(id); err != nil {
		s.writeStoreErr(w, err)
		return
	}
	s.audit(r, "delete", "run", fmt.Sprintf("#%d", id), "任务明细一并删除")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleBatchDeleteRuns 批量删除执行记录。
func (s *Server) handleBatchDeleteRuns(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IDs []int64 `json:"ids"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if len(req.IDs) == 0 {
		writeError(w, http.StatusBadRequest, "ids is empty")
		return
	}
	okN := 0
	for _, id := range req.IDs {
		if err := s.st.DeleteRun(id); err == nil {
			okN++
		}
	}
	s.audit(r, "batch_delete", "run", fmt.Sprintf("%d 条", len(req.IDs)), fmt.Sprintf("成功 %d", okN))
	writeJSON(w, http.StatusOK, map[string]any{"ok": okN, "failed": len(req.IDs) - okN})
}

// truncate 超长任务明细截断。截断点回退到完整 UTF-8 序列边界：按字节
// 硬切多字节字符会留下乱码尾字节（任务输出常含中文）。实现与 executor
// 共用 fmtutil.TruncateUTF8（全仓唯一实现）。
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return fmtutil.TruncateUTF8(s, n) + "\n... (truncated)"
}

// agentHostModelWithScheme 是 ExecService 的 HostModel 注入：台账行 →
// executor 连接模型（探活定 mTLS/明文 scheme，与远程命令同一条路）。
func (s *Server) agentHostModelWithScheme(ctx context.Context, h *store.Host) *model.Host {
	return s.agentHostModel(h)
}
