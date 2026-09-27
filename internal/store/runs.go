package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	_ "modernc.org/sqlite"
)

// ---- 执行记录 ----

// Run 是一次执行（应用执行按序每个应用一行；远程命令 kind=exec 单行）。
type Run struct {
	ID         int64
	Kind       string
	AppID      int64
	AppName    string
	Version    string
	Phase      string // 应用执行使用的相位（行级；exec 类为空）
	Seq        int
	Status     string
	Selector   string
	Summary    string
	User       string // 触发者（会话用户；历史行为空）
	StartedAt  string
	FinishedAt string
}

// RunTask 是执行中的一条任务结果（reporter 回调写入）。
type RunTask struct {
	ID      int64
	RunID   int64
	Play    string
	Task    string
	Module  string
	Host    string
	Status  string
	Changed bool
	Detail  string
}

// CreateRun 建执行记录（phase：应用执行的相位，exec 传空；status 初始
// 状态：'queued'（等执行闸门）或 'running'）。
func (s *Store) CreateRun(kind string, appID int64, appName, version, phase string, seq int, selector, user, status string) (int64, error) {
	if status == "" {
		status = "running"
	}
	res, err := s.db.Exec(`INSERT INTO runs (kind, app_id, app_name, version, phase, seq, status, selector, user, started_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		kind, appID, appName, version, phase, seq, status, selector, user, nowUTC())
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	return id, err
}

// SetRunStatus 仅改状态（queued → running）。
func (s *Store) SetRunStatus(id int64, status string) error {
	_, err := s.db.Exec(`UPDATE runs SET status = ? WHERE id = ?`, status, id)
	return err
}

// FinishRun 收尾。
func (s *Store) FinishRun(id int64, status, summary string) error {
	_, err := s.db.Exec(`UPDATE runs SET status = ?, summary = ?, finished_at = ? WHERE id = ?`, status, summary, nowUTC(), id)
	return err
}

// AddRunTask 记一条任务结果。
func (s *Store) AddRunTask(t *RunTask) error {
	_, err := s.db.Exec(`INSERT INTO run_tasks (run_id, play, task, module, host, status, changed, detail) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		t.RunID, t.Play, t.Task, t.Module, t.Host, t.Status, t.Changed, t.Detail)
	return err
}

// ListRuns 最近执行（limit 上限 100）。
func (s *Store) ListRuns(limit int) ([]*Run, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	rows, err := s.db.Query(`SELECT id, kind, app_id, app_name, version, phase, seq, status, selector, summary, user, started_at, finished_at FROM runs ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Run{}
	for rows.Next() {
		r := &Run{}
		if err := rows.Scan(&r.ID, &r.Kind, &r.AppID, &r.AppName, &r.Version, &r.Phase, &r.Seq, &r.Status, &r.Selector, &r.Summary, &r.User, &r.StartedAt, &r.FinishedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetRun 单条执行。
func (s *Store) GetRun(id int64) (*Run, error) {
	r := &Run{}
	err := s.db.QueryRow(`SELECT id, kind, app_id, app_name, version, phase, seq, status, selector, summary, user, started_at, finished_at FROM runs WHERE id = ?`, id).
		Scan(&r.ID, &r.Kind, &r.AppID, &r.AppName, &r.Version, &r.Phase, &r.Seq, &r.Status, &r.Selector, &r.Summary, &r.User, &r.StartedAt, &r.FinishedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return r, err
}

// RunTasks 执行的任务明细。
func (s *Store) RunTasks(runID int64) ([]*RunTask, error) {
	rows, err := s.db.Query(`SELECT id, run_id, play, task, module, host, status, changed, detail FROM run_tasks WHERE run_id = ? ORDER BY id`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*RunTask{}
	for rows.Next() {
		t := &RunTask{}
		if err := rows.Scan(&t.ID, &t.RunID, &t.Play, &t.Task, &t.Module, &t.Host, &t.Status, &t.Changed, &t.Detail); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// DeleteRun 删除执行记录及其任务明细。
func (s *Store) DeleteRun(id int64) error {
	// 主记录与明细同事务：只删主记录会永留孤儿明细
	return s.tx(func(q execer) error {
		res, err := q.Exec(`DELETE FROM runs WHERE id = ?`, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		_, err = q.Exec(`DELETE FROM run_tasks WHERE run_id = ?`, id)
		return err
	})
}

// ---- 执行状态对账与准入 ----

// ReconcileStaleRuns 启动对账：server 重启/崩溃后，内存里的执行全部
// 消失，但库里 queued/running 的 run 永远停在那（没有任何进程会再写
// 它们的终态）。启动时全部判为 failed——执行是「至多一次」语义，重启
// 后的真相是「不知道执行到哪」，标 failed + 原因，让人重新发起。
// run_tasks 里同批 run 的 running 明细一并收尾（防半途明细悬空）。
// 返回收尾的 run 数。
func (s *Store) ReconcileStaleRuns(reason string) (int64, error) {
	var n int64
	err := s.tx(func(q execer) error {
		now := nowUTC()
		// detail 已有内容时以全角分号追加（与半角内容区分），空串/NULL 直接写入
		if _, err := q.Exec(`UPDATE run_tasks SET status = 'failed', detail = COALESCE(NULLIF(detail, '') || '；', '') || ? WHERE run_id IN (SELECT id FROM runs WHERE status IN ('queued','running')) AND status = 'running'`, reason); err != nil {
			return err
		}
		res, err := q.Exec(`UPDATE runs SET status = 'failed', summary = ?, finished_at = ? WHERE status IN ('queued','running')`, reason, now)
		if err != nil {
			return err
		}
		n, _ = res.RowsAffected()
		return nil
	})
	return n, err
}

// ActiveRun 是一条未终结的执行记录（准入冲突反馈用）。
type ActiveRun struct {
	ID      int64
	AppID   int64
	AppName string
	Phase   string
	Status  string
	User    string
}

// ActiveRunsByApp 查一批应用当前 queued/running 的执行（应用级执行
// 准入：同应用并发跑会交错写 marker/状态，必须在入口拒绝并反馈）。
func (s *Store) ActiveRunsByApp(appIDs []int64) ([]ActiveRun, error) {
	if len(appIDs) == 0 {
		return nil, nil
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(appIDs)), ",")
	args := make([]any, 0, len(appIDs))
	for _, id := range appIDs {
		args = append(args, id)
	}
	rows, err := s.db.Query(`SELECT id, app_id, app_name, phase, status, user FROM runs WHERE status IN ('queued','running') AND app_id IN (`+ph+`) ORDER BY id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ActiveRun
	for rows.Next() {
		var r ActiveRun
		if err := rows.Scan(&r.ID, &r.AppID, &r.AppName, &r.Phase, &r.Status, &r.User); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RunConflictError 由 CreateRunsExclusive 返回：事务内发现目标应用已有
// queued/running 的执行（准入拒绝）。Active 携带冲突方明细供 409 反馈。
type RunConflictError struct {
	Active []ActiveRun
}

func (e *RunConflictError) Error() string {
	return fmt.Sprintf("active run exists (%d)", len(e.Active))
}

// CreateRunsExclusive 在单个事务内完成「应用级准入检查 + 逐条插入」。
// 先查后插分两步执行时，两个并发请求可同时通过检查并同时创建——
// 单连接只串行化单条语句，串行化不了两条语句之间的窗口；事务内
// 检查+插入才构成真正的互斥（SQLite 单写者，BEGIN 后写锁到手）。
// 返回的 ids 与 items 等长，逐条对应插入的 run ID。
func (s *Store) CreateRunsExclusive(items []RunInput) (ids []int64, err error) {
	if len(items) == 0 {
		return nil, nil
	}
	appIDs := make([]int64, 0, len(items))
	for _, it := range items {
		appIDs = append(appIDs, it.AppID)
	}
	var conflict []ActiveRun
	txErr := s.tx(func(q execer) error {
		active, err := activeRunsByApp(q, appIDs)
		if err != nil {
			return err
		}
		if len(active) > 0 {
			conflict = active
			return errConflict
		}
		ids = make([]int64, 0, len(items))
		for _, it := range items {
			res, err := q.Exec(`INSERT INTO runs (kind, app_id, app_name, version, phase, seq, status, selector, user, started_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				it.Kind, it.AppID, it.AppName, it.Version, it.Phase, it.Seq, it.Status, it.Selector, it.User, nowUTC())
			if err != nil {
				return err
			}
			id, err := res.LastInsertId()
			if err != nil {
				return err
			}
			ids = append(ids, id)
		}
		return nil
	})
	if errors.Is(txErr, errConflict) {
		return nil, &RunConflictError{Active: conflict}
	}
	// 出错时不返回半填充 ids：事务已回滚，残留 id 指向不存在的行
	if txErr != nil {
		return nil, txErr
	}
	return ids, nil
}

// errConflict 是事务内部 sentinel：回滚插入并以 RunConflictError 对外交付。
var errConflict = errors.New("active run exists")

// RunInput 是 CreateRunsExclusive 的入参（CreateRun 的结构化形态）。
type RunInput struct {
	Kind     string
	AppID    int64
	AppName  string
	Version  string
	Phase    string
	Seq      int
	Status   string
	Selector string
	User     string
}

// activeRunsByApp 是 ActiveRunsByApp 的事务内版本（复用准入查询口径）。
func activeRunsByApp(q execer, appIDs []int64) ([]ActiveRun, error) {
	ph := strings.TrimSuffix(strings.Repeat("?,", len(appIDs)), ",")
	args := make([]any, 0, len(appIDs))
	for _, id := range appIDs {
		args = append(args, id)
	}
	rows, err := q.Query(`SELECT id, app_id, app_name, phase, status, user FROM runs WHERE status IN ('queued','running') AND app_id IN (`+ph+`) ORDER BY id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ActiveRun
	for rows.Next() {
		var r ActiveRun
		if err := rows.Scan(&r.ID, &r.AppID, &r.AppName, &r.Phase, &r.Status, &r.User); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// HostTaskItem 是主机视角的一条执行任务（join runs 取状态与应用）。
type HostTaskItem struct {
	RunID   int64  `json:"RunID"`
	AppName string `json:"AppName"`
	Kind    string `json:"Kind"`
	Task    string `json:"Task"`
	Module  string `json:"Module"`
	Status  string `json:"Status"`
	Changed bool   `json:"Changed"`
	Detail  string `json:"Detail"`
	StartAt string `json:"StartAt"`
}

// HostTasksByName 主机名匹配的最近执行任务（按任务行插入序倒排）。
func (s *Store) HostTasksByName(host string, limit int) ([]*HostTaskItem, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := s.db.Query(`SELECT rt.run_id, COALESCE(r.app_name, ''), r.kind, rt.task, rt.module, rt.status, rt.changed, rt.detail, r.started_at
		FROM run_tasks rt JOIN runs r ON r.id = rt.run_id
		WHERE rt.host = ? ORDER BY rt.id DESC LIMIT ?`, host, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*HostTaskItem{}
	for rows.Next() {
		t := &HostTaskItem{}
		if err := rows.Scan(&t.RunID, &t.AppName, &t.Kind, &t.Task, &t.Module, &t.Status, &t.Changed, &t.Detail, &t.StartAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
