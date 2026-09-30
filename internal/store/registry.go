package store

import (
	"database/sql"
	"encoding/json"
	"errors"

	_ "modernc.org/sqlite"
)

// ---- 池 / 组 / 标签注册表 ----

// Pool 是主机池（按名引用，一台主机可属多个池）。
type Pool struct {
	ID        int64
	Name      string
	Note      string
	Members   int // 池内主机数（列表查询附带）
	CreatedAt string
}

// GroupEntry 是组注册表项（成员关系在 host_group_map 映射表，按名引用）。
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
		var err error
		id, err = s.lastInsertID(q, `INSERT INTO pools (name, note, created_at) VALUES (?, ?, ?)`, name, note, nowUTC())
		if err != nil {
			return s.dupErr(err, "pool", name)
		}
		for _, hid := range hostIDs {
			// host_pools 无外键约束，INSERT OR IGNORE 对不存在的 hostID
			// 会落孤儿成员行（口径同 CreateLabel：先 SELECT 校验，事务内
			// 整体回滚）
			var n int
			if err := q.QueryRow(`SELECT COUNT(*) FROM hosts WHERE id = ?`, hid).Scan(&n); err != nil {
				return err
			}
			if n == 0 {
				return ErrNotFound
			}
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
	rows, err := s.query(`SELECT id, name, note, created_at,
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

// DeletePool 删除池并解除成员映射（主机保留）。
// 注册行与成员归属同事务：只删一半会出现成员仍归属已删池的孤儿行；
// 池名读取也收进事务——预读在外时，窗口内同名池被删后重建，按陈旧
// 名删映射会误删新池成员（事务内查询一律走 q，见 tx 注释）。
func (s *Store) DeletePool(id int64) error {
	return s.tx(func(q execer) error {
		var name string
		if err := q.QueryRow(`SELECT name FROM pools WHERE id = ?`, id).Scan(&name); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if _, err := q.Exec(`DELETE FROM host_pools WHERE pool = ?`, name); err != nil {
			return err
		}
		res, err := q.Exec(`DELETE FROM pools WHERE id = ?`, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return nil
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
		var err error
		id, err = s.lastInsertID(q, `INSERT INTO host_groups (name, note, created_at) VALUES (?, ?, ?)`, name, note, nowUTC())
		if err != nil {
			return s.dupErr(err, "group", name)
		}
		for _, hid := range hostIDs {
			// host_group_map 同样无外键约束，校验口径同 CreatePool
			// （对齐 CreateLabel：缺失 hostID → ErrNotFound 整体回滚）
			var n int
			if err := q.QueryRow(`SELECT COUNT(*) FROM hosts WHERE id = ?`, hid).Scan(&n); err != nil {
				return err
			}
			if n == 0 {
				return ErrNotFound
			}
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
	rows, err := s.query(`SELECT id, name, note, created_at FROM host_groups ORDER BY name`)
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
// 与 DeletePool 同理：注册行与成员归属同事务，组名读取也收进事务
// （预读在外时窗口内同名组删后重建会误删新组成员）。
func (s *Store) DeleteGroup(id int64) error {
	return s.tx(func(q execer) error {
		var name string
		if err := q.QueryRow(`SELECT name FROM host_groups WHERE id = ?`, id).Scan(&name); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if _, err := q.Exec(`DELETE FROM host_group_map WHERE group_name = ?`, name); err != nil {
			return err
		}
		res, err := q.Exec(`DELETE FROM host_groups WHERE id = ?`, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// CreateLabel 新建标签键并可附加到已有主机（value 追加进 hosts.labels）。
// 主机 labels 的读取与全部写入同事务：预读在外时，窗口内并发的
// BatchAssign/UpdateHost 改 labels 会被本事务按陈旧值整体覆盖（丢更新）。
// 事务内查询一律走 q（见 tx 注释）。
func (s *Store) CreateLabel(key, note string, hostIDs []int64, value string) (int64, error) {
	if err := validScopeName("label key", key); err != nil {
		return 0, err
	}
	var id int64
	err := s.tx(func(q execer) error {
		var err error
		id, err = s.lastInsertID(q, `INSERT INTO label_defs (key, note, created_at) VALUES (?, ?, ?)`, key, note, nowUTC())
		if err != nil {
			return s.dupErr(err, "label", key)
		}
		for _, hid := range hostIDs {
			var labels string
			if err := q.QueryRow(`SELECT labels FROM hosts WHERE id = ?`, hid).Scan(&labels); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return ErrNotFound
				}
				return err
			}
			m := map[string]string{}
			// 写入时 validLabels 已保证是 JSON 对象，解码失败按空对象处理
			_ = json.Unmarshal([]byte(labels), &m)
			m[key] = value
			b, err := json.Marshal(m)
			if err != nil {
				return err
			}
			if _, err := q.Exec(`UPDATE hosts SET labels = ?, updated_at = ? WHERE id = ?`, string(b), nowUTC(), hid); err != nil {
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
	rows, err := s.query(`SELECT id, key, note, created_at FROM label_defs ORDER BY key`)
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

// labelUpdate 是一次主机 labels 的整值替换（删除标签键路径的中间形态）。
type labelUpdate struct {
	id     int64
	labels string
}

// DeleteLabel 删除标签键并从全部主机的 labels 中移除该键。
// 键名读取与各主机 labels 的读取/写入同事务：预读在外时窗口内并发的
// labels 修改会被陈旧值覆盖（丢更新）。事务内查询一律走 q（见 tx 注释）；
// 各主机 labels 更新与注册行删除同事务：半途失败会出现部分主机仍带
// 已删键的不一致，故错误必须传播。
func (s *Store) DeleteLabel(id int64) error {
	return s.tx(func(q execer) error {
		var key string
		if err := q.QueryRow(`SELECT key FROM label_defs WHERE id = ?`, id).Scan(&key); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		rows, err := q.Query(`SELECT id, labels FROM hosts`)
		if err != nil {
			return err
		}
		updates, err := collectLabelDrops(rows, key)
		if cerr := rows.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
		for _, u := range updates {
			if _, err := q.Exec(`UPDATE hosts SET labels = ?, updated_at = ? WHERE id = ?`, u.labels, nowUTC(), u.id); err != nil {
				return err
			}
		}
		_, err = q.Exec(`DELETE FROM label_defs WHERE id = ?`, id)
		return err
	})
}

// collectLabelDrops 从主机清单行挑出携带 key 的主机并给出移除后的 labels
// JSON。rows 迭代完毕由调用方显式 Close 后再 Exec——同连接上未关的 rows
// 与写语句不混用（CreateRunsExclusive 的同款口径）。
func collectLabelDrops(rows *sql.Rows, key string) ([]labelUpdate, error) {
	var updates []labelUpdate
	for rows.Next() {
		var hid int64
		var labels string
		if err := rows.Scan(&hid, &labels); err != nil {
			return nil, err
		}
		var m map[string]string
		if err := json.Unmarshal([]byte(labels), &m); err != nil || m == nil {
			continue
		}
		if _, ok := m[key]; !ok {
			continue
		}
		delete(m, key)
		b, err := json.Marshal(m)
		if err != nil {
			return nil, err
		}
		updates = append(updates, labelUpdate{hid, string(b)})
	}
	return updates, rows.Err()
}
