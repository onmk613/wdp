package web

// 应用执行：run 受理与预校验、流水线执行与 dbReporter 落 runs/run_tasks。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"

	"wdp/internal/model"
	"wdp/internal/report"
	"wdp/internal/store"
)

// ---- 应用执行 ----

// RunRequest 应用执行请求：items 按序执行，selector 决定目标主机。
// 相位是行级语义——清单里每个条目独立选择，同一应用可多次出现、
// 各执行不同（或相同）相位。
type RunRequest struct {
	Items    []RunItem `json:"items"`
	Selector struct {
		Kind    string  `json:"kind"` // all | pool | group | label | hosts | inline
		Value   string  `json:"value"`
		HostIDs []int64 `json:"host_ids"`
		// Inventory 是 kind=inline 时粘贴的 inventory YAML（未纳管主机；
		// 可能含凭据，只进执行期内存，不落库）
		Inventory string `json:"inventory"`
	} `json:"selector"`
}

type RunItem struct {
	AppID   int64  `json:"app_id"`
	Version string `json:"version"` // 空 = 最新
	Phase   string `json:"phase"`   // 空 = deploy
}

// handleRunApps 同步顺序执行全部应用（每个应用一行 run 记录，前端轮询
// runs 列表查看状态；同步返回全部 run id）。校验/建 run/执行三个阶段
// 各自拆为私有函数（runAppsTargets / runAppsItems / runAppsCreateRuns /
// execRunItems），本函数只做编排。
func (s *Server) handleRunApps(w http.ResponseWriter, r *http.Request) {
	var req RunRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if len(req.Items) == 0 {
		writeError(w, http.StatusBadRequest, "items is empty")
		return
	}
	var (
		hosts      []*store.Host
		inlineHost []*model.Host
	)
	if req.Selector.Kind == "inline" {
		var sel string
		var ok bool
		inlineHost, sel, ok = s.runAppsInlineTargets(w, r, req.Selector.Inventory)
		if !ok {
			return
		}
		// inline 目标在受理期已定型，run 记录直接以它建
		items, ok := s.runAppsItems(w, r, &req)
		if !ok {
			return
		}
		user, _ := r.Context().Value(ctxUser{}).(string)
		runIDs, ok := s.runAppsCreateRuns(w, items, sel, user)
		if !ok {
			return
		}
		go s.execRunItems(runIDs, items, nil, inlineHost, inlineGateIDs(inlineHost))
		writeJSON(w, http.StatusAccepted, map[string]any{"run_ids": runIDs, "hosts": len(inlineHost)})
		return
	}
	hosts, sel, ok := s.runAppsTargets(w, r, &req)
	if !ok {
		return
	}
	items, ok := s.runAppsItems(w, r, &req)
	if !ok {
		return
	}
	// 模块能力对账（受理期失败优于执行期半途炸）；无清单/无上报的
	// 兼容口径见函数注释
	if err := s.runAppsModuleCheck(hosts, items); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	user, _ := r.Context().Value(ctxUser{}).(string)
	runIDs, ok := s.runAppsCreateRuns(w, items, sel, user)
	if !ok {
		return
	}
	hostIDs := make([]int64, 0, len(hosts))
	for _, h := range hosts {
		hostIDs = append(hostIDs, h.ID)
	}
	go s.execRunItems(runIDs, items, hosts, nil, hostIDs)
	writeJSON(w, http.StatusAccepted, map[string]any{"run_ids": runIDs, "hosts": len(hosts)})
}

// runAppsTargets 目标解析（校验阶段）：选择器 → run:execute 执行交集
// 裁剪 → 落库 selector（记实际执行集合）。失败时已写好响应。
func (s *Server) runAppsTargets(w http.ResponseWriter, r *http.Request, req *RunRequest) ([]*store.Host, string, bool) {
	// 原始选择器仅在裁剪发生时附注进审计（见下），落库 selector 一律记
	// 实际执行集合——裁前集合让事后审计无法还原真实触达范围
	origSel, _ := json.Marshal(req.Selector)
	hosts, err := s.st.HostsBySelector(req.Selector.Kind, req.Selector.Value, req.Selector.HostIDs)
	if err != nil {
		s.writeInternal(w, err)
		return nil, "", false
	}
	if len(hosts) == 0 {
		writeError(w, http.StatusBadRequest, "selector matched no hosts")
		return nil, "", false
	}
	// 执行交集：目标主机 ∩ run:execute 允许范围（全局权限 = 全部）；
	// 交集为空 = 对选中的主机一台都无执行权，拒绝（防越权扫全网）
	if allowed := s.hostScopeSet(r, verbRunExec); allowed != nil {
		trimmed := intersectHosts(hosts, allowed)
		if len(trimmed) == 0 {
			writeError(w, http.StatusForbidden, "forbidden: no target host inside your run:execute scope")
			return nil, "", false
		}
		if len(trimmed) < len(hosts) {
			s.audit(r, "run", "exec", "", fmt.Sprintf("执行范围被权限裁剪：%d → %d 台（原始选择器 %s）", len(hosts), len(trimmed), origSel))
		}
		hosts = trimmed
	}
	// run 的 selector 记实际执行集合（与 exec.go 同口径）：请求 selector
	// 在作用域裁剪后不再反映真实触达范围，按最终 hosts 构造
	hostIDs := make([]int64, 0, len(hosts))
	for _, h := range hosts {
		hostIDs = append(hostIDs, h.ID)
	}
	sel, _ := json.Marshal(map[string]any{"kind": "hosts", "ids": hostIDs})
	return hosts, string(sel), true
}

// runAppsItems 预校验全部应用/版本/相位（校验阶段；避免执行到一半才发现
// 版本或相位不存在）。失败时已写好响应。
func (s *Server) runAppsItems(w http.ResponseWriter, r *http.Request, req *RunRequest) ([]runItem, bool) {
	items := make([]runItem, 0, len(req.Items))
	for _, it := range req.Items {
		app, err := s.st.GetApp(it.AppID)
		if err != nil {
			// 统一文案：不回显应用名/ID 存在性，避免逐 ID 枚举应用
			writeError(w, http.StatusForbidden, "app not found or outside your scope")
			return nil, false
		}
		// 应用级授权：run:execute 可作用域化（perm.go 的 scopableVerbs），
		// 只校验"有该 verb"会让 run:execute@poolA 的账号把**任意应用**
		// （含 scope 属 poolB 的）部署到 poolA 主机上。与上面的主机交集
		// 合起来才是注释里承诺的"选择器 ∩ run:execute 允许集合"。
		if !s.permsOf(permUser(r)).canApp(verbRunExec, app.Pools, app.Groups, app.Labels) {
			writeError(w, http.StatusForbidden, "forbidden: app not found or outside your scope")
			return nil, false
		}
		version := it.Version
		if version == "" {
			version = app.LatestVersion
		}
		tgz, err := s.st.VersionTgz(it.AppID, version)
		if err != nil {
			// 预校验口径：未知版本回 400（既有契约，apps_test 锚定）；
			// DB 故障不得伪装成"版本不存在"，走 writeStoreErr 分流 500
			if !errors.Is(err, store.ErrNotFound) {
				s.writeStoreErr(w, err)
				return nil, false
			}
			writeError(w, http.StatusBadRequest, fmt.Sprintf("app version %q not found", version))
			return nil, false
		}
		phase := it.Phase
		if phase == "" {
			phase = "deploy"
		}
		// 新版本创建时提取了相位清单，可精确预校验；旧行（迁移前）无清单，
		// 留给执行期 chart 加载报错
		if known, perr := s.st.VersionPhases(it.AppID, version); perr == nil && len(known) > 0 {
			found := false
			for _, p := range known {
				if p == phase {
					found = true
					break
				}
			}
			if !found {
				writeError(w, http.StatusBadRequest, fmt.Sprintf(
					"app version %q has no phase %q (available: %s)", version, phase, strings.Join(known, ", ")))
				return nil, false
			}
		}
		items = append(items, runItem{app: app, version: version, tgz: tgz, phase: phase})
	}
	return items, true
}

// runAppsCreateRuns 建全部 run 行（编排阶段）。应用级执行准入：同应用
// 已有 queued/running 的执行 → 409 拒绝并反馈冲突方（谁在跑、跑到哪、
// 什么相位）。并发跑同一应用会交错写 marker/状态/marker 值，结果不可
// 解释——此前只挡了主机级（gate），两个不同选择器打同一应用照样并行。
// 检查与创建收进 store 单事务（先查后插分开执行存在窗口：两个并发请求
// 可同时通过检查同时创建）。失败时已写好响应。
func (s *Server) runAppsCreateRuns(w http.ResponseWriter, items []runItem, sel, user string) ([]int64, bool) {
	inputs := make([]store.RunInput, 0, len(items))
	for i, it := range items {
		inputs = append(inputs, store.RunInput{
			Kind: "app", AppID: it.app.ID, AppName: it.app.Name,
			Version: it.version, Phase: it.phase, Seq: i,
			Status: "queued", Selector: sel, User: user,
		})
	}
	runIDs, err := s.st.CreateRunsExclusive(inputs)
	var conflict *store.RunConflictError
	if errors.As(err, &conflict) {
		parts := make([]string, 0, len(conflict.Active))
		for _, a := range conflict.Active {
			parts = append(parts, fmt.Sprintf("%s[%s]（%s · run #%d · %s）", a.AppName, a.Phase, a.Status, a.ID, a.User))
		}
		writeError(w, http.StatusConflict,
			"应用正在执行中，完成后再发起："+strings.Join(parts, "、"))
		return nil, false
	}
	if err != nil {
		s.writeInternal(w, err)
		return nil, false
	}
	return runIDs, true
}

// execRunItems 执行阶段（后台 goroutine）：排队等主机闸门 → 逐应用执行
// 并落 runs/run_tasks。
func (s *Server) execRunItems(runIDs []int64, items []runItem, hosts []*store.Host, inlineHost []*model.Host, hostIDs []int64) {
	// 取消注册（排队期即可被 /api/runs/{id}/cancel 命中）；终态由下方
	// 循环逐个注销
	s.registerRunCancels(runIDs)
	// per-host 执行闸门：拿到全部目标主机的锁才开始（同主机串行，
	// 排队期间 run 状态为 queued）
	// 约束：排队等待必须可中断——挂在 server 生命周期 ctx 上并叠加
	// 排队超时；闸门被长执行占用时旧实现会无限堆积 goroutine，
	// server 关停也无法中断
	queueCtx, cancelQueue := context.WithTimeout(s.background(), runQueueTimeout)
	defer cancelQueue()
	release, ok := s.gate.AcquireCtx(queueCtx, hostIDs)
	if !ok {
		// 获取失败（排队超时/服务关停）：run 置 failed，不再执行
		reason := "server 正在关停，执行排队中止"
		if queueCtx.Err() == context.DeadlineExceeded {
			reason = fmt.Sprintf("排队超时（%s 内未取得主机执行闸门）", runQueueTimeout)
		}
		for _, id := range runIDs {
			_ = s.st.FinishRun(id, "failed", reason)
			s.runs.notify(runEvent{ID: id, Status: "failed", Summary: reason})
			s.unregisterRunCancel(id)
		}
		s.logger.Warn("app run aborted while queuing", "runs", len(runIDs), "reason", reason)
		return
	}
	defer release()
	// 执行换挂独立 ctx：排队超时只封顶"等闸门"，不得封顶执行本身——
	// 多主机/多相位部署轻松超过 30 分钟，沿用排队 ctx 会把部署拦腰
	// 打断（半完成态比失败更糟）。执行不另设上限：单任务超时在执行器
	// 内部（console.RunService 600s/任务），server 关停仍可取消。
	ctx, cancelExec := context.WithCancel(s.background())
	defer cancelExec()
	// 探测一次全部目标主机（agent scheme 判定），items 循环内复用：
	// 每个应用条目重探 N 台主机 ×2 趟 /health 是纯重复开销
	// 自定义目标（inline）：解析出的主机模型直通（自带 conn 配置，
	// 不经台账探活）；managed 目标按台账行建模
	mhosts := inlineHost
	if mhosts == nil {
		mhosts = s.runHostModels(ctx, hosts)
	}
	for i, it := range items {
		// 逐 run 派生 ctx：单 run 可被用户取消（/api/runs/{id}/cancel），
		// 不拖累同批其它应用；取消请求在排队期已发出的直接跳过
		itemCtx, itemCancel := context.WithCancel(ctx)
		if !s.armRunCancel(runIDs[i], itemCancel) {
			itemCancel()
			_ = s.st.FinishRun(runIDs[i], "cancelled", "用户取消（排队阶段，未执行任何任务）")
			s.runs.notify(runEvent{ID: runIDs[i], Status: "cancelled"})
			s.unregisterRunCancel(runIDs[i])
			continue
		}
		_ = s.st.SetRunStatus(runIDs[i], "running")
		s.runs.notify(runEvent{ID: runIDs[i], Status: "running"})
		err := s.runsvc.RunOneApp(itemCtx, it.tgz, mhosts, it.phase, &dbReporter{st: s.st, runID: runIDs[i], hub: s.runs, logger: s.logger})
		itemCancel()
		status, summary := "succeeded", "ok"
		switch {
		case s.runCancelRequested(runIDs[i]):
			// 执行器边界响应取消：在途任务已跑完，已执行任务的结果都在
			// run_tasks——幂等模块下重新发起即断点续跑
			status, summary = "cancelled", "用户取消（已执行任务的结果已保留，可重新发起续跑）"
		case err != nil:
			status, summary = "failed", err.Error()
		}
		s.unregisterRunCancel(runIDs[i])
		_ = s.st.FinishRun(runIDs[i], status, summary)
		s.runs.notify(runEvent{ID: runIDs[i], Status: status, Summary: summary})
		s.logger.Info("app run finished", "app", it.app.Name, "version", it.version, "phase", it.phase, "status", status, "err", err)
	}
}

// runItem 是一个待执行的应用版本（执行流水线的内部形态）。
type runItem struct {
	app     *store.App
	version string
	tgz     string
	phase   string
}

// runAppsModuleCheck 执行受理期的模块能力对账（分层方案 P2）：应用版本
// 各相位要用的内置模块 ∩ 目标主机 agent 上报的模块集（/info 探活落库），
// 缺了在受理期拒绝——agent 缺新模块的典型场景是控制端升级后老 agent
// 还没跟上，报错应指向「升级该主机 agent」而不是执行半途的模块不存在。
//
// 兼容口径（放行，不阻塞存量）：
//   - 版本无模块清单（迁移前旧行 / 提取失败）：” 或坏 JSON → 跳过
//   - 主机无上报（老 agent 无 /info、尚未探活）：AgentModules 空 → 跳过
//   - inline 目标不经此校验（自定义 inventory 无台账行，天然无上报）
func (s *Server) runAppsModuleCheck(hosts []*store.Host, items []runItem) error {
	needed := map[string]bool{}
	for _, it := range items {
		raw, err := s.st.VersionModules(it.app.ID, it.version)
		if err != nil || raw == "" {
			continue // 版本未知模块清单：放行（兼容旧行）
		}
		var perPhase map[string][]string
		if err := json.Unmarshal([]byte(raw), &perPhase); err != nil {
			continue // 坏清单：放行（提取侧已保证结构，防御性兜底）
		}
		for _, m := range perPhase[it.phase] {
			needed[m] = true
		}
	}
	if len(needed) == 0 {
		return nil
	}
	var lines []string
	for _, h := range hosts {
		if h.AgentModules == "" {
			continue // 主机未上报：放行（老 agent 兼容）
		}
		var have []string
		if err := json.Unmarshal([]byte(h.AgentModules), &have); err != nil || have == nil {
			continue
		}
		haveSet := make(map[string]bool, len(have))
		for _, m := range have {
			haveSet[m] = true
		}
		var missing []string
		for m := range needed {
			if !haveSet[m] {
				missing = append(missing, m)
			}
		}
		if len(missing) > 0 {
			slices.Sort(missing)
			build := h.AgentBuild
			if build == "" {
				build = "未知版本"
			}
			lines = append(lines, fmt.Sprintf("%s（agent %s）：缺 %s", h.Name, build, strings.Join(missing, "、")))
		}
	}
	if len(lines) == 0 {
		return nil
	}
	return fmt.Errorf("目标主机的 agent 缺少本次执行需要的模块，请先升级对应主机的 agent（或调整应用）：\n  %s",
		strings.Join(lines, "\n  "))
}

// dbReporter 把 executor 回调落 runs/run_tasks。
type dbReporter struct {
	st     *store.Store
	runID  int64
	hub    *runHub // 任务明细落库后发事件（SSE 订阅方增量拉详情）
	logger *slog.Logger

	mu        sync.Mutex
	curPlay   string
	curTask   string
	curModule string
	failed    int
	total     int
}

func (d *dbReporter) PlayStart(name string, _ []string) {
	d.mu.Lock()
	d.curPlay = name
	d.mu.Unlock()
}

func (d *dbReporter) TaskStart(task, module string) {
	d.mu.Lock()
	d.curTask, d.curModule = task, module
	d.mu.Unlock()
}

func (d *dbReporter) HostResult(host string, r *model.TaskResult) {
	status := "ok"
	switch {
	case r.Unreachable:
		status = "unreachable"
	case r.Failed:
		status = "failed"
	case r.Skipped:
		status = "skipped"
	}
	detail := r.Msg
	if r.Stderr != "" {
		detail += "\n" + r.Stderr
	}
	if r.Stdout != "" {
		detail += "\n" + r.Stdout
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.total++
	if status == "failed" || status == "unreachable" {
		d.failed++
	}
	d.hub.notify(runEvent{ID: d.runID, Status: "running"})
	// run_tasks 是执行审计面：落库失败（磁盘满/库锁）不能中断执行，
	// 但也不能无声——与 exec 路径（console/exec.go）同口径留告警供事后核对
	if terr := d.st.AddRunTask(&store.RunTask{
		RunID: d.runID, Play: d.curPlay, Task: d.curTask, Module: d.curModule,
		Host: host, Status: status, Changed: r.Changed, Detail: truncate(detail, 16<<10),
	}); terr != nil {
		d.logger.Warn("run_tasks audit write failed",
			"run_id", d.runID, "host", host, "err", terr)
	}
}

func (d *dbReporter) TaskDone() {}

func (d *dbReporter) PlayMsg(format string, a ...any) {}

func (d *dbReporter) Recap(_ string, _ map[string]*model.Stats, _ int64) {}

func (d *dbReporter) Finish() {}

var _ report.Reporter = (*dbReporter)(nil)

// runHostModels 执行目标从台账行转 executor 主机模型：逐台探活定传输
// （https+mTLS 优先、明文回退；探不通照常建连由 executor 报 unreachable），
// group_names 按台账组填入——FromHosts 据此建组，play 的 hosts: 在选择器
// 范围内按组细分（一个应用的大组内部分 web/db 小组）。
func (s *Server) runHostModels(ctx context.Context, hosts []*store.Host) []*model.Host {
	mhosts := make([]*model.Host, 0, len(hosts))
	for _, h := range hosts {
		mh := s.agentHostModel(h)
		mh.Vars = map[string]any{"group_names": h.Groups}
		mhosts = append(mhosts, mh)
	}
	return mhosts
}
