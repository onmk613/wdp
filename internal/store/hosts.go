package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	_ "modernc.org/sqlite"
)

// Host 是纳管主机台账行。labels 为 JSON 对象字符串（前端展开）；
// status 由 server 探活循环维护（unknown/online/offline）。
type Host struct {
	ID         int64
	Name       string
	Address    string
	AgentPort  int
	Pools      []string // 多值：一台主机可属多个池（用户/权限分配的基础）
	Groups     []string // 多值：同上
	Labels     string   // JSON 对象（天然多值）
	Status     string
	LastSeenAt string
	CreatedAt  string
	UpdatedAt  string
	// AllowPlaintext 声明该主机的 agent 未启用 mTLS（明文 HTTP，仅限可信
	// 内网）。默认 false：CA 启用时一律按 mTLS 建连，避免"探测失败即降级
	// 明文"这种可被中间人触发的通道降级。
	AllowPlaintext bool `json:"allow_plaintext,omitempty"`
}

// CreateHost 新增主机（name 唯一；重复返回错误）。
func (s *Store) CreateHost(h *Host) (int64, error) {
	if strings.TrimSpace(h.Name) == "" || strings.TrimSpace(h.Address) == "" {
		return 0, Bizf("name and address are required")
	}
	if h.AgentPort <= 0 {
		h.AgentPort = 7602
	}
	if h.Labels == "" {
		h.Labels = "{}"
	}
	if err := validLabels(h.Labels); err != nil {
		return 0, err
	}
	h.CreatedAt, h.UpdatedAt, h.Status = nowUTC(), nowUTC(), "unknown"
	var id int64
	// 台账行与池/组归属同事务：中途失败不留无归属（或归属半截）的行
	err := s.tx(func(q execer) error {
		res, err := q.Exec(`INSERT INTO hosts (name, address, agent_port, labels, status, created_at, updated_at, allow_plaintext)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			h.Name, h.Address, h.AgentPort, h.Labels, h.Status, h.CreatedAt, h.UpdatedAt, boolInt(h.AllowPlaintext))
		if err != nil {
			return err
		}
		if id, err = res.LastInsertId(); err != nil {
			return err
		}
		if err := setHostPools(q, id, h.Pools); err != nil {
			return err
		}
		return setHostGroups(q, id, h.Groups)
	})
	if err != nil {
		return 0, err
	}
	return id, nil
}

// setHostPools 整体替换主机的池归属（多值）。
func setHostPools(q execer, id int64, pools []string) error {
	list := dedup(pools)
	if err := validScopeNames("pool name", list); err != nil {
		return err
	}
	if _, err := q.Exec(`DELETE FROM host_pools WHERE host_id = ?`, id); err != nil {
		return err
	}
	for _, p := range list {
		if _, err := q.Exec(`INSERT OR IGNORE INTO host_pools (host_id, pool) VALUES (?, ?)`, id, p); err != nil {
			return err
		}
	}
	return nil
}

// setHostGroups 整体替换主机的组归属（多值）。
func setHostGroups(q execer, id int64, groups []string) error {
	list := dedup(groups)
	if err := validScopeNames("group name", list); err != nil {
		return err
	}
	if _, err := q.Exec(`DELETE FROM host_group_map WHERE host_id = ?`, id); err != nil {
		return err
	}
	for _, g := range list {
		if _, err := q.Exec(`INSERT OR IGNORE INTO host_group_map (host_id, group_name) VALUES (?, ?)`, id, g); err != nil {
			return err
		}
	}
	return nil
}

// UpdateHost 更新主机连接信息（status/last_seen 由探活维护，不在此覆盖）。
func (s *Store) UpdateHost(id int64, h *Host) error {
	if strings.TrimSpace(h.Address) == "" {
		return Bizf("address is required")
	}
	if h.AgentPort <= 0 {
		h.AgentPort = 7602
	}
	if h.Labels == "" {
		h.Labels = "{}"
	}
	if err := validLabels(h.Labels); err != nil {
		return err
	}
	// 更新与池/组整体替换同事务：替换中途失败不得留下半替换状态
	return s.tx(func(q execer) error {
		res, err := q.Exec(`UPDATE hosts SET address = ?, agent_port = ?, labels = ?, allow_plaintext = ?, updated_at = ? WHERE id = ?`,
			h.Address, h.AgentPort, h.Labels, boolInt(h.AllowPlaintext), nowUTC(), id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		if err := setHostPools(q, id, h.Pools); err != nil {
			return err
		}
		return setHostGroups(q, id, h.Groups)
	})
}

// DeleteHost 删除主机（连同池/组归属映射同事务清理）。
func (s *Store) DeleteHost(id int64) error {
	return s.tx(func(q execer) error {
		res, err := q.Exec(`DELETE FROM hosts WHERE id = ?`, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		// 残留映射行会把已删主机继续计入池成员数且永久累积，必须一并清掉
		if _, err := q.Exec(`DELETE FROM host_pools WHERE host_id = ?`, id); err != nil {
			return err
		}
		_, err = q.Exec(`DELETE FROM host_group_map WHERE host_id = ?`, id)
		return err
	})
}

// GetHost 按 id 查询。
func (s *Store) GetHost(id int64) (*Host, error) {
	row := s.db.QueryRow(`SELECT id, name, address, agent_port, labels, status, last_seen_at, created_at, updated_at, allow_plaintext,
		(SELECT COALESCE(group_concat(pool, ','), '') FROM host_pools WHERE host_id = hosts.id),
		(SELECT COALESCE(group_concat(group_name, ','), '') FROM host_group_map WHERE host_id = hosts.id)
		FROM hosts WHERE id = ?`, id)
	return scanHostMulti(row)
}

// ListHosts 台账（按 name 排序）。q 非空时按 主机名/地址/池/组/标签
// 模糊匹配过滤（标签为 JSON 文本 LIKE，键值子串均可命中）。
func (s *Store) ListHosts(q string) ([]*Host, error) {
	if q = strings.TrimSpace(q); q == "" {
		return s.listHostsWhere("")
	}
	like := "%" + escapeLike(q) + "%"
	return s.listHostsWhere(`name LIKE ? ESCAPE '\' OR address LIKE ? ESCAPE '\' OR labels LIKE ? ESCAPE '\'
		OR EXISTS (SELECT 1 FROM host_pools hp WHERE hp.host_id = hosts.id AND hp.pool LIKE ? ESCAPE '\')
		OR EXISTS (SELECT 1 FROM host_group_map hg WHERE hg.host_id = hosts.id AND hg.group_name LIKE ? ESCAPE '\')`,
		like, like, like, like, like)
}

// escapeLike 转义 LIKE 通配符（%/_/\），配合 ESCAPE '\' 使用：不转义时
// 搜索串里的元字符会意外全匹配/单字符通配。
func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// listHostsWhere 台账查询的参数化内核：ListHosts 与 HostsBySelector 共用
// （后者把 pool/group/hosts 选择器下推到 SQL，避免"全表扫 + Go 侧逐行
// 过滤"——权限解析每个请求都会走这里，主机上千台后是热点）。
func (s *Store) listHostsWhere(where string, args ...any) ([]*Host, error) {
	sqlStr := `SELECT id, name, address, agent_port, labels, status, last_seen_at, created_at, updated_at, allow_plaintext,
		(SELECT COALESCE(group_concat(pool, ','), '') FROM host_pools WHERE host_id = hosts.id),
		(SELECT COALESCE(group_concat(group_name, ','), '') FROM host_group_map WHERE host_id = hosts.id)
		FROM hosts`
	if where != "" {
		sqlStr += " WHERE " + where
	}
	sqlStr += ` ORDER BY name`
	rows, err := s.db.Query(sqlStr, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Host{} // 非 nil：空台账编码为 []
	for rows.Next() {
		h, err := scanHostMulti(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// SetHostStatus 探活循环写回状态与最近在线时刻。
func (s *Store) SetHostStatus(id int64, status string) error {
	lastSeen := ""
	if status == "online" {
		lastSeen = nowUTC()
	}
	_, err := s.db.Exec(`UPDATE hosts SET status = ?, last_seen_at = CASE WHEN ? = 'online' THEN ? ELSE last_seen_at END, updated_at = ? WHERE id = ?`,
		status, status, lastSeen, nowUTC(), id)
	return err
}

// GetHostByName 按台账名查主机（不存在返回 ErrNotFound）。纳管 claim 的
// 主机名碰撞校验用。
func (s *Store) GetHostByName(name string) (*Host, error) {
	row := s.db.QueryRow(`SELECT id, name, address, agent_port, labels, status, last_seen_at, created_at, updated_at, allow_plaintext,
		(SELECT COALESCE(group_concat(pool, ','), '') FROM host_pools WHERE host_id = hosts.id),
		(SELECT COALESCE(group_concat(group_name, ','), '') FROM host_group_map WHERE host_id = hosts.id)
		FROM hosts WHERE name = ?`, name)
	return scanHostMulti(row)
}

// UpsertHostByName 纳管完成落账：同名更新地址/端口（保留分组与标签），
// 不存在则创建。返回台账 id。
func (s *Store) UpsertHostByName(name, address string, agentPort int) (int64, error) {
	if strings.TrimSpace(name) == "" || strings.TrimSpace(address) == "" {
		return 0, Bizf("name and address are required")
	}
	if agentPort <= 0 {
		agentPort = 7602
	}
	var id int64
	err := s.db.QueryRow(`SELECT id FROM hosts WHERE name = ?`, name).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		res, ierr := s.db.Exec(`INSERT INTO hosts (name, address, agent_port, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
			name, address, agentPort, nowUTC(), nowUTC())
		if ierr != nil {
			// SELECT 与 INSERT 之间被并发同名纳管抢先：重读既有行返回
			// （幂等语义），不把裸唯一约束错抛给调用方
			if !isUniqueErr(ierr) {
				return 0, ierr
			}
			if rerr := s.db.QueryRow(`SELECT id FROM hosts WHERE name = ?`, name).Scan(&id); rerr != nil {
				return 0, ierr
			}
			return id, nil
		}
		return res.LastInsertId()
	}
	if err != nil {
		return 0, err
	}
	res, err := s.db.Exec(`UPDATE hosts SET address = ?, agent_port = ?, updated_at = ? WHERE id = ?`, address, agentPort, nowUTC(), id)
	if err != nil {
		return 0, err
	}
	// SELECT 与 UPDATE 之间主机被删：RowsAffected 为 0，返回 ErrNotFound
	// 而非已删主机的 id
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, ErrNotFound
	}
	return id, nil
}

// HostIDs 返回全部主机 id（探活遍历）。
func (s *Store) HostIDs() ([]int64, error) {
	rows, err := s.db.Query(`SELECT id FROM hosts`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

type rowScanner interface{ Scan(dest ...any) error }

// boolInt 把布尔落成 SQLite 的 0/1。
func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// scanHostMulti 扫描 hosts 行（含尾部 group_concat 的池/组多值列）。
func scanHostMulti(row rowScanner) (*Host, error) {
	h := &Host{}
	var pools, groups string
	var plaintext int
	err := row.Scan(&h.ID, &h.Name, &h.Address, &h.AgentPort, &h.Labels, &h.Status, &h.LastSeenAt, &h.CreatedAt, &h.UpdatedAt, &plaintext, &pools, &groups)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	h.AllowPlaintext = plaintext != 0
	// 排序保证输出稳定：group_concat 无 ORDER BY，行序不定会让结果抖动
	if pools != "" {
		h.Pools = strings.Split(pools, ",")
		slices.Sort(h.Pools)
	}
	if groups != "" {
		h.Groups = strings.Split(groups, ",")
		slices.Sort(h.Groups)
	}
	return h, nil
}

// ---- 批量操作 ----

// BatchAssign 批量设置池/组/标签：pools/groups 为 nil 表示不改
// （非 nil 即整体替换该集合，空切片 = 清空）；labels 追加合并
// （replace=true 时整体替换）。单台失败（读取/写入出错）返回首个错误，
// 其余主机继续处理——静默吞错会让调用方看到"失败却无原因"（n=0 且
// err=nil 时尤其误导）。
func (s *Store) BatchAssign(ids []int64, pools, groups []string, setPools, setGroups bool, labels map[string]string, replace bool) (int, error) {
	n := 0
	var firstErr error
	for _, id := range ids {
		h, err := s.GetHost(id)
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("host %d: %w", id, err)
			}
			continue
		}
		if setPools {
			h.Pools = pools
		}
		if setGroups {
			h.Groups = groups
		}
		if replace {
			if labels == nil {
				labels = map[string]string{}
			}
			b, _ := json.Marshal(labels) // map[string]string 的 Marshal 不会失败
			h.Labels = string(b)
		} else if len(labels) > 0 {
			m := map[string]string{}
			// h.Labels 写入时 validLabels 已保证是 JSON 对象，解码失败按空对象合并
			_ = json.Unmarshal([]byte(h.Labels), &m)
			for k, v := range labels {
				m[k] = v
			}
			b, _ := json.Marshal(m) // 同上，不会失败
			h.Labels = string(b)
		}
		if err := s.UpdateHost(id, h); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("host %d: %w", id, err)
			}
			continue
		}
		n++
	}
	return n, firstErr
}

// HostsBySelector 按选择器解析主机：kind = all|pool|group|label|hosts。
// pool/group/hosts 下推 SQL；label 用"JSON 键字面量 LIKE 预筛 + Go 侧
// 精确判定"（JSON 路径函数对含点/特殊字符的键不可靠，预筛已把候选集
// 从全表收敛到近命中集）。
func (s *Store) HostsBySelector(kind, value string, hostIDs []int64) ([]*Host, error) {
	switch kind {
	case "pool":
		return s.listHostsWhere(`EXISTS (SELECT 1 FROM host_pools hp WHERE hp.host_id = hosts.id AND hp.pool = ?)`, value)
	case "group":
		return s.listHostsWhere(`EXISTS (SELECT 1 FROM host_group_map hg WHERE hg.host_id = hosts.id AND hg.group_name = ?)`, value)
	case "label":
		k, v, hasV := strings.Cut(value, "=")
		cands, err := s.listHostsWhere(`labels LIKE ? ESCAPE '\'`, "%"+escapeLike(`"`+k+`"`)+"%")
		if err != nil {
			return nil, err
		}
		var out []*Host
		for _, h := range cands {
			var m map[string]string
			// labels 写入时 validLabels 已保证是 JSON 对象，解码失败按无标签处理（不命中）
			_ = json.Unmarshal([]byte(h.Labels), &m)
			// value 形态：键存在（env）或键值精确匹配（env=prod）
			if hasV {
				if m[k] == v {
					out = append(out, h)
				}
			} else if _, ok := m[k]; ok {
				out = append(out, h)
			}
		}
		return out, nil
	case "hosts":
		if len(hostIDs) == 0 {
			return []*Host{}, nil
		}
		ph := strings.TrimSuffix(strings.Repeat("?,", len(hostIDs)), ",")
		args := make([]any, 0, len(hostIDs))
		for _, id := range hostIDs {
			args = append(args, id)
		}
		return s.listHostsWhere("id IN ("+ph+")", args...)
	default: // "", "all"
		return s.listHostsWhere("")
	}
}
