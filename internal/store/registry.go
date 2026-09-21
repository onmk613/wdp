package store

import (
	"database/sql"
	"encoding/json"
	"errors"

	_ "modernc.org/sqlite"
)

// ---- 池 / 组 / 标签注册表 ----

// Pool 是主机池（一台主机至多属一个池，按名引用）。
type Pool struct {
	ID        int64
	Name      string
	Note      string
	Members   int // 池内主机数（列表查询附带）
	CreatedAt string
}

// GroupEntry 是组注册表项（hosts.group_name 按名引用）。
type GroupEntry struct {
	ID        int64
	Name      string
	Note      string
	CreatedAt string
}

// LabelDef 是标签键注册表项（hosts.labels JSON 按键引用）。
type LabelDef struct {
	ID        int64
	Key       string
	Note      string
	CreatedAt string
}

// CreatePool 新建池并可同时把已有主机划入（hostIDs 为台账 id）。
func (s *Store) CreatePool(name, note string, hostIDs []int64) (int64, error) {
	if err := validScopeName("pool name", name); err != nil {
		return 0, err
	}
	var id int64
	// 注册行与成员划入同事务：中途失败不留"建了池却少划了主机"的半状态
	err := s.tx(func(q execer) error {
		res, err := q.Exec(`INSERT INTO pools (name, note, created_at) VALUES (?, ?, ?)`, name, note, nowUTC())
		if err != nil {
			return dupErr(err, "pool", name)
		}
		if id, err = res.LastInsertId(); err != nil {
			return err
		}
		for _, hid := range hostIDs {
			if _, err := q.Exec(`INSERT OR IGNORE INTO host_pools (host_id, pool) VALUES (?, ?)`, hid, name); err != nil {
				return err
			}
			if _, err := q.Exec(`UPDATE hosts SET updated_at = ? WHERE id = ?`, nowUTC(), hid); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return id, nil
}

// ListPools 全部池（附成员数）。
func (s *Store) ListPools() ([]*Pool, error) {
	rows, err := s.db.Query(`SELECT id, name, note, created_at,
		(SELECT COUNT(*) FROM host_pools WHERE pool = pools.name) FROM pools ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Pool{} // 非 nil：空列表编码为 [] 而非 null（前端下拉直接可用）
	for rows.Next() {
		p := &Pool{}
		if err := rows.Scan(&p.ID, &p.Name, &p.Note, &p.CreatedAt, &p.Members); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// DeletePool 删除池并解除成员归属（主机保留，pool 置空）。
func (s *Store) DeletePool(id int64) error {
	var name string
	if err := s.db.QueryRow(`SELECT name FROM pools WHERE id = ?`, id).Scan(&name); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	// 注册行与成员归属同事务：只删一半会出现成员仍归属已删池的孤儿行
	return s.tx(func(q execer) error {
		if _, err := q.Exec(`DELETE FROM host_pools WHERE pool = ?`, name); err != nil {
			return err
		}
		_, err := q.Exec(`DELETE FROM pools WHERE id = ?`, id)
		return err
	})
}

// CreateGroup 新建组并可同时把已有主机划入。
func (s *Store) CreateGroup(name, note string, hostIDs []int64) (int64, error) {
	if err := validScopeName("group name", name); err != nil {
		return 0, err
	}
	var id int64
	// 与 CreatePool 同理：注册与成员划入同事务
	err := s.tx(func(q execer) error {
		res, err := q.Exec(`INSERT INTO host_groups (name, note, created_at) VALUES (?, ?, ?)`, name, note, nowUTC())
		if err != nil {
			return dupErr(err, "group", name)
		}
		if id, err = res.LastInsertId(); err != nil {
			return err
		}
		for _, hid := range hostIDs {
			if _, err := q.Exec(`INSERT OR IGNORE INTO host_group_map (host_id, group_name) VALUES (?, ?)`, hid, name); err != nil {
				return err
			}
			if _, err := q.Exec(`UPDATE hosts SET updated_at = ? WHERE id = ?`, nowUTC(), hid); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return id, nil
}

// ListGroups 全部组。
func (s *Store) ListGroups() ([]*GroupEntry, error) {
	rows, err := s.db.Query(`SELECT id, name, note, created_at FROM host_groups ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*GroupEntry{}
	for rows.Next() {
		g := &GroupEntry{}
		if err := rows.Scan(&g.ID, &g.Name, &g.Note, &g.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// DeleteGroup 删除组并解除成员归属。
func (s *Store) DeleteGroup(id int64) error {
	var name string
	if err := s.db.QueryRow(`SELECT name FROM host_groups WHERE id = ?`, id).Scan(&name); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	// 与 DeletePool 同理：注册行与成员归属同事务
	return s.tx(func(q execer) error {
		if _, err := q.Exec(`DELETE FROM host_group_map WHERE group_name = ?`, name); err != nil {
			return err
		}
		_, err := q.Exec(`DELETE FROM host_groups WHERE id = ?`, id)
		return err
	})
}

// CreateLabel 新建标签键并可附加到已有主机（value 追加进 hosts.labels）。
func (s *Store) CreateLabel(key, note string, hostIDs []int64, value string) (int64, error) {
	if err := validScopeName("label key", key); err != nil {
		return 0, err
	}
	// 各主机的 labels 须在事务外预读（单连接下事务内嵌套查询会死锁）
	type target struct {
		id     int64
		labels string
	}
	targets := make([]target, 0, len(hostIDs))
	for _, hid := range hostIDs {
		h, err := s.GetHost(hid)
		if err != nil {
			return 0, err
		}
		m := map[string]string{}
		// 写入时 validLabels 已保证是 JSON 对象，解码失败按空对象处理
		_ = json.Unmarshal([]byte(h.Labels), &m)
		m[key] = value
		b, err := json.Marshal(m)
		if err != nil {
			return 0, err
		}
		targets = append(targets, target{hid, string(b)})
	}
	var id int64
	// 注册行与各主机 labels 更新同事务：不留"标签键建了但主机没打上"的半状态
	err := s.tx(func(q execer) error {
		res, err := q.Exec(`INSERT INTO label_defs (key, note, created_at) VALUES (?, ?, ?)`, key, note, nowUTC())
		if err != nil {
			return dupErr(err, "label", key)
		}
		if id, err = res.LastInsertId(); err != nil {
			return err
		}
		for _, t := range targets {
			if _, err := q.Exec(`UPDATE hosts SET labels = ?, updated_at = ? WHERE id = ?`, t.labels, nowUTC(), t.id); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return id, nil
}

// ListLabels 全部标签键。
func (s *Store) ListLabels() ([]*LabelDef, error) {
	rows, err := s.db.Query(`SELECT id, key, note, created_at FROM label_defs ORDER BY key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*LabelDef{}
	for rows.Next() {
		l := &LabelDef{}
		if err := rows.Scan(&l.ID, &l.Key, &l.Note, &l.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// DeleteLabel 删除标签键并从全部主机的 labels 中移除该键。
func (s *Store) DeleteLabel(id int64) error {
	var key string
	if err := s.db.QueryRow(`SELECT key FROM label_defs WHERE id = ?`, id).Scan(&key); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	// 各主机的新 labels 在事务外预读预计算（单连接下事务内嵌套查询会死锁）
	hosts, err := s.ListHosts("")
	if err != nil {
		return err
	}
	type updated struct {
		id     int64
		labels string
	}
	var updates []updated
	for _, h := range hosts {
		var m map[string]string
		if err := json.Unmarshal([]byte(h.Labels), &m); err != nil || m == nil {
			continue
		}
		if _, ok := m[key]; !ok {
			continue
		}
		delete(m, key)
		b, err := json.Marshal(m)
		if err != nil {
			return err
		}
		updates = append(updates, updated{h.ID, string(b)})
	}
	// 各主机 labels 更新与注册行删除同事务：半途失败会出现部分主机仍带
	// 已删键的不一致，故错误必须传播
	return s.tx(func(q execer) error {
		for _, u := range updates {
			if _, err := q.Exec(`UPDATE hosts SET labels = ?, updated_at = ? WHERE id = ?`, u.labels, nowUTC(), u.id); err != nil {
				return err
			}
		}
		_, err := q.Exec(`DELETE FROM label_defs WHERE id = ?`, id)
		return err
	})
}
