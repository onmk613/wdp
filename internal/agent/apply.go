package agent

// 自治执行（docs/15 §7）：agent 收到完全解析的 plan 后在本地实例化
// executor 完成收敛，进度落 journal（断点续跑），控制端可随时断开
//（POST /plan 异步：落盘、后台启动、立即返回 run_id——G1 由此外成立）。
//
// 主机侧执行位置的路由：计划中标记为本机（local_host）的主机走 selfexec
// 连接（完整 become 语义）；其余主机按各自连接元类型远程执行——同一
// RunPlan 调用天然覆盖网段中继（G3）：跳板机 agent 即该网段的本地控制端。

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"wdp/internal/conn"
	_ "wdp/internal/conn/selfexec" // 注册 selfexec 工厂（本机主机条目路由到本机执行）
	"wdp/internal/executor"
	"wdp/internal/inventory"
	"wdp/internal/model"
	"wdp/internal/plan"
	"wdp/internal/report"
)

// maxPlanBodyBytes 是 /plan 请求体（解压后）上限：plan 内嵌 chart 树与
// values，体积远超 /exec；制品（packages/）不进 plan，走 PayloadRef。
const maxPlanBodyBytes = 512 << 20

// PlanSubmitRequest 是 POST /plan 请求体。BecomePassword 仅驻留内存
// （随 plan 一次性下发），不落盘到 run 目录。
type PlanSubmitRequest struct {
	RunID          string     `json:"run_id"`                    // 调用方生成；与 plan_id 联合幂等
	Plan           *plan.Plan `json:"plan"`                      // 完整计划（自包含）
	LocalHost      string     `json:"local_host,omitempty"`      // 计划中属于本机的主机名（空 = 纯中继）
	Resume         bool       `json:"resume,omitempty"`          // 断点续跑（plan_id 须与落盘一致）
	BecomePassword string     `json:"become_password,omitempty"` // become 密码（内存态；推荐免密 sudo）
	Forks          int        `json:"forks,omitempty"`           // 并发上限（0 = 内置默认）
}

// PlanSubmitResponse 是 POST /plan 响应。
type PlanSubmitResponse struct {
	RunID          string         `json:"run_id"`
	Accepted       bool           `json:"accepted"`
	State          string         `json:"state"`
	ResumedFromIdx map[string]int `json:"resumed_from_idx,omitempty"` // 各主机已跳过的完成任务数
}

// PlanStatusResponse 是 GET /plan/status 响应。
type PlanStatusResponse struct {
	RunID   string                  `json:"run_id"`
	PlanID  string                  `json:"plan_id"`
	State   string                  `json:"state"` // running|done|failed|cancelled
	Journal []JournalEntry          `json:"journal"`
	Stats   map[string]*model.Stats `json:"stats,omitempty"`
	Error   string                  `json:"error,omitempty"`
}

// planRun 是一次已接受的自治执行（journal 即租约：running 期间拒绝新 run）。
type planRun struct {
	runID  string
	planID string
	dir    string

	mu     sync.Mutex
	state  string
	stats  map[string]*model.Stats
	errMsg string

	cancel    context.CancelFunc
	createdAt time.Time
}

// planManager 管理本 agent 上的自治执行（同刻至多一个 running run）。
type planManager struct {
	mu      sync.Mutex
	byRunID map[string]*planRun
	byPlan  map[string]string // plan_id → run_id（幂等键）
	current *planRun          // 当前（或最近一次）run
}

func newPlanManager() *planManager {
	return &planManager{byRunID: map[string]*planRun{}, byPlan: map[string]string{}}
}

// register 接受一个提交：返回既有 run（幂等命中）或登记新 run。
// 拒绝：run_id 属于别的 plan；不同 plan_id 复用 run_id；已有 run 在跑
// （journal 即租约，并发 run 互相踩踏比排队更危险）。
func (m *planManager) register(runID, planID string) (*planRun, *planRun, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if otherRun := m.byPlan[planID]; otherRun != "" && otherRun != runID {
		// 同一 plan 换 run_id 重复提交：幂等命中既有 run（§7.7：不启动第二个收敛）
		return m.byRunID[otherRun], nil, nil
	}
	if existing, ok := m.byRunID[runID]; ok {
		if existing.planID != planID {
			return nil, nil, fmt.Errorf("run_id %s belongs to plan %s, refusing to reuse it for plan %s", runID, existing.planID[:12], planID[:12])
		}
		return existing, nil, nil // 幂等命中：同一 plan 重复提交不启动第二个收敛
	}
	if m.current != nil && m.current.state == "running" {
		return nil, nil, fmt.Errorf("another run %s is still executing (wait, cancel it, or query its status)", m.current.runID)
	}
	r := &planRun{runID: runID, planID: planID, state: "running", createdAt: time.Now().UTC()}
	m.byRunID[runID] = r
	m.byPlan[planID] = runID
	m.current = r
	return nil, r, nil
}

// handlePlanSubmit 接受 plan 分片：校验完整性 → 落盘（0700/0600）→ 后台
// 启动收敛 → 立即返回。落盘的 plan.json 不含 become 密码（仅内存态）。
func (s *Server) handlePlanSubmit(w http.ResponseWriter, r *http.Request) {
	body := io.Reader(r.Body)
	if r.Header.Get("Content-Encoding") == "gzip" {
		gz, err := gzip.NewReader(body)
		if err != nil {
			http.Error(w, "invalid gzip body: "+err.Error(), http.StatusBadRequest)
			return
		}
		defer gz.Close()
		body = gz
	}
	var req PlanSubmitRequest
	if err := json.NewDecoder(io.LimitReader(body, maxPlanBodyBytes)).Decode(&req); err != nil {
		http.Error(w, "request body parse failed: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.Plan == nil || req.Plan.PlanID == "" {
		http.Error(w, "plan is required", http.StatusBadRequest)
		return
	}
	if req.RunID == "" {
		http.Error(w, "run_id is required", http.StatusBadRequest)
		return
	}
	// 完整性：内容寻址复核（防传输损坏与篡改后的静默执行）
	if id := req.Plan.ComputeID(); id != req.Plan.PlanID {
		http.Error(w, fmt.Sprintf("plan content hash mismatch: submitted %s, computed %s", req.Plan.PlanID[:12], id[:12]), http.StatusBadRequest)
		return
	}
	// 自更新拒绝：plan 若升级 agent 自身（二进制路径/单元名），会杀掉正在
	// 执行本 plan 的进程——收敛中断且难恢复；自更新走 agentctl 专用路径
	if err := s.rejectSelfUpdate(req.Plan); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	existing, run, err := s.plans.register(req.RunID, req.Plan.PlanID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	if existing != nil {
		// 幂等：同一 plan 重复提交返回同一 run 与当前状态
		writeJSON(w, http.StatusOK, PlanSubmitResponse{
			RunID: existing.runID, Accepted: true,
			State: existing.stateLocked(),
		})
		return
	}

	// 落盘（plan.json 0600：values 可能含敏感配置；run 目录 0700）
	run.dir = s.runDir(req.RunID)
	if err := os.MkdirAll(run.dir, 0o700); err != nil {
		http.Error(w, "failed to create run dir: "+err.Error(), http.StatusInternalServerError)
		return
	}
	pb, err := json.MarshalIndent(req.Plan, "", "  ")
	if err != nil {
		http.Error(w, "failed to encode plan: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := os.WriteFile(filepath.Join(run.dir, "plan.json"), pb, 0o600); err != nil {
		http.Error(w, "failed to persist plan: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// 断点续跑：resume 且 plan_id 与已有落盘一致时，journal 中已 ok/changed
	// 的 idx 不重做；plan_id 不一致则明确拒绝（旧进度对新 plan 无意义）
	skipDone := map[string]map[int]bool{}
	resumed := map[string]int{}
	if req.Resume {
		prevID, perr := os.ReadFile(filepath.Join(run.dir, "plan.id"))
		switch {
		case os.IsNotExist(perr):
			// 首次提交，无进度可续
		case perr != nil:
			http.Error(w, "failed to read previous plan id: "+perr.Error(), http.StatusInternalServerError)
			return
		case string(prevID) != req.Plan.PlanID:
			http.Error(w, fmt.Sprintf("resume refused: run dir holds plan %s, submitted plan %s (old progress is meaningless for a changed plan; use a new run_id)", string(prevID)[:12], req.Plan.PlanID[:12]), http.StatusConflict)
			return
		default:
			done, jerr := CompletedIdx(filepath.Join(run.dir, "journal.ndjson"))
			if jerr != nil {
				http.Error(w, "failed to replay journal: "+jerr.Error(), http.StatusInternalServerError)
				return
			}
			skipDone = done
			for h, idxs := range done {
				resumed[h] = len(idxs)
			}
		}
	}
	if err := os.WriteFile(filepath.Join(run.dir, "plan.id"), []byte(req.Plan.PlanID), 0o600); err != nil {
		http.Error(w, "failed to persist plan id: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := WriteStateFile(run.dir, "running"); err != nil {
		http.Error(w, "failed to persist state: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// 后台收敛：ctx 独立于本 HTTP 请求（控制端断开不影响——G1）
	ctx, cancel := context.WithCancel(context.Background())
	run.mu.Lock()
	run.cancel = cancel
	run.mu.Unlock()
	s.planActive.Store(true) // 空闲看门狗抑制（§7.5：running 期间不得自杀）
	go s.runPlanAsync(ctx, run, req, skipDone)

	s.logInfo("plan accepted: run=%s plan=%s hosts=%d local=%q resume=%v skipped=%d",
		req.RunID, req.Plan.PlanID[:12], len(req.Plan.Hosts), req.LocalHost, req.Resume, len(skipDone))
	writeJSON(w, http.StatusOK, PlanSubmitResponse{
		RunID: req.RunID, Accepted: true, State: "running", ResumedFromIdx: resumed,
	})
}

// runPlanAsync 后台执行计划并维护 run 状态。
func (s *Server) runPlanAsync(ctx context.Context, run *planRun, req PlanSubmitRequest, skipDone map[string]map[int]bool) {
	defer s.planActive.Store(false)
	defer func() { run.mu.Lock(); run.cancel = nil; run.mu.Unlock() }()

	jr, err := OpenJournal(filepath.Join(run.dir, "journal.ndjson"))
	if err != nil {
		run.finish("failed", "", err)
		return
	}
	defer jr.Close()

	inv := planInventory(req.Plan, req.LocalHost, req.BecomePassword)
	conns := conn.NewManager()
	conns.SetConnectConcurrency(4)
	rep := &journalReporter{srv: s, run: run, journal: jr}
	ex := executor.New(inv, conns, rep, executor.Options{
		Forks:      max(req.Forks, 1),
		Phase:      req.Plan.Phase,
		WdpVersion: Version,
		SkipDone:   skipDone,
	})
	failed := ex.RunPlan(ctx, req.Plan)
	conns.CloseAll()

	if ctx.Err() != nil {
		run.finish("cancelled", "", nil)
		s.logInfo("plan cancelled: run=%s", run.runID)
		return
	}
	if failed {
		run.finish("failed", "", errors.New("execution finished with failed hosts"))
		s.logInfo("plan failed: run=%s", run.runID)
		return
	}
	run.finish("done", "", nil)
	s.logInfo("plan done: run=%s", run.runID)
}

// finish 落终态（幂等）。
func (r *planRun) finish(state, _ string, err error) {
	r.mu.Lock()
	if r.state == "running" {
		r.state = state
		if err != nil {
			r.errMsg = err.Error()
		}
	}
	r.mu.Unlock()
	_ = WriteStateFile(r.dir, r.state)
}

func (r *planRun) stateLocked() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state
}

// planInventory 从计划构造合成 inventory：本机主机条目改走 selfexec
// （become 密码经 host 字段注入，仅内存态）；其余主机按计划内连接元数据
// 远程执行（网段中继：跳板机 agent 即本地控制端）。
func planInventory(p *plan.Plan, localHost, becomePassword string) *inventory.Inventory {
	var hosts []*model.Host
	for _, name := range p.Host() {
		hp := p.HostPlansOf(name)[0]
		h := hp.Conn.Host(name)
		if name == localHost {
			h.Conn = "selfexec"
			if becomePassword != "" {
				h.BecomePassword = becomePassword
			}
		}
		hosts = append(hosts, h)
	}
	return inventory.FromHosts(hosts)
}

// handlePlanStatus 查询进度（journal 增量：since_seq 游标）。
func (s *Server) handlePlanStatus(w http.ResponseWriter, r *http.Request) {
	runID := r.URL.Query().Get("run_id")
	if runID == "" {
		http.Error(w, "missing run_id parameter", http.StatusBadRequest)
		return
	}
	var since int64
	if v := r.URL.Query().Get("since_seq"); v != "" {
		if _, err := fmt.Sscanf(v, "%d", &since); err != nil {
			http.Error(w, "invalid since_seq parameter", http.StatusBadRequest)
			return
		}
	}
	run := s.plans.lookup(runID)
	dir := s.runDir(runID)
	if run == nil {
		// 进程重启后的历史 run：从落盘状态回答
		state := ReadStateFile(dir)
		if state == "" {
			http.Error(w, "unknown run_id", http.StatusNotFound)
			return
		}
		entries, err := ReadJournal(filepath.Join(dir, "journal.ndjson"), since)
		if err != nil {
			http.Error(w, "failed to read journal: "+err.Error(), http.StatusInternalServerError)
			return
		}
		planID, _ := os.ReadFile(filepath.Join(dir, "plan.id"))
		writeJSON(w, http.StatusOK, PlanStatusResponse{RunID: runID, PlanID: string(planID), State: state, Journal: entries})
		return
	}
	entries, err := ReadJournal(filepath.Join(dir, "journal.ndjson"), since)
	if err != nil {
		http.Error(w, "failed to read journal: "+err.Error(), http.StatusInternalServerError)
		return
	}
	run.mu.Lock()
	resp := PlanStatusResponse{
		RunID: run.runID, PlanID: run.planID, State: run.state,
		Journal: entries, Stats: run.stats, Error: run.errMsg,
	}
	run.mu.Unlock()
	writeJSON(w, http.StatusOK, resp)
}

// handlePlanCancel 请求中止。
func (s *Server) handlePlanCancel(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RunID string `json:"run_id"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil || req.RunID == "" {
		http.Error(w, "run_id is required", http.StatusBadRequest)
		return
	}
	run := s.plans.lookup(req.RunID)
	if run == nil {
		http.Error(w, "unknown run_id", http.StatusNotFound)
		return
	}
	run.mu.Lock()
	cancel, state := run.cancel, run.state
	run.mu.Unlock()
	if state != "running" || cancel == nil {
		writeJSON(w, http.StatusOK, PlanStatusResponse{RunID: run.runID, State: state})
		return
	}
	cancel()
	writeJSON(w, http.StatusOK, PlanStatusResponse{RunID: run.runID, State: "cancelling"})
}

func (m *planManager) lookup(runID string) *planRun {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.byRunID[runID]
}

// rejectSelfUpdate 拒绝以 agent 自身为目标的 plan（升级二进制/停用单元会
// 杀掉正在执行的收敛进程）。
func (s *Server) rejectSelfUpdate(p *plan.Plan) error {
	selfBin, _ := os.Executable()
	unit := s.systemdUnit
	if unit == "" {
		unit = "wdp-agent"
	}
	var check func(ts []*plan.ResolvedTask) error
	check = func(ts []*plan.ResolvedTask) error {
		for _, t := range ts {
			for _, v := range t.Args {
				if str, ok := v.(string); ok {
					if selfBin != "" && str == selfBin {
						return fmt.Errorf("plan targets the running agent binary %s (self-update would kill the executor; use `wdp agentctl`)", selfBin)
					}
				}
			}
			if t.Module == "systemd_unit" {
				if u, ok := t.Args["name"].(string); ok && u == unit {
					return fmt.Errorf("plan stops/restarts the agent unit %s (self-update would kill the executor; use `wdp agentctl`)", unit)
				}
			}
			if err := check(t.Block); err != nil {
				return err
			}
			if err := check(t.Rescue); err != nil {
				return err
			}
			if err := check(t.Always); err != nil {
				return err
			}
		}
		return nil
	}
	for _, hp := range p.Hosts {
		for _, group := range [][]*plan.ResolvedTask{hp.Pre, hp.Tasks, hp.Post, hp.Handlers} {
			if err := check(group); err != nil {
				return err
			}
		}
	}
	return nil
}

// journalReporter 把执行事件落 journal（PlanIdx > 0 的任务级结果）并转播
// 到 agent 日志；Recap 快照进 run.stats。
type journalReporter struct {
	srv     *Server
	run     *planRun
	journal *Journal
}

func (j *journalReporter) PlayStart(name string, hosts []string) {
	j.srv.logInfo("plan %s: play %q hosts=%d", j.run.runID, name, len(hosts))
}
func (j *journalReporter) TaskStart(task, module string) {}
func (j *journalReporter) TaskDone()                     {}
func (j *journalReporter) Finish()                       {}
func (j *journalReporter) PlayMsg(format string, a ...any) {
	j.srv.logInfo("plan %s: "+format, append([]any{j.run.runID}, a...)...)
}

func (j *journalReporter) HostResult(host string, r *model.TaskResult) {
	// plan 模式下到达这里的都是计划任务（子 chart/block 子任务聚合在顶层
	// 结果里，不发独立 HostResult）
	state := "ok"
	switch {
	case r.Unreachable:
		state = "unreachable"
	case r.Failed:
		state = "failed"
	case r.Skipped:
		state = "skipped"
	case r.Changed:
		state = "changed"
	}
	msg := r.Msg
	if msg == "" && r.Failed && r.Stderr != "" {
		msg = firstLine(r.Stderr)
	}
	if err := j.journal.Append(JournalEntry{
		Host: host, Idx: r.PlanIdx, Label: r.Task, State: state,
		Changed: r.Changed, Msg: msg,
	}); err != nil {
		j.srv.logInfo("plan %s: journal append failed: %v", j.run.runID, err)
	}
}

func (j *journalReporter) Recap(name string, stats map[string]*model.Stats) {
	j.run.mu.Lock()
	j.run.stats = stats
	j.run.mu.Unlock()
}

func firstLine(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			return s[:i]
		}
	}
	return s
}

// planIdleActive 供空闲看门狗查询（§7.5：自治执行期间没有请求到达，
// agent 不能被自己的空闲计时器杀掉）。
func (s *Server) planIdleActive() bool { return s.planActive.Load() }

var _ report.Reporter = (*journalReporter)(nil)
