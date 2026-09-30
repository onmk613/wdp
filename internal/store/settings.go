package store

// 控制台运行时设置的存取（表结构见 store.go 最新迁移）：
// 全库仅一份（CHECK(id=1)），data 是 web 层定义的设置文档 JSON，
// version 做乐观锁——并发保存时后写方拿 409，不静默覆盖。

import (
	"database/sql"
	"errors"
)

// ErrSettingsVersion 乐观锁冲突：请求携带的期望版本已落后。
var ErrSettingsVersion = errors.New("settings version conflict")

// SettingsRow 设置行（data 原文透传，字段定义与校验在 web 层）。
type SettingsRow struct {
	Data      string
	Version   int
	UpdatedAt string
	UpdatedBy string
}

// LoadSettings 读设置行；从未保存过返回 ErrNotFound（调用方按默认初始化）。
func (s *Store) LoadSettings() (SettingsRow, error) {
	var r SettingsRow
	err := s.queryRow(`SELECT data, version, updated_at, updated_by FROM settings WHERE id = 1`).
		Scan(&r.Data, &r.Version, &r.UpdatedAt, &r.UpdatedBy)
	if errors.Is(err, sql.ErrNoRows) {
		return r, ErrNotFound
	}
	return r, err
}

// SaveSettings 乐观锁写入（expectVersion < 0 = 首次创建，行已存在且版本
// 不匹配同样 409）。整个读-验-写在单事务内完成——SQLite 单写者下天然
// 串行，事务边界仍保留给语义完整性（将来换库不改口径）。
func (s *Store) SaveSettings(data string, expectVersion int, updatedBy string) (int, error) {
	var newVer int
	err := s.tx(func(q execer) error {
		var cur int
		err := q.QueryRow(`SELECT version FROM settings WHERE id = 1`).Scan(&cur)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			if expectVersion >= 0 {
				return ErrSettingsVersion
			}
			newVer = 1
			_, err = q.Exec(`INSERT INTO settings (id, data, version, updated_at, updated_by) VALUES (1, ?, 1, ?, ?)`,
				data, nowUTC(), updatedBy)
			return err
		case err != nil:
			return err
		case expectVersion != cur:
			return ErrSettingsVersion
		}
		newVer = cur + 1
		_, err = q.Exec(`UPDATE settings SET data = ?, version = ?, updated_at = ?, updated_by = ? WHERE id = 1`,
			data, newVer, nowUTC(), updatedBy)
		return err
	})
	if err != nil {
		return 0, err
	}
	return newVer, nil
}

// OverwriteSettings 无版本写入（迁移等系统内部路径用：写的是同一份事实
// 的派生物，不存在并发编辑语义）。
func (s *Store) OverwriteSettings(data string, updatedBy string) error {
	_, err := s.SaveSettings(data, -1, updatedBy)
	if errors.Is(err, ErrSettingsVersion) {
		// 行已存在：直接覆盖并保留版本递增语义
		var cur int
		if err := s.queryRow(`SELECT version FROM settings WHERE id = 1`).Scan(&cur); err != nil {
			return err
		}
		_, err := s.exec(`UPDATE settings SET data = ?, version = ?, updated_at = ?, updated_by = ? WHERE id = 1`,
			data, cur+1, nowUTC(), updatedBy)
		return err
	}
	return err
}
