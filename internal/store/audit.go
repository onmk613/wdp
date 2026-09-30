package store

import (
	"strings"

	_ "modernc.org/sqlite"
)

// ---- 操作审计 ----

// AuditLog 是一条控制台操作记录（谁在何时对什么做了什么）。
type AuditLog struct {
	ID        int64  `json:"ID"`
	User      string `json:"User"`
	Action    string `json:"Action"` // create / update / delete / import / install / login / logout / set_latest / upload / run
	Object    string `json:"Object"` // host / pool / group / label / app / version / run / chart
	Name      string `json:"Name"`
	Detail    string `json:"Detail"`
	IP        string `json:"IP"`
	CreatedAt string `json:"CreatedAt"`
}

// CreateAuditLog 写一条操作审计（失败不阻断业务，由调用方决定是否记日志）。
func (s *Store) CreateAuditLog(a *AuditLog) error {
	_, err := s.exec(`INSERT INTO audit_logs (user, action, object, name, detail, ip, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		a.User, a.Action, a.Object, a.Name, a.Detail, a.IP, nowUTC())
	return err
}

// PruneAuditLogs 保留策略：删除 created_at 早于 cutoff 的审计行，返回
// 删除数。审计按操作频率线性增长（登录失败也逐条落库），无清理通道则
// 库无界膨胀。
func (s *Store) PruneAuditLogs(cutoff string) (int64, error) {
	res, err := s.exec(`DELETE FROM audit_logs WHERE created_at < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// ListAuditLogs 最近审计（q 对 user/action/object/name/detail 模糊过滤；
// limit 上限 500）。
// ListAuditLogsPage 分页审计（q 过滤全量后分页；新→旧）。
func (s *Store) ListAuditLogsPage(q string, page, size int) ([]*AuditLog, int64, error) {
	sqlWhere, args := "", []any{}
	if q = strings.TrimSpace(q); q != "" {
		sqlWhere = ` WHERE user LIKE ? ESCAPE '\' OR action LIKE ? ESCAPE '\' OR object LIKE ? ESCAPE '\' OR name LIKE ? ESCAPE '\' OR detail LIKE ? ESCAPE '\'`
		like := "%" + escapeLike(q) + "%"
		args = append(args, like, like, like, like, like)
	}
	var total int64
	if err := s.queryRow(`SELECT COUNT(*) FROM audit_logs`+sqlWhere, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.query(`SELECT id, user, action, object, name, detail, ip, created_at FROM audit_logs`+sqlWhere+` ORDER BY id DESC LIMIT ? OFFSET ?`,
		append(args, size, (page-1)*size)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []*AuditLog{}
	for rows.Next() {
		var l AuditLog
		if err := rows.Scan(&l.ID, &l.User, &l.Action, &l.Object, &l.Name, &l.Detail, &l.IP, &l.CreatedAt); err != nil {
			return nil, 0, err
		}
		out = append(out, &l)
	}
	return out, total, rows.Err()
}

func (s *Store) ListAuditLogs(limit int, q string) ([]*AuditLog, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	sqlStr := `SELECT id, user, action, object, name, detail, ip, created_at FROM audit_logs`
	args := []any{}
	if q = strings.TrimSpace(q); q != "" {
		sqlStr += ` WHERE user LIKE ? ESCAPE '\' OR action LIKE ? ESCAPE '\' OR object LIKE ? ESCAPE '\' OR name LIKE ? ESCAPE '\' OR detail LIKE ? ESCAPE '\'`
		like := "%" + escapeLike(q) + "%"
		args = append(args, like, like, like, like, like)
	}
	sqlStr += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.query(sqlStr, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*AuditLog{}
	for rows.Next() {
		a := &AuditLog{}
		if err := rows.Scan(&a.ID, &a.User, &a.Action, &a.Object, &a.Name, &a.Detail, &a.IP, &a.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
