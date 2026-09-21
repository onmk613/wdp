package web

// 应用执行：run 受理与预校验、流水线执行与 dbReporter 落 runs/run_tasks。

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
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
		Kind    string  `json:"kind"` // all | pool | group | label | hosts
		Value   string  `json:"value"`
		HostIDs []int64 `json:"host_ids"`
	} `json:"selector"`
}

type RunItem struct {
	AppID   int64  `json:"app_id"`
	Version string `json:"version"` // 空 = 最新
	Phase   string `json:"phase"`   // 空 = deploy
}

// handleRunApps 同步顺序执行全部应用（每个应用一行 run 记录，前端轮询
// runs 列表查看状态；同步返回首个 run id）。
func (s *Server) handleRunApps(w http.ResponseWriter, r *http.Request) {
	var req RunRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if len(req.Items) == 0 {
		writeError(w, http.StatusBadRequest, "items is empty")
		return
	}
	hosts, err := s.st.HostsBySelector(req.Selector.Kind, req.Selector.Value, req.Selector.HostIDs)
	if err != nil {
		s.writeInternal(w, err)
		return
	}
	if len(hosts) == 0 {
		writeError(w, http.StatusBadRequest, "selector matched no hosts")
		return
	}
	// 执行交集：目标主机 ∩ run:execute 允许范围（全局权限 = 全部）；
	// 交集为空 = 对选中的主机一台都无执行权，拒绝（防越权扫全网）
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
	sel, _ := json.Marshal(req.Selector)

	// 预校验全部应用/版本/相位（避免执行到一半才发现版本或相位不存在）
	items := make([]runItem, 0, len(req.Items))
	for _, it := range req.Items {
		app, err := s.st.GetApp(it.AppID)
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("app %d not found", it.AppID))
			return
		}
		version := it.Version
		if version == "" {
			version = app.LatestVersion
		}
		tgz, err := s.st.VersionTgz(it.AppID, version)
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("app %s version %s not found", app.Name, version))
			return
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
					"app %s version %s has no phase %q (available: %s)", app.Name, version, phase, strings.Join(known, ", ")))
				return
			}
		}
		items = append(items, runItem{app: app, version: version, tgz: tgz, phase: phase})
	}

	// 应用级执行准入：同应用已有 queued/running 的执行 → 409 拒绝并反馈
	// 冲突方（谁在跑、跑到哪、什么相位）。并发跑同一应用会交错写 marker/
	// 状态/marker 值，结果不可解释——此前只挡了主机级（gate），两个不同
	// 选择器打同一应用照样并行。
	appIDs := make([]int64, 0, len(items))
	for _, it := range items {
		appIDs = append(appIDs, it.app.ID)
	}
	if active, err := s.st.ActiveRunsByApp(appIDs); err != nil {
		s.writeInternal(w, err)
		return
	} else if len(active) > 0 {
		parts := make([]string, 0, len(active))
		for _, a := range active {
			parts = append(parts, fmt.Sprintf("%s[%s]（%s · run #%d · %s）", a.AppName, a.Phase, a.Status, a.ID, a.User))
		}
		writeError(w, http.StatusConflict,
			"应用正在执行中，完成后再发起："+strings.Join(parts, "、"))
		return
	}

	runIDs := make([]int64, 0, len(items))
	user, _ := r.Context().Value(ctxUser{}).(string)
	for i, it := range items {
		runID, err := s.st.CreateRun("app", it.app.ID, it.app.Name, it.version, it.phase, i, string(sel), user, "queued")
		if err != nil {
			s.writeInternal(w, err)
			return
		}
		runIDs = append(runIDs, runID)
	}

	// per-host 执行闸门：拿到全部目标主机的锁才开始（同主机串行，
	// 排队期间 run 状态为 queued）
	hostIDs := make([]int64, 0, len(hosts))
	for _, h := range hosts {
		hostIDs = append(hostIDs, h.ID)
	}
	go func() {
		// 约束：排队等待必须可中断——挂在 server 生命周期 ctx 上并叠加
		// 排队超时；闸门被长执行占用时旧实现会无限堆积 goroutine，
		// server 关停也无法中断
		ctx, cancel := context.WithTimeout(s.background(), runQueueTimeout)
		defer cancel()
		release, ok := s.gate.AcquireCtx(ctx, hostIDs)
		if !ok {
			// 获取失败（排队超时/服务关停）：run 置 failed，不再执行
			reason := "server 正在关停，执行排队中止"
			if ctx.Err() == context.DeadlineExceeded {
				reason = fmt.Sprintf("排队超时（%s 内未取得主机执行闸门）", runQueueTimeout)
			}
			for _, id := range runIDs {
				_ = s.st.FinishRun(id, "failed", reason)
				s.runs.notify(runEvent{ID: id, Status: "failed", Summary: reason})
			}
			s.logger.Warn("app run aborted while queuing", "runs", len(runIDs), "reason", reason)
			return
		}
		defer release()
		for i, it := range items {
			_ = s.st.SetRunStatus(runIDs[i], "running")
			s.runs.notify(runEvent{ID: runIDs[i], Status: "running"})
			mhosts := s.runHostModels(ctx, hosts)
			err := s.runsvc.RunOneApp(ctx, it.tgz, mhosts, it.phase, &dbReporter{st: s.st, runID: runIDs[i], hub: s.runs})
			status, summary := "succeeded", "ok"
			if err != nil {
				status, summary = "failed", err.Error()
			}
			_ = s.st.FinishRun(runIDs[i], status, summary)
			s.runs.notify(runEvent{ID: runIDs[i], Status: status, Summary: summary})
			s.logger.Info("app run finished", "app", it.app.Name, "version", it.version, "phase", it.phase, "status", status, "err", err)
		}
	}()
	writeJSON(w, http.StatusAccepted, map[string]any{"run_ids": runIDs, "hosts": len(hosts)})
}

// runItem 是一个待执行的应用版本（执行流水线的内部形态）。
type runItem struct {
	app     *store.App
	version string
	tgz     string
	phase   string
}

// dbReporter 把 executor 回调落 runs/run_tasks。
type dbReporter struct {
	st    *store.Store
	runID int64
	hub   *runHub // 任务明细落库后发事件（SSE 订阅方增量拉详情）

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
	_ = d.st.AddRunTask(&store.RunTask{
		RunID: d.runID, Play: d.curPlay, Task: d.curTask, Module: d.curModule,
		Host: host, Status: status, Changed: r.Changed, Detail: truncate(detail, 16<<10),
	})
}

func (d *dbReporter) TaskDone() {}

func (d *dbReporter) PlayMsg(format string, a ...any) {}

func (d *dbReporter) Recap(_ string, _ map[string]*model.Stats) {}

func (d *dbReporter) Finish() {}

var _ report.Reporter = (*dbReporter)(nil)

// runHostModels 执行目标从台账行转 executor 主机模型：逐台探活定传输
// （https+mTLS 优先、明文回退；探不通照常建连由 executor 报 unreachable），
// group_names 按台账组填入——FromHosts 据此建组，play 的 hosts: 在选择器
// 范围内按组细分（一个应用的大组内部分 web/db 小组）。
func (s *Server) runHostModels(ctx context.Context, hosts []*store.Host) []*model.Host {
	mhosts := make([]*model.Host, 0, len(hosts))
	for _, h := range hosts {
		mh := s.agentHostModel(h, s.agentScheme(ctx, h))
		mh.Vars = map[string]any{"group_names": h.Groups}
		mhosts = append(mhosts, mh)
	}
	return mhosts
}
