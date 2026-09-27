package agent

// plan 运行状态机：planRun（一次自治执行的进度与互斥）与 planManager
//（在册运行表：注册互斥/回滚登记/容量修剪/查询）。HTTP 端点与执行接线
// 在 apply.go。

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
	"wdp/internal/model"
)

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

// keepRuns 是 run 目录与 planManager 内存记录的保留条数：run 目录含
// journal/state 供控制端断连后回查，但无限保留会让 /var/lib/wdp/runs 与
// byRunID/byPlan map 只增不减（clean.go 的自清理不覆盖 runs 根目录），
// 故按目录修改时间裁剪最旧的。
const keepRuns = 20

// register 接受一个提交：返回既有 run（幂等命中）或登记新 run。
// 拒绝：run_id 属于别的 plan；不同 plan_id 复用 run_id；已有 run 在跑
// （journal 即租约，并发 run 互相踩踏比排队更危险）。
// 终态旧 run 的重跑规则：failed → 无条件清映射允许重跑（对漂移主机重新
// 收敛是核心场景，不能要求重启 agent）；done/cancelled → 仅 force 时清
// 映射重跑，非 force 维持幂等返回（§7.7：不启动第二个收敛）。
func (m *planManager) register(runID, planID string, force bool) (*planRun, *planRun, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if otherID := m.byPlan[planID]; otherID != "" && otherID != runID {
		// 同一 plan 换 run_id 重复提交
		prev := m.byRunID[otherID]
		if prev == nil {
			delete(m.byPlan, planID) // 悬空映射（run 已被裁剪遗忘）：直接清掉
		} else if st := prev.stateLocked(); st == "running" || (st != "failed" && !force) {
			return prev, nil, nil // 在跑或已终态且未要求重跑：幂等命中既有 run
		} else {
			delete(m.byPlan, planID)
			delete(m.byRunID, otherID)
		}
	}
	if existing, ok := m.byRunID[runID]; ok {
		if existing.planID != planID {
			return nil, nil, fmt.Errorf("run_id %s belongs to plan %s, refusing to reuse it for plan %s", runID, shortID(existing.planID), shortID(planID))
		}
		if st := existing.stateLocked(); st == "running" || (st != "failed" && !force) {
			return existing, nil, nil // 幂等命中：同一 plan 重复提交不启动第二个收敛
		}
		// 终态重跑（failed，或 done/cancelled + force）：控制端 run_id 由 plan
		// 内容确定性派生，重跑必然同 id——清映射按新 run 登记
		delete(m.byRunID, runID)
		delete(m.byPlan, planID)
	}
	// stateLocked 而非裸读：state 的写侧（finish）持的是 run 自己的锁，
	// 与 manager 锁不构成 happens-before，裸读是数据竞争（同函数其余
	// 读取均已走 stateLocked）
	if m.current != nil && m.current.stateLocked() == "running" {
		return nil, nil, fmt.Errorf("another run %s is still executing (wait, cancel it, or query its status)", m.current.runID)
	}
	r := &planRun{runID: runID, planID: planID, state: "running", createdAt: time.Now().UTC()}
	m.byRunID[runID] = r
	m.byPlan[planID] = runID
	m.current = r
	return nil, r, nil
}

// rollback 撤销一次尚未启动的登记（提交路径落盘失败专用）：若不回滚，
// 该 run 会以 state:"running" 永久驻留 manager——register 的 running 检查
// 从此拒绝一切新提交（409），且 /plan/cancel 因 cancel==nil 无法取消。
func (m *planManager) rollback(r *planRun) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.byRunID[r.runID] == r {
		delete(m.byRunID, r.runID)
	}
	if m.byPlan[r.planID] == r.runID {
		delete(m.byPlan, r.planID)
	}
	if m.current == r {
		m.current = nil
	}
}

// trim 收缩内存记录到最近 keep 条（按 createdAt 裁最旧；running 的不受
// 裁剪影响——终态 run 才会触发调用，此处仅防御性跳过）。
func (m *planManager) trim(keep int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.byRunID) <= keep {
		return
	}
	recs := make([]*planRun, 0, len(m.byRunID))
	for _, r := range m.byRunID {
		recs = append(recs, r)
	}
	sort.Slice(recs, func(i, j int) bool { return recs[i].createdAt.Before(recs[j].createdAt) })
	for _, r := range recs[:len(recs)-keep] {
		delete(m.byRunID, r.runID)
		if m.byPlan[r.planID] == r.runID {
			delete(m.byPlan, r.planID)
		}
		if m.current == r {
			m.current = nil
		}
	}
}

// forget 遗忘一个 run 的内存记录（其目录刚被磁盘裁剪；running 的不遗忘）。
func (m *planManager) forget(runID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.byRunID[runID]
	if r == nil || r.stateLocked() == "running" {
		return
	}
	delete(m.byRunID, runID)
	if m.byPlan[r.planID] == runID {
		delete(m.byPlan, r.planID)
	}
}

// reapZombieRuns 启动期扫描 runs 根目录，把无进程支撑的 running 状态改判
// failed。崩溃窗口：WriteStateFile("running") 在执行 goroutine 启动前落盘
// （apply.go 提交路径），此后进程崩溃/被杀 → 重启后磁盘状态永驻 running：
// /plan/status 谎报进行中、/plan/cancel 404（内存表里没有该 run）、register
// 的 running 检查与 byPlan 幂等映射永远卡死后续提交。本进程刚启动时
// planManager 必为空（serve 在首个请求前调用本函数），磁盘上的 running
// 不可能有进程支撑，统一改判 failed 并在 journal 追注说明——failed 语义
// 下控制端可无须 force 直接重跑，自治收敛得以恢复。
func (s *Server) reapZombieRuns() {
	root := s.runsRoot()
	entries, err := os.ReadDir(root)
	if err != nil {
		return // 根目录不存在是常态（从未跑过自治执行）
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		if ReadStateFile(dir) != "running" {
			continue
		}
		if err := WriteStateFile(dir, "failed"); err != nil {
			s.logWarn("reap zombie run %s: failed to rewrite state: %v", e.Name(), err)
			continue
		}
		// 注释进 journal（追加行协议，对 CompletedIdx 续跑重放无害：
		// failed 且 idx=0 不计入完成集合），控制端 /plan/status 可见原因
		if jr, jerr := OpenJournal(filepath.Join(dir, "journal.ndjson")); jerr == nil {
			_ = jr.Append(JournalEntry{Host: "agent", Label: "state", State: "failed",
				Msg: "agent restarted during run; state reaped as failed at startup"})
			_ = jr.Close()
		}
		s.logWarn("reaped zombie run %s: state running had no live process (agent restarted during run), re-judged as failed", e.Name())
	}
}

// finish 把 run 落到终态：内存状态仅在仍为 running 时迁移（首个终态
// 生效，后续调用只补写磁盘 state）。磁盘 state 无条件重写（幂等，
// 崩溃恢复路径依赖终态文件落盘）。
func (r *planRun) finish(state string, err error) {
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

func (m *planManager) lookup(runID string) *planRun {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.byRunID[runID]
}
