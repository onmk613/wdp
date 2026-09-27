// Package store 是 web-console 的本地 SQLite 持久层：主机台账、管理员
// 账号与后续的操作审计。驱动用纯 Go 实现（modernc.org/sqlite），保持
// CGO_ENABLED=0 的交叉编译故事；WAL 模式 + busy_timeout，但连接限 1 条
// ——语句与事务全串行，天然避开 SQLITE_BUSY；代价是调用方不可在事务内
// 或迭代 rows 期间发起嵌套查询（单连接被占住，会死锁）。
package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// ErrNotFound 操作对象不存在（上层转 HTTP 404）。
var ErrNotFound = errors.New("not found")

// Store 包装 SQLite 连接。
type Store struct {
	db *sql.DB
}

var migrations = []string{`
CREATE TABLE hosts (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  name         TEXT NOT NULL UNIQUE,
  address      TEXT NOT NULL,
  agent_port   INTEGER NOT NULL DEFAULT 7602,
  group_name   TEXT NOT NULL DEFAULT '',
  labels       TEXT NOT NULL DEFAULT '{}',
  status       TEXT NOT NULL DEFAULT 'unknown',
  last_seen_at TEXT NOT NULL DEFAULT '',
  created_at   TEXT NOT NULL,
  updated_at   TEXT NOT NULL
);
CREATE TABLE users (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  name          TEXT NOT NULL UNIQUE,
  password_hash TEXT NOT NULL,
  created_at    TEXT NOT NULL
);
`, `
CREATE TABLE enroll_tokens (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  token         TEXT NOT NULL UNIQUE,
  host_name     TEXT NOT NULL DEFAULT '',
  agent_port    INTEGER NOT NULL DEFAULT 7602,
  created_at    TEXT NOT NULL,
  expires_at    TEXT NOT NULL,
  used_at       TEXT NOT NULL DEFAULT '',
  claim_host    TEXT NOT NULL DEFAULT '',
  claim_address TEXT NOT NULL DEFAULT ''
);
`, `
ALTER TABLE hosts ADD COLUMN pool TEXT NOT NULL DEFAULT '';
CREATE TABLE pools (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  name       TEXT NOT NULL UNIQUE,
  note       TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL
);
CREATE TABLE host_groups (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  name       TEXT NOT NULL UNIQUE,
  note       TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL
);
CREATE TABLE label_defs (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  key        TEXT NOT NULL UNIQUE,
  note       TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL
);
`, `
-- 池/组多值化：单值列搬迁到关系表后删除
CREATE TABLE host_pools (host_id INTEGER NOT NULL, pool TEXT NOT NULL, UNIQUE(host_id, pool));
CREATE TABLE host_group_map (host_id INTEGER NOT NULL, group_name TEXT NOT NULL, UNIQUE(host_id, group_name));
INSERT INTO host_pools (host_id, pool) SELECT id, pool FROM hosts WHERE pool != '';
INSERT INTO host_group_map (host_id, group_name) SELECT id, group_name FROM hosts WHERE group_name != '';
ALTER TABLE hosts DROP COLUMN pool;
ALTER TABLE hosts DROP COLUMN group_name;
-- 应用（chart 版本化）
CREATE TABLE apps (
  id             INTEGER PRIMARY KEY AUTOINCREMENT,
  name           TEXT NOT NULL UNIQUE,
  note           TEXT NOT NULL DEFAULT '',
  latest_version TEXT NOT NULL DEFAULT '',
  labels         TEXT NOT NULL DEFAULT '{}',
  created_at     TEXT NOT NULL,
  updated_at     TEXT NOT NULL
);
CREATE TABLE app_pools (app_id INTEGER NOT NULL, pool TEXT NOT NULL, UNIQUE(app_id, pool));
CREATE TABLE app_groups (app_id INTEGER NOT NULL, group_name TEXT NOT NULL, UNIQUE(app_id, group_name));
CREATE TABLE app_versions (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  app_id     INTEGER NOT NULL,
  version    TEXT NOT NULL,
  tgz_path   TEXT NOT NULL,
  sha256     TEXT NOT NULL DEFAULT '',
  size       INTEGER NOT NULL DEFAULT 0,
  note       TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  UNIQUE(app_id, version)
);
-- 执行记录（应用执行与远程命令共用；多应用顺序执行每个应用一行，seq 记序）
CREATE TABLE runs (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  kind       TEXT NOT NULL DEFAULT 'app',
  app_id     INTEGER NOT NULL DEFAULT 0,
  app_name   TEXT NOT NULL DEFAULT '',
  version    TEXT NOT NULL DEFAULT '',
  seq        INTEGER NOT NULL DEFAULT 0,
  status     TEXT NOT NULL DEFAULT 'running',
  selector   TEXT NOT NULL DEFAULT '',
  summary    TEXT NOT NULL DEFAULT '',
  started_at TEXT NOT NULL,
  finished_at TEXT NOT NULL DEFAULT ''
);
CREATE TABLE run_tasks (
  id      INTEGER PRIMARY KEY AUTOINCREMENT,
  run_id  INTEGER NOT NULL,
  play    TEXT NOT NULL DEFAULT '',
  task    TEXT NOT NULL DEFAULT '',
  module  TEXT NOT NULL DEFAULT '',
  host    TEXT NOT NULL DEFAULT '',
  status  TEXT NOT NULL DEFAULT '',
  changed INTEGER NOT NULL DEFAULT 0,
  detail  TEXT NOT NULL DEFAULT ''
);
`, `
ALTER TABLE app_versions ADD COLUMN pools TEXT NOT NULL DEFAULT '';
ALTER TABLE app_versions ADD COLUMN groups TEXT NOT NULL DEFAULT '';
ALTER TABLE app_versions ADD COLUMN labels TEXT NOT NULL DEFAULT '';
`, `
ALTER TABLE runs ADD COLUMN user TEXT NOT NULL DEFAULT '';
CREATE TABLE audit_logs (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  user       TEXT NOT NULL DEFAULT '',
  action     TEXT NOT NULL, -- create / update / delete / import / install / login / logout / set_latest / upload
  object     TEXT NOT NULL, -- host / pool / group / label / app / version / run / chart
  name       TEXT NOT NULL DEFAULT '',
  detail     TEXT NOT NULL DEFAULT '',
  ip         TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL
);
CREATE INDEX idx_audit_created ON audit_logs (id DESC);
`, `
CREATE TABLE metrics_5m (
  host_id INTEGER NOT NULL,
  metric  TEXT NOT NULL,
  labels  TEXT NOT NULL DEFAULT '',
  bucket  INTEGER NOT NULL, -- 5 分钟桶起点（Unix 秒）
  n       INTEGER NOT NULL DEFAULT 0,
  vsum    REAL NOT NULL DEFAULT 0,
  vmax    REAL NOT NULL DEFAULT 0,
  PRIMARY KEY (host_id, metric, labels, bucket)
);
CREATE TABLE host_alerts (
  host_id    INTEGER NOT NULL,
  kind       TEXT NOT NULL, -- cpu / mem / fs / offline
  level      TEXT NOT NULL, -- warn / crit
  detail     TEXT NOT NULL DEFAULT '',
  value      REAL NOT NULL DEFAULT 0,
  updated_at TEXT NOT NULL,
  PRIMARY KEY (host_id, kind)
);
`, `
ALTER TABLE app_versions ADD COLUMN phases TEXT NOT NULL DEFAULT ''; -- 相位清单（JSON 数组，版本创建时从 chart 提取；'' = 迁移前旧行）
ALTER TABLE runs ADD COLUMN phase TEXT NOT NULL DEFAULT ''; -- 本次执行使用的相位（app 类；exec 为空）
`, `
ALTER TABLE users ADD COLUMN role TEXT NOT NULL DEFAULT ''; -- admin / operator / viewer（空 = 迁移前旧行，按 operator 处理）
ALTER TABLE users ADD COLUMN disabled INTEGER NOT NULL DEFAULT 0;
CREATE TABLE user_scopes ( -- 细粒度授权：角色之外的按作用域追加授权（叠加模型）
  user_id INTEGER NOT NULL,
  verb    TEXT NOT NULL, -- 权限点，如 host:edit
  kind    TEXT NOT NULL DEFAULT '', -- '' = 全部 | pool | group | label
  value   TEXT NOT NULL DEFAULT '',
  UNIQUE(user_id, verb, kind, value)
);
`, `
CREATE TABLE app_drafts ( -- 编辑器草稿（按用户隔离；app_key = '<appID>' 或 'new:<应用名>'）
  user_id      INTEGER NOT NULL,
  app_key      TEXT NOT NULL,
  base_version TEXT NOT NULL DEFAULT '', -- 编辑底本（保存乐观锁回传）
  payload      TEXT NOT NULL,            -- 完整文件集 + UI 状态（JSON，前端定义）
  updated_at   TEXT NOT NULL,
  UNIQUE(user_id, app_key)
);
`, `
-- 明文通道显式声明：CA 启用时控制台→agent 默认走 mTLS（证书校验含主机名），
-- 只有明确标记的主机才允许明文 HTTP。此前"探活失败即回落明文"是可被中间人
-- 主动触发的降级（阻断 TLS 后自行应答即可拿到脚本/become 密码/制品）。
ALTER TABLE hosts ADD COLUMN allow_plaintext INTEGER NOT NULL DEFAULT 0;
-- 逐主机私钥交付标记：私钥只在"证书尚未交付"的窗口内可取，取走一次即
-- 作废该路径（纳管 token 会经 URL 进反代日志/shell 历史，仅靠 TTL 与
-- 来源 IP 绑定仍嫌宽）
ALTER TABLE enroll_tokens ADD COLUMN key_delivered_at TEXT NOT NULL DEFAULT '';
`, `
-- 热路径补索引（走新迁移版本，不改历史迁移）：
--   host_pools(pool) / host_group_map(group_name)：按池/组圈选主机
--   （ListHosts 的 scope 过滤）、池/组列表的成员计数与删除池/组时清理
--   成员关系，此前全部全表扫。host_id 一侧无需另建——建表时的
--   UNIQUE(host_id, pool/group_name) 前缀已覆盖按主机删/查。
--   runs(app_id, status) / runs(status)：执行互斥预检（同应用 queued/
--   running 判定）与启动期失败收尾按状态扫表；runs 随执行历史线性增长，
--   无索引时越用越慢。
CREATE INDEX idx_host_pools_pool ON host_pools (pool);
CREATE INDEX idx_host_group_map_group ON host_group_map (group_name);
CREATE INDEX idx_runs_app_status ON runs (app_id, status);
CREATE INDEX idx_runs_status ON runs (status);
`}

// Open 打开（必要时创建）数据库并执行增量迁移。
func Open(path string) (*Store, error) {
	// synchronous(NORMAL)：WAL 下的推荐档——写不 fsync 每次提交，掉电
	// 最多丢最后几笔事务但不损坏库；监控指标/审计类高频小写收益明显。
	// foreign_keys(1)：schema 暂无 FOREIGN KEY 声明，pragma 目前空转，
	// 为将来声明外键的迁移预留；孤儿行防护现阶段靠同事务删除（见
	// DeletePool/DeleteGroup/DeleteUser 等的成对清理）。
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	// 单连接全串行：SQLite 单写者模型下语句/事务天然互斥，免去 BUSY 处理
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close 关闭底层连接。
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL)`); err != nil {
		return err
	}
	var version int
	row := s.db.QueryRow(`SELECT version FROM schema_version`)
	if err := row.Scan(&version); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if _, err := s.db.Exec(`INSERT INTO schema_version (version) VALUES (0)`); err != nil {
			return err
		}
	}
	for v := version; v < len(migrations); v++ {
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[v]); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("migration v%d: %w", v+1, err)
		}
		if _, err := tx.Exec(`UPDATE schema_version SET version = ?`, v+1); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func nowUTC() string { return time.Now().UTC().Format(time.RFC3339) }

// execer 是写操作的最小接口：*sql.DB 与 *sql.Tx 都满足，辅助函数在
// 事务内外共用同一份实现。Query/QueryRow 供事务内「先查后写」类互斥
// 使用（如 CreateRunsExclusive、DeleteVersion 的 latest 重算）——必须走
// q 自身（事务连接），事务内再走 s.db 查询单连接会被占住而死锁。
type execer interface {
	Exec(query string, args ...any) (sql.Result, error)
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

// tx 把多语句变更收进单事务：任一步失败整体回滚，杜绝半状态（如
// "先 DELETE 后 INSERT"的整体替换中途失败丢光旧数据）。fn 内不得再走
// s.db 查询——单连接已被事务占住，嵌套查询会死锁。
func (s *Store) tx(fn func(q execer) error) error {
	t, err := s.db.Begin()
	if err != nil {
		return err
	}
	if err := fn(t); err != nil {
		_ = t.Rollback()
		return err
	}
	return t.Commit()
}

// dedup 去空去重（保持顺序）。
func dedup(in []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, v := range in {
		v = strings.TrimSpace(v)
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

// bizErr 标记用户可见的业务校验错误（参数缺漏/格式/存在性冲突等固定
// 文案）。web 层据此分流：标记错误原样回 400，未标记错误（数据库引擎/
// IO）按内部错误脱敏 500——裸 SQL 错误串会暴露内部表结构与路径。
type bizErr struct{ error }

// Bizf 构造业务校验错误。
func Bizf(format string, a ...any) error { return bizErr{fmt.Errorf(format, a...)} }

// IsBizErr 报告错误链中是否含业务校验错误（web 层 400/500 分流依据）。
func IsBizErr(err error) bool {
	var b bizErr
	return errors.As(err, &b)
}

func validLabels(s string) error {
	var m map[string]string
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return Bizf("labels must be a JSON object of strings: %v", err)
	}
	return nil
}

// validScopeName 校验池/组/标签键命名。这些名字会进 group_concat 逗号
// 拼接列（读取端按逗号拆分），含逗号/空白会把一个名字拆成多个，写入即拒。
func validScopeName(what, name string) error {
	if strings.TrimSpace(name) == "" {
		return Bizf("%s is required", what)
	}
	if strings.ContainsAny(name, ", \t\r\n") {
		return Bizf("%s must not contain commas or whitespace", what)
	}
	return nil
}

// validScopeNames 校验一组归属名（主机/应用侧的池、组写入点共用）。
func validScopeNames(what string, list []string) error {
	for _, v := range list {
		if err := validScopeName(what, v); err != nil {
			return err
		}
	}
	return nil
}

// isUniqueErr 是否 SQLite 唯一约束冲突（modernc 驱动以错误串暴露）。
func isUniqueErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// IsUniqueErr 是 isUniqueErr 的导出形态：web 层用它区分"用户可见的
// 重名冲突"（400）与基础设施错误（500 脱敏，理由见 bizErr 注释）。
func IsUniqueErr(err error) bool { return isUniqueErr(err) }

// dupErr 把 SQLite 唯一约束错误翻译为可读的"已存在"语义（业务校验类，
// 经 Bizf 标记——web 层原样回 400 而非脱敏 500）。
func dupErr(err error, what, name string) error {
	if isUniqueErr(err) {
		return Bizf("%s %q already exists", what, name)
	}
	return err
}
