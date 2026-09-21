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
	_, err := s.db.Exec(`INSERT INTO audit_logs (user, action, object, name, detail, ip, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		a.User, a.Action, a.Object, a.Name, a.Detail, a.IP, nowUTC())
	return err
}

// ListAuditLogs 最近审计（q 对 user/action/object/name/detail 模糊过滤；
// limit 上限 500）。
func (s *Store) ListAuditLogs(limit int, q string) ([]*AuditLog, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	sqlStr := `SELECT id, user, action, object, name, detail, ip, created_at FROM audit_logs`
	args := []any{}
	if q = strings.TrimSpace(q); q != "" {
		sqlStr += ` WHERE user LIKE ? OR action LIKE ? OR object LIKE ? OR name LIKE ? OR detail LIKE ?`
		like := "%" + q + "%"
		args = append(args, like, like, like, like, like)
	}
	sqlStr += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.Query(sqlStr, args...)
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
