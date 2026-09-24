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

// ---- 应用与版本 ----

// App 是一个应用（chart 的版本集合）。
type App struct {
	ID            int64
	Name          string
	Note          string
	LatestVersion string   `json:"LatestVersion"`
	Pools         []string `json:"Pools"`
	Groups        []string `json:"Groups"`
	Labels        string   `json:"Labels"`
	VersionCount  int      `json:"VersionCount"`
	CreatedAt     string   `json:"CreatedAt"`
	UpdatedAt     string   `json:"UpdatedAt"`
}

// AppVersion 是应用的一个不可变版本（tgz 制品在磁盘，库只存元信息）。
// Phases 是该版本 chart 实际具备的相位（创建时提取，前端执行清单行级
// 相位下拉的数据源；迁移前旧行为 nil，调用方自行回退）。
type AppVersion struct {
	ID        int64
	AppID     int64
	Version   string
	TgzPath   string `json:"-"`
	Sha256    string
	Size      int64
	Note      string
	Phases    []string `json:"Phases"`
	CreatedAt string
}

func setAppScopes(q execer, id int64, pools, groups []string) error {
	pl, gl := dedup(pools), dedup(groups)
	// 应用侧池/组名同样进 group_concat 拼接列，含逗号/空白的名字在此拒绝
	if err := validScopeNames("pool name", pl); err != nil {
		return err
	}
	if err := validScopeNames("group name", gl); err != nil {
		return err
	}
	if _, err := q.Exec(`DELETE FROM app_pools WHERE app_id = ?`, id); err != nil {
		return err
	}
	for _, p := range pl {
		if _, err := q.Exec(`INSERT OR IGNORE INTO app_pools (app_id, pool) VALUES (?, ?)`, id, p); err != nil {
			return err
		}
	}
	if _, err := q.Exec(`DELETE FROM app_groups WHERE app_id = ?`, id); err != nil {
		return err
	}
	for _, g := range gl {
		if _, err := q.Exec(`INSERT OR IGNORE INTO app_groups (app_id, group_name) VALUES (?, ?)`, id, g); err != nil {
			return err
		}
	}
	return nil
}

// CreateApp 新建应用并写入首个版本（tgz 已由调用方落盘）。
func (s *Store) CreateApp(name, note, labels string, pools, groups []string, version, tgzPath, sha string, size int64, phases []string) (int64, error) {
	if strings.TrimSpace(name) == "" {
		return 0, fmt.Errorf("app name is required")
	}
	if version == "" {
		version = "v1"
	}
	if labels == "" {
		labels = "{}"
	}
	if err := validLabels(labels); err != nil {
		return 0, err
	}
	var id int64
	// 应用行、作用域与首个版本同事务：不留缺版本或缺作用域的半初始化应用
	err := s.tx(func(q execer) error {
		res, err := q.Exec(`INSERT INTO apps (name, note, latest_version, labels, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`,
			name, note, version, labels, nowUTC(), nowUTC())
		if err != nil {
			return dupErr(err, "app", name)
		}
		if id, err = res.LastInsertId(); err != nil {
			return err
		}
		if err := setAppScopes(q, id, pools, groups); err != nil {
			return err
		}
		pj, gj := scopeJSON(pools), scopeJSON(groups)
		_, err = q.Exec(`INSERT INTO app_versions (app_id, version, tgz_path, sha256, size, pools, groups, labels, phases, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			id, version, tgzPath, sha, size, pj, gj, labels, scopeJSON(phases), nowUTC())
		return err
	})
	if err != nil {
		return 0, err
	}
	return id, nil
}

// ListApps 全部应用（最新版本 + 作用域 + 版本数）。
func (s *Store) ListApps() ([]*App, error) {
	rows, err := s.db.Query(`SELECT id, name, note, latest_version, labels, created_at, updated_at,
		(SELECT COUNT(*) FROM app_versions WHERE app_id = apps.id),
		(SELECT COALESCE(group_concat(pool, ','), '') FROM app_pools WHERE app_id = apps.id),
		(SELECT COALESCE(group_concat(group_name, ','), '') FROM app_groups WHERE app_id = apps.id)
		FROM apps ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*App{}
	for rows.Next() {
		a := &App{}
		var pools, groups string
		if err := rows.Scan(&a.ID, &a.Name, &a.Note, &a.LatestVersion, &a.Labels, &a.CreatedAt, &a.UpdatedAt, &a.VersionCount, &pools, &groups); err != nil {
			return nil, err
		}
		// 排序保证输出稳定：group_concat 无 ORDER BY，行序不定会让结果抖动
		if pools != "" {
			a.Pools = strings.Split(pools, ",")
			slices.Sort(a.Pools)
		}
		if groups != "" {
			a.Groups = strings.Split(groups, ",")
			slices.Sort(a.Groups)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// GetApp 按 id 查询。
func (s *Store) GetApp(id int64) (*App, error) {
	row := s.db.QueryRow(`SELECT id, name, note, latest_version, labels, created_at, updated_at,
		(SELECT COUNT(*) FROM app_versions WHERE app_id = apps.id),
		(SELECT COALESCE(group_concat(pool, ','), '') FROM app_pools WHERE app_id = apps.id),
		(SELECT COALESCE(group_concat(group_name, ','), '') FROM app_groups WHERE app_id = apps.id)
		FROM apps WHERE id = ?`, id)
	a := &App{}
	var pools, groups string
	err := row.Scan(&a.ID, &a.Name, &a.Note, &a.LatestVersion, &a.Labels, &a.CreatedAt, &a.UpdatedAt, &a.VersionCount, &pools, &groups)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	// 排序保证输出稳定：group_concat 无 ORDER BY，行序不定会让结果抖动
	if pools != "" {
		a.Pools = strings.Split(pools, ",")
		slices.Sort(a.Pools)
	}
	if groups != "" {
		a.Groups = strings.Split(groups, ",")
		slices.Sort(a.Groups)
	}
	return a, nil
}

// ListVersions 应用的全部版本（新→旧）。
func (s *Store) ListVersions(appID int64) ([]*AppVersion, error) {
	rows, err := s.db.Query(`SELECT id, app_id, version, tgz_path, sha256, size, note, phases, created_at
		FROM app_versions WHERE app_id = ? ORDER BY id DESC`, appID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*AppVersion{}
	for rows.Next() {
		v := &AppVersion{}
		var phases string
		if err := rows.Scan(&v.ID, &v.AppID, &v.Version, &v.TgzPath, &v.Sha256, &v.Size, &v.Note, &phases, &v.CreatedAt); err != nil {
			return nil, err
		}
		v.Phases = parseStrings(phases)
		out = append(out, v)
	}
	return out, rows.Err()
}

// ErrVersionExists 版本号已存在：版本一经发布不可覆盖（真实版本控制），
// 需改 chart.yaml 的 version 后重传（或编辑器里改版本号保存为新版本）。
var ErrVersionExists = errors.New("version already exists")

// AddVersion 写入新版本并置为最新（版本级池/组/标签随版本落库：编辑保存
// = 底本 scope + 用户改动；chart 上传 = 继承当前应用级 scope）。phases 是
// 该版本 chart 的可用相位（调用方从制品提取，版本不可变故只写一次）。
// 版本号已存在返回 ErrVersionExists。
func (s *Store) AddVersion(appID int64, version, tgzPath, sha string, size int64, note string, pools, groups []string, labels string, phases []string) error {
	if version == "" {
		return fmt.Errorf("version is required")
	}
	var existing int64
	err := s.db.QueryRow(`SELECT id FROM app_versions WHERE app_id = ? AND version = ?`, appID, version).Scan(&existing)
	if err == nil {
		return fmt.Errorf("%w: %s", ErrVersionExists, version)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if labels == "" {
		labels = "{}"
	}
	// 与 CreateApp/UpdateAppScopes 同口径：非法 JSON 的 labels 一旦落库，
	// 权限层 labelKeys 解析失败返回空集，label 作用域限制会静默失效
	if err := validLabels(labels); err != nil {
		return err
	}
	// 版本落库与 latest 指针同事务：否则 latest 更新失败会留下"有新版本
	// 但默认版本还指向旧的"不一致
	return s.tx(func(q execer) error {
		if _, err := q.Exec(`INSERT INTO app_versions (app_id, version, tgz_path, sha256, size, note, pools, groups, labels, phases, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			appID, version, tgzPath, sha, size, note, scopeJSON(pools), scopeJSON(groups), labels, scopeJSON(phases), nowUTC()); err != nil {
			if isUniqueErr(err) {
				return fmt.Errorf("%w: %s", ErrVersionExists, version)
			}
			return err
		}
		_, err := q.Exec(`UPDATE apps SET latest_version = ?, updated_at = ? WHERE id = ?`, version, nowUTC(), appID)
		return err
	})
}

// scopeJSON 版本级 scope 列编码（” = 未设置，仅迁移前旧行）。
func scopeJSON(list []string) string {
	if len(list) == 0 {
		return "[]"
	}
	b, err := json.Marshal(list)
	if err != nil {
		return "[]"
	}
	return string(b)
}

// parseStrings 解码 JSON 字符串数组列（” 或 '[]' = nil）。
func parseStrings(s string) []string {
	if s == "" || s == "[]" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil
	}
	return out
}

// VersionPhases 某版本 chart 的可用相位（” 旧行返回 nil）。
func (s *Store) VersionPhases(appID int64, version string) ([]string, error) {
	var phases string
	err := s.db.QueryRow(`SELECT phases FROM app_versions WHERE app_id = ? AND version = ?`, appID, version).Scan(&phases)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return parseStrings(phases), nil
}

// VersionScopes 读某版本的池/组/标签。列值为 ”（迁移前旧行）时返回
// ok=false，调用方回退应用级 scope（旧行为）。
func (s *Store) VersionScopes(appID int64, version string) (pools, groups []string, labels string, ok bool, err error) {
	var pj, gj string
	err = s.db.QueryRow(`SELECT pools, groups, labels FROM app_versions WHERE app_id = ? AND version = ?`, appID, version).Scan(&pj, &gj, &labels)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, "", false, ErrNotFound
	}
	if err != nil {
		return nil, nil, "", false, err
	}
	if pj == "" || gj == "" { // 旧行未迁移 scope → 回退应用级
		return nil, nil, "", false, nil
	}
	if err := json.Unmarshal([]byte(pj), &pools); err != nil {
		return nil, nil, "", false, err
	}
	if err := json.Unmarshal([]byte(gj), &groups); err != nil {
		return nil, nil, "", false, err
	}
	if labels == "" {
		labels = "{}"
	}
	return pools, groups, labels, true, nil
}

// HasVersion 版本号是否已存在（保存前预检，给出可读提示）。
func (s *Store) HasVersion(appID int64, version string) (bool, error) {
	var id int64
	err := s.db.QueryRow(`SELECT id FROM app_versions WHERE app_id = ? AND version = ?`, appID, version).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// AppByName 按名查询应用（上传按 chart.yaml 名称归位用）。
func (s *Store) AppByName(name string) (*App, error) {
	var id int64
	err := s.db.QueryRow(`SELECT id FROM apps WHERE name = ?`, name).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return s.GetApp(id)
}

// DeleteVersion 删除版本；删的是最新版时以剩余最新版顶替，无剩余则清空。
// 版本删除与 latest 重算同事务，错误传播（吞错会留下没人重算的旧 latest）。
func (s *Store) DeleteVersion(appID, versionID int64) (string, error) {
	var version, tgz string
	if err := s.db.QueryRow(`SELECT version, tgz_path FROM app_versions WHERE id = ? AND app_id = ?`, versionID, appID).Scan(&version, &tgz); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrNotFound
		}
		return "", err
	}
	a, err := s.GetApp(appID)
	if err != nil {
		return "", err
	}
	var latest string
	if a.LatestVersion == version {
		// 预读删除后的最新版（等价于删除后 ORDER BY id DESC LIMIT 1，排除本行；
		// 查询须在事务外——单连接下事务内嵌套查询会死锁）
		if err := s.db.QueryRow(`SELECT version FROM app_versions WHERE app_id = ? AND id != ? ORDER BY id DESC LIMIT 1`, appID, versionID).Scan(&latest); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return "", err
		}
	}
	err = s.tx(func(q execer) error {
		res, err := q.Exec(`DELETE FROM app_versions WHERE id = ?`, versionID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		if a.LatestVersion == version {
			_, err = q.Exec(`UPDATE apps SET latest_version = ?, updated_at = ? WHERE id = ?`, latest, nowUTC(), appID)
		} else {
			_, err = q.Exec(`UPDATE apps SET updated_at = ? WHERE id = ?`, nowUTC(), appID)
		}
		return err
	})
	if err != nil {
		return "", err
	}
	return tgz, nil
}

// SetLatestVersion 指定某版本为默认版本（latest）。版本必须存在。
func (s *Store) SetLatestVersion(appID int64, version string) error {
	var id int64
	err := s.db.QueryRow(`SELECT id FROM app_versions WHERE app_id = ? AND version = ?`, appID, version).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: version %s", ErrNotFound, version)
	}
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`UPDATE apps SET latest_version = ?, updated_at = ? WHERE id = ?`, version, nowUTC(), appID)
	return err
}

// VersionTgz 返回版本制品路径。
func (s *Store) VersionTgz(appID int64, version string) (string, error) {
	var tgz string
	err := s.db.QueryRow(`SELECT tgz_path FROM app_versions WHERE app_id = ? AND version = ?`, appID, version).Scan(&tgz)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return tgz, err
}

// UpdateAppScopes 更新应用作用域（note 一并），并同步写入当前默认版本的
// 版本级 scope——编辑器按底本版本加载 scope，两处口径必须一致。
func (s *Store) UpdateAppScopes(id int64, note string, pools, groups []string, labels string) error {
	if labels == "" {
		labels = "{}"
	}
	if err := validLabels(labels); err != nil {
		return err
	}
	// 应用级与默认版本级 scope 同事务：两处口径必须一致，写一半即不一致
	return s.tx(func(q execer) error {
		res, err := q.Exec(`UPDATE apps SET note = ?, labels = ?, updated_at = ? WHERE id = ?`, note, labels, nowUTC(), id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		if err := setAppScopes(q, id, pools, groups); err != nil {
			return err
		}
		_, err = q.Exec(`UPDATE app_versions SET pools = ?, groups = ?, labels = ?
			WHERE app_id = ? AND version = (SELECT latest_version FROM apps WHERE id = ?)`,
			scopeJSON(pools), scopeJSON(groups), labels, id, id)
		return err
	})
}

// UpdateVersionScopes 就地变更某版本的作用域（不产生新版本——后期的
// 权限/归属调整）。返回是否为默认版本行，调用方据此决定是否回写应用级
// scope（应用级 = 最近保存口径，仅 latest 同步）。
func (s *Store) UpdateVersionScopes(appID int64, version string, pools, groups []string, labels string) (isLatest bool, err error) {
	if labels == "" {
		labels = "{}"
	}
	if err := validLabels(labels); err != nil {
		return false, err
	}
	var latest string
	err = s.db.QueryRow(`SELECT latest_version FROM apps WHERE id = ?`, appID).Scan(&latest)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrNotFound
	}
	if err != nil {
		return false, err
	}
	res, err := s.db.Exec(`UPDATE app_versions SET pools = ?, groups = ?, labels = ? WHERE app_id = ? AND version = ?`,
		scopeJSON(pools), scopeJSON(groups), labels, appID, version)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, ErrNotFound
	}
	return version == latest, nil
}

// DeleteApp 删除应用与全部版本记录（磁盘制品路径由调用方清理）。
func (s *Store) DeleteApp(id int64) ([]string, error) {
	vs, err := s.ListVersions(id)
	if err != nil {
		return nil, err
	}
	paths := []string{}
	for _, v := range vs {
		paths = append(paths, v.TgzPath)
	}
	// 应用、版本与作用域同事务：留一半会出现查不到应用却占着版本号的孤儿
	err = s.tx(func(q execer) error {
		if _, err := q.Exec(`DELETE FROM app_versions WHERE app_id = ?`, id); err != nil {
			return err
		}
		if _, err := q.Exec(`DELETE FROM app_pools WHERE app_id = ?`, id); err != nil {
			return err
		}
		if _, err := q.Exec(`DELETE FROM app_groups WHERE app_id = ?`, id); err != nil {
			return err
		}
		res, err := q.Exec(`DELETE FROM apps WHERE id = ?`, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return paths, nil
}
