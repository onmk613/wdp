package web

// 远程执行：对选中的主机经 agent 通道执行命令/脚本，结果与审计入库
// （runs 表 kind=exec）。并发 5，单主机默认超时 120s。

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
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

// maxExecScriptStored exec 脚本入库快照上限（16 KiB，与任务明细截断同档）：
// 超长脚本存截断快照 + 完整 sha256，证据链不因截断受损。
const maxExecScriptStored = 16 << 10

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
		h, err := s.st.GetHost(id)
		// 台账已删的 ID 跳过（客户端可能持有过期列表）；但 DB 故障必须
		// fail-loud——静默缩小执行范围会让"部分主机没执行"看起来像成功
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			s.writeInternal(w, err)
			return
		}
		hosts = append(hosts, h)
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

	sel, _ := json.Marshal(map[string]any{"kind": "hosts", "ids": hostIDs})
	user, _ := r.Context().Value(ctxUser{}).(string)
	// 执行证据入库（截断快照 + 完整脚本 sha256）：exec 是任意命令执行，
	// 此前只记 selector/用户，事后无法回答"谁在哪台机执行了什么命令"。
	// 截断不损害证据链——哈希可对质完整脚本。
	runID, err := s.st.CreateRun(store.RunInput{
		Kind: "exec", Selector: string(sel), User: user, Status: "running",
		Script:    truncate(req.Script, maxExecScriptStored),
		ScriptSHA: fmt.Sprintf("%x", sha256.Sum256([]byte(req.Script))),
	})
	if err != nil {
		release()
		s.writeInternal(w, err)
		return
	}

	// 立即返回 run_id，执行在后台推进（SSE 逐主机推送，见 execstream.go）：
	// 大批量/长超时执行不必占住 HTTP 响应（此前响应要等全部主机完成，
	// 数百台 × 分钟级超时下页面长时间无反馈）。执行闸门与后台 ctx 随执行
	// 结束释放，而非请求返回。
	s.execStreams.start(runID)
	execCtx, cancelExec := context.WithCancel(s.background())
	go func() {
		defer release()
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
		s.execStreams.finish(runID, okN, len(results)-okN)
	}()
	writeJSON(w, http.StatusOK, map[string]any{"run_id": runID})
}

// execOnHosts 委托 console.ExecService（并发执行/逐主机落库）；逐主机事件
// 双路分发：exec 专属流（带结果载荷）+ runs 全局 poke（RunsPage 抽屉刷新）。
// host 模型构建（mTLS scheme 探活）留在传输层注入。
func (s *Server) execOnHosts(ctx context.Context, hosts []*store.Host, script string, timeoutSec int, runID int64) []ExecHostResult {
	return s.execsvc.ExecOnHosts(ctx, hosts, script, timeoutSec, runID, func(res ExecHostResult, status string) {
		s.execStreams.host(runID, res, status)
		s.runs.notify(runEvent{ID: runID, Status: "running"})
	})
}

// ---- runs 查询 ----

func (s *Server) handleListRuns(w http.ResponseWriter, r *http.Request) {
	// 分页模式（page/page_size）：作用域可见性过滤后内存分页（run 可见
	// 性依赖触达主机，SQL 化会耦合权限模型；执行记录按 id 索引翻页）
	if r.URL.Query().Has("page") || r.URL.Query().Has("page_size") {
		pp := parsePage(r)
		runs, _, err := s.st.ListRunsPage(r.URL.Query().Get("kind"), 1, 1<<30) // 全量取，可见性过滤后切页
		if err != nil {
			s.writeInternal(w, err)
			return
		}
		vc := s.runViewCtxOf(r)
		vis := make([]*store.Run, 0, len(runs))
		for _, rn := range runs {
			if vc.runVisible(rn) {
				vis = append(vis, rn)
			}
		}
		lo, hi := pp.offset(), pp.offset()+pp.PageSize
		if lo > len(vis) {
			lo = len(vis)
		}
		if hi > len(vis) {
			hi = len(vis)
		}
		writeJSON(w, http.StatusOK, pagedResp[*store.Run]{Items: vis[lo:hi], Total: int64(len(vis)), Page: pp.Page, PageSize: pp.PageSize})
		return
	}
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		_, _ = fmt.Sscanf(v, "%d", &limit)
	}
	runs, err := s.st.ListRuns(limit)
	if err != nil {
		s.writeInternal(w, err)
		return
	}
	vc := s.runViewCtxOf(r)
	out := make([]*store.Run, 0, len(runs))
	for _, rn := range runs {
		if vc.runVisible(rn) {
			out = append(out, rn)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// handleGetRun 单条执行详情（exec 附带脚本快照与完整哈希——执行证据
// 只有详情端点回传，列表不带）。
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
	vc := s.runViewCtxOf(r)
	if !vc.runVisible(run) {
		permRun403(w)
		return
	}
	tasks, err := s.st.RunTasks(id)
	if err != nil {
		s.writeInternal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"run": run, "tasks": vc.filterTasks(tasks),
		"script": run.Script, "script_sha256": run.ScriptSHA,
	})
}

// ---- run 可见性（run:view 作用域裁剪）----
//
// 此前 runs 列表/详情只要求全局 run:view（viewer 角色即有），run_tasks
// 的 stdout/stderr 常含凭据，是主机/应用列表作用域裁剪之外的旁路泄露面。
// 现在 run:view 可作用域化（perm.go 的覆盖语义）：作用域行的解析结果
// 取代全局授予，列表/详情/SSE 按"run 实际触达的主机"判定可见性。

// runViewCtx 一次请求（或一条 SSE 订阅）的可见性判定上下文。
type runViewCtx struct {
	unrestricted bool
	allowedIDs   map[int64]bool
	allowedNames map[string]bool // 任务明细行存主机名（不是 ID）
}

// runViewCtxOf 解析当前用户的 run:view 作用域。全局权限或显式"全部"
// 的作用域行不裁剪（与既有覆盖语义一致）。
func (s *Server) runViewCtxOf(r *http.Request) *runViewCtx {
	if s.permsOf(permUser(r)).global[verbRunView] {
		return &runViewCtx{unrestricted: true}
	}
	set := s.hostScopeSet(r, verbRunView)
	if set == nil {
		return &runViewCtx{unrestricted: true}
	}
	vc := &runViewCtx{allowedIDs: set, allowedNames: map[string]bool{}}
	for id := range set {
		if h, err := s.st.GetHost(id); err == nil {
			vc.allowedNames[h.Name] = true
		}
	}
	return vc
}

// runVisible 按落库 selector 判定（exec 与应用执行同口径：selector 一律
// 记作用域裁剪后实际触达的 host ID 集合）。解析失败或无交集按不可见
// （fail-closed）。主机按当前归属判定——与主机列表裁剪同一口径。
func (vc *runViewCtx) runVisible(run *store.Run) bool {
	if vc.unrestricted {
		return true
	}
	var sel struct {
		IDs []int64 `json:"ids"`
	}
	if err := json.Unmarshal([]byte(run.Selector), &sel); err != nil {
		return false
	}
	for _, id := range sel.IDs {
		if vc.allowedIDs[id] {
			return true
		}
	}
	return false
}

// filterTasks 任务明细按允许主机名裁剪：run 可见只说明部分触达主机在
// 作用域内，其它主机的输出仍不得外泄。
func (vc *runViewCtx) filterTasks(tasks []*store.RunTask) []*store.RunTask {
	if vc.unrestricted {
		return tasks
	}
	out := make([]*store.RunTask, 0, len(tasks))
	for _, t := range tasks {
		if vc.allowedNames[t.Host] {
			out = append(out, t)
		}
	}
	return out
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
