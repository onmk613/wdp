package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	_ "modernc.org/sqlite"
)

// User 是控制台账号（MVP 单管理员；密码 bcrypt 散列）。
type User struct {
	ID           int64
	Name         string
	PasswordHash string `json:"-"`
	Role         string // admin / operator / viewer
	Disabled     bool
	CreatedAt    string
}

// UserScope 是一条按作用域追加的授权（叠加在角色之上）。
type UserScope struct {
	Verb  string `json:"verb"`
	Kind  string `json:"kind"` // '' = 全部 | pool | group | label
	Value string `json:"value"`
}

// EnrollToken 是一条主机纳管凭证：随机 token + TTL + 单次成功使用。
// claim 阶段记录执行脚本的 hostname 与来源地址（证书 SAN 用），done 阶段

func (s *Store) CountUsers() (int, error) {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// CreateUser 新增账号（密码须为 bcrypt 散列；role 缺省 viewer——共享
// 环境新用户最小权限起步，由管理员提权）。
func (s *Store) CreateUser(name, passwordHash, role string) error {
	if role == "" {
		role = "viewer"
	}
	_, err := s.db.Exec(`INSERT INTO users (name, password_hash, role, created_at) VALUES (?, ?, ?, ?)`, name, passwordHash, role, nowUTC())
	return err
}

// SetUserPassword upsert 账号密码：不存在则创建，存在则重置散列
// （显式配置的管理员密码：改值后重启即改密）。
func (s *Store) SetUserPassword(name, passwordHash string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("user name is required")
	}
	_, err := s.db.Exec(`INSERT INTO users (name, password_hash, created_at) VALUES (?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET password_hash = excluded.password_hash`,
		name, passwordHash, nowUTC())
	return err
}

// UserByName 按名查询账号。
func (s *Store) UserByName(name string) (*User, error) {
	u := &User{}
	err := s.db.QueryRow(`SELECT id, name, password_hash, role, disabled, created_at FROM users WHERE name = ?`, name).
		Scan(&u.ID, &u.Name, &u.PasswordHash, &u.Role, &u.Disabled, &u.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return u, nil
}

// UserByID 按 id 查询。
func (s *Store) UserByID(id int64) (*User, error) {
	u := &User{}
	err := s.db.QueryRow(`SELECT id, name, password_hash, role, disabled, created_at FROM users WHERE id = ?`, id).
		Scan(&u.ID, &u.Name, &u.PasswordHash, &u.Role, &u.Disabled, &u.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return u, nil
}

// ListUsers 全部账号（名字典序）。
func (s *Store) ListUsers() ([]*User, error) {
	rows, err := s.db.Query(`SELECT id, name, password_hash, role, disabled, created_at FROM users ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*User{}
	for rows.Next() {
		u := &User{}
		if err := rows.Scan(&u.ID, &u.Name, &u.PasswordHash, &u.Role, &u.Disabled, &u.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// UpdateUser 更新角色/禁用态（空字段保持不变）。
func (s *Store) UpdateUser(id int64, role string, disabled *bool) error {
	u, err := s.UserByID(id)
	if err != nil {
		return err
	}
	if role != "" {
		u.Role = role
	}
	if disabled != nil {
		u.Disabled = *disabled
	}
	res, err := s.db.Exec(`UPDATE users SET role = ?, disabled = ? WHERE id = ?`, u.Role, u.Disabled, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteUser 删除账号及其授权（保留最后一个 admin 由调用方把关）。
func (s *Store) DeleteUser(id int64) error {
	// 账号与授权同事务：账号删掉而授权残留即为指向不存在账号的孤儿行
	return s.tx(func(q execer) error {
		res, err := q.Exec(`DELETE FROM users WHERE id = ?`, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		_, err = q.Exec(`DELETE FROM user_scopes WHERE user_id = ?`, id)
		return err
	})
}

// CountAdmins 未禁用的 admin 数（最后一个 admin 不可删/禁/降级）。
func (s *Store) CountAdmins() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM users WHERE role = 'admin' AND disabled = 0`).Scan(&n)
	return n, err
}

// SetUserRole 设置角色（bootstrapAdmin 首启把管理员账号标为 admin）。
func (s *Store) SetUserRole(name, role string) error {
	_, err := s.db.Exec(`UPDATE users SET role = ? WHERE name = ?`, role, name)
	return err
}

// UserScopes 某用户的追加授权清单。
func (s *Store) UserScopes(userID int64) ([]*UserScope, error) {
	rows, err := s.db.Query(`SELECT verb, kind, value FROM user_scopes WHERE user_id = ? ORDER BY verb, kind, value`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*UserScope{}
	for rows.Next() {
		sc := &UserScope{}
		if err := rows.Scan(&sc.Verb, &sc.Kind, &sc.Value); err != nil {
			return nil, err
		}
		out = append(out, sc)
	}
	return out, rows.Err()
}

// AllUserScopes 一次取回全部用户的追加授权（按用户 ID 分组，组内排序与
// UserScopes 同口径）。用户列表页逐用户查询是 N+1——用户多了列表接口
// 线性变慢，单连接 SQLite 下尤甚。
func (s *Store) AllUserScopes() (map[int64][]*UserScope, error) {
	rows, err := s.db.Query(`SELECT user_id, verb, kind, value FROM user_scopes ORDER BY user_id, verb, kind, value`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64][]*UserScope{}
	for rows.Next() {
		var uid int64
		sc := &UserScope{}
		if err := rows.Scan(&uid, &sc.Verb, &sc.Kind, &sc.Value); err != nil {
			return nil, err
		}
		out[uid] = append(out[uid], sc)
	}
	return out, rows.Err()
}

// ReplaceUserScopes 整体替换某用户的追加授权（单事务：INSERT 失败时
// 已执行的 DELETE 随回滚还原，授权不丢）。
func (s *Store) ReplaceUserScopes(userID int64, scopes []*UserScope) error {
	return s.tx(func(q execer) error {
		if _, err := q.Exec(`DELETE FROM user_scopes WHERE user_id = ?`, userID); err != nil {
			return err
		}
		for _, sc := range scopes {
			if _, err := q.Exec(`INSERT OR IGNORE INTO user_scopes (user_id, verb, kind, value) VALUES (?, ?, ?, ?)`,
				userID, sc.Verb, sc.Kind, sc.Value); err != nil {
				return err
			}
		}
		return nil
	})
}
