package store

import (
	"database/sql"
	"errors"
	"strings"

	_ "modernc.org/sqlite"
)

// User 是控制台账号（角色 admin/operator/viewer，可按作用域追加授权；
// 密码 bcrypt 散列）。
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

func (s *Store) CountUsers() (int, error) {
	var n int
	if err := s.queryRow(`SELECT COUNT(*) FROM users`).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// validUserRole 校验角色枚举（与 web 层 builtinRoles 同一集合）。store
// 不能反向依赖 web（web 导入 store），domain 层独立兜底：web 已把非法
// 角色映射/拒绝，这里防的是绕过 web 直写 store 的调用方——任意串一旦
// 入库，perm 语义会把空值当 operator、把未知值也当 operator 处理。
func validUserRole(role string) bool {
	switch role {
	case "admin", "operator", "viewer":
		return true
	}
	return false
}

// CreateUser 新增账号（密码须为 bcrypt 散列；role 必填且限
// admin/operator/viewer——缺省语义由调用方显式决定，不再隐式补 viewer：
// 空 role 入库会被 perm 当 operator，新账号静默拿到高于 viewer 的权限）。
func (s *Store) CreateUser(name, passwordHash, role string) error {
	if !validUserRole(role) {
		return Bizf("invalid role %q (admin | operator | viewer)", role)
	}
	_, err := s.exec(`INSERT INTO users (name, password_hash, role, created_at) VALUES (?, ?, ?, ?)`, name, passwordHash, role, nowUTC())
	return err
}

// SetUserPassword upsert 账号密码：不存在则创建，存在则重置散列
// （显式配置的管理员密码：改值后重启即改密）。
func (s *Store) SetUserPassword(name, passwordHash string) error {
	if strings.TrimSpace(name) == "" {
		return Bizf("user name is required")
	}
	_, err := s.exec(`INSERT INTO users (name, password_hash, created_at) VALUES (?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET password_hash = excluded.password_hash`,
		name, passwordHash, nowUTC())
	return err
}

// UserByName 按名查询账号。
func (s *Store) UserByName(name string) (*User, error) {
	u := &User{}
	err := s.queryRow(`SELECT id, name, password_hash, role, disabled, created_at FROM users WHERE name = ?`, name).
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
	err := s.queryRow(`SELECT id, name, password_hash, role, disabled, created_at FROM users WHERE id = ?`, id).
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
	rows, err := s.query(`SELECT id, name, password_hash, role, disabled, created_at FROM users ORDER BY name`)
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

// UpdateUser 更新角色/禁用态（空字段保持不变；role 非空时须为合法枚举）。
// 最后一个活跃 admin 的降级/禁用在此原子拒绝：预检若只放 web 层
// （CountAdmins 与 UPDATE 分离），并发双降级会同时通过预检、事后系统
// 失去任何可用管理者。读取与 COUNT 同事务后，web 的预检退化为提前给
// 出友好提示的快路径，语义与这里一致（Bizf → web 层自然 400）。
func (s *Store) UpdateUser(id int64, role string, disabled *bool) error {
	// 空 = 保持不变（web 的 PATCH 语义）；非空非法值拒绝——store 层
	// 兜底，理由见 validUserRole
	if role != "" && !validUserRole(role) {
		return Bizf("invalid role %q (admin | operator | viewer)", role)
	}
	return s.tx(func(q execer) error {
		var curRole string
		var curDisabled bool
		if err := q.QueryRow(`SELECT role, disabled FROM users WHERE id = ?`, id).Scan(&curRole, &curDisabled); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		newRole, newDisabled := curRole, curDisabled
		if role != "" {
			newRole = role
		}
		if disabled != nil {
			newDisabled = *disabled
		}
		// 目标当前是活跃 admin 且更新后将不再是（降级或禁用）：须还有
		// 其他活跃 admin，否则拒绝（COUNT 与 UPDATE 同事务，杜绝窗口）
		if curRole == "admin" && !curDisabled && (newRole != "admin" || newDisabled) {
			var others int
			if err := q.QueryRow(`SELECT COUNT(*) FROM users WHERE role = 'admin' AND disabled = 0 AND id != ?`, id).Scan(&others); err != nil {
				return err
			}
			if others == 0 {
				return Bizf("cannot demote/disable the last admin")
			}
		}
		_, err := q.Exec(`UPDATE users SET role = ?, disabled = ? WHERE id = ?`, newRole, newDisabled, id)
		return err
	})
}

// DeleteUser 删除账号及其授权。最后一个活跃 admin 同样在此原子拒绝
// （理由与 UpdateUser 相同：web 预检与 DELETE 分离存在并发双删窗口）。
func (s *Store) DeleteUser(id int64) error {
	// 账号与授权同事务：账号删掉而授权残留即为指向不存在账号的孤儿行
	return s.tx(func(q execer) error {
		var curRole string
		var curDisabled bool
		if err := q.QueryRow(`SELECT role, disabled FROM users WHERE id = ?`, id).Scan(&curRole, &curDisabled); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if curRole == "admin" && !curDisabled {
			var others int
			if err := q.QueryRow(`SELECT COUNT(*) FROM users WHERE role = 'admin' AND disabled = 0 AND id != ?`, id).Scan(&others); err != nil {
				return err
			}
			if others == 0 {
				return Bizf("cannot delete the last admin")
			}
		}
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
	err := s.queryRow(`SELECT COUNT(*) FROM users WHERE role = 'admin' AND disabled = 0`).Scan(&n)
	return n, err
}

// SetUserRole 设置角色（bootstrapAdmin 首启把管理员账号标为 admin）。
func (s *Store) SetUserRole(name, role string) error {
	_, err := s.exec(`UPDATE users SET role = ? WHERE name = ?`, role, name)
	return err
}

// UserScopes 某用户的追加授权清单。
func (s *Store) UserScopes(userID int64) ([]*UserScope, error) {
	rows, err := s.query(`SELECT verb, kind, value FROM user_scopes WHERE user_id = ? ORDER BY verb, kind, value`, userID)
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
	rows, err := s.query(`SELECT user_id, verb, kind, value FROM user_scopes ORDER BY user_id, verb, kind, value`)
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
