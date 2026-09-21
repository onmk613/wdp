package store

import (
	"database/sql"
	"errors"

	_ "modernc.org/sqlite"
)

// ---- 编辑器草稿 ----

// AppDraft 是一份编辑器草稿（按用户 + 应用隔离；payload 为前端定义的
// 完整文件集 JSON，store 只当不透明文本存取）。
type AppDraft struct {
	UserID      int64
	AppKey      string
	BaseVersion string
	Payload     string
	UpdatedAt   string
}

// GetAppDraft 取草稿（不存在返回 ErrNotFound）。
func (s *Store) GetAppDraft(userID int64, appKey string) (*AppDraft, error) {
	row := s.db.QueryRow(`SELECT user_id, app_key, base_version, payload, updated_at
		FROM app_drafts WHERE user_id = ? AND app_key = ?`, userID, appKey)
	d := &AppDraft{}
	err := row.Scan(&d.UserID, &d.AppKey, &d.BaseVersion, &d.Payload, &d.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return d, nil
}

// PutAppDraft 写草稿（同键覆盖）。
func (s *Store) PutAppDraft(d *AppDraft) error {
	_, err := s.db.Exec(`INSERT INTO app_drafts (user_id, app_key, base_version, payload, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(user_id, app_key) DO UPDATE SET
		  base_version = excluded.base_version,
		  payload      = excluded.payload,
		  updated_at   = excluded.updated_at`,
		d.UserID, d.AppKey, d.BaseVersion, d.Payload, nowUTC())
	return err
}

// ListAppDrafts 列用户的全部草稿（不含 payload 正文，草稿箱列表用），
// 按暂存时间倒序。
func (s *Store) ListAppDrafts(userID int64) ([]*AppDraft, error) {
	rows, err := s.db.Query(`SELECT user_id, app_key, base_version, updated_at
		FROM app_drafts WHERE user_id = ? ORDER BY updated_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*AppDraft
	for rows.Next() {
		d := &AppDraft{}
		if err := rows.Scan(&d.UserID, &d.AppKey, &d.BaseVersion, &d.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// DeleteAppDraft 删草稿（不存在不报错——"清掉"是幂等语义）。
func (s *Store) DeleteAppDraft(userID int64, appKey string) error {
	_, err := s.db.Exec(`DELETE FROM app_drafts WHERE user_id = ? AND app_key = ?`, userID, appKey)
	return err
}
