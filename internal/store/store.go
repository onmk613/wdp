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
	"regexp"
	"strings"
	"time"

	"wdp/internal/store/dialect"
)

// ErrNotFound 操作对象不存在（上层转 HTTP 404）。
var ErrNotFound = errors.New("not found")

// Store 包装数据库连接与方言。
type Store struct {
	// raw 是底层连接。除方言无关的系统操作外不要直接用——业务读写走
	// exec/query/queryRow，它们会做方言改写（见各方法注释）。
	raw *sql.DB
	// d 是方言（sqlite / postgres / mysql）。
	d dialect.Dialect
	// addr 是可外显的库地址（密码已遮罩）。
	addr string
}

// Open 打开（必要时创建）SQLite 数据库并执行增量迁移。
//
// 这是历史入口，语义不变：path 是 SQLite 文件路径。要连 MySQL/PostgreSQL 或
// 带连接参数，用 OpenDSN（CLI 的 --db 走那条路）。
func Open(path string) (*Store, error) {
	return OpenDSN(DSN{
		Dialect:   mustDialect("sqlite"),
		Config:    applyDefaults("sqlite", dialect.Config{Path: path}),
		driverDSN: mustDialect("sqlite").DSN(applyDefaults("sqlite", dialect.Config{Path: path})),
		Redacted:  path,
	})
}

// OpenDSN 按解析后的库地址打开数据库并执行增量迁移。支持 sqlite / postgres /
// mysql 三种方言（方言差异见 internal/store/dialect）。
//
// 连接池由方言自行配置：SQLite 限单连接（单写者模型，语句与事务天然互斥），
// 网络库给正常池。因此调用方仍需遵守「事务内不得再走 Store 的其它方法」——
// SQLite 下会死锁，网络库下只是多占一条连接。
func OpenDSN(d DSN) (*Store, error) {
	if d.Dialect == nil {
		return nil, errors.New("store: nil dialect")
	}
	db, err := sql.Open(d.Driver(), d.DriverDSN())
	if err != nil {
		return nil, err
	}
	d.Dialect.Configure(db)
	s := &Store{raw: db, d: d.Dialect, addr: d.Redacted}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate %s database: %w", d.Redacted, err)
	}
	return s, nil
}

// Close 关闭底层连接。
func (s *Store) Close() error { return s.raw.Close() }

// DB 暴露底层连接（系统级操作用；业务读写一律走 Store 方法，不在这里绕过）。
// 经它执行的 SQL 不经过方言改写，除 SQLite 外不要用。
func (s *Store) DB() *sql.DB { return s.raw }

// Address 返回可安全外显的库地址（密码已遮罩），用于日志与错误信息。
func (s *Store) Address() string { return s.addr }

// DialectName 返回方言名（sqlite / postgres / mysql）。
func (s *Store) DialectName() string { return s.d.Name() }

// --- 方言感知的查询入口 ---
//
// 所有业务读写都走下面四个方法：它们把 SQL 交给方言改写（占位符、聚合、
// 冲突子句、字符串拼接）后再执行。直接调 s.raw 会跳过改写——除 SQLite 外
// 都可能语法错，因此 s.raw 只应出现在方言无关的系统操作里（如 Ping）。

// exec 执行写语句（方言改写后）。
func (s *Store) exec(query string, args ...any) (sql.Result, error) {
	return s.raw.Exec(s.d.Rewrite(query), args...)
}

// query 执行查询（方言改写后）。
func (s *Store) query(query string, args ...any) (*sql.Rows, error) {
	return s.raw.Query(s.d.Rewrite(query), args...)
}

// queryRow 执行单行查询（方言改写后）。
func (s *Store) queryRow(query string, args ...any) *sql.Row {
	return s.raw.QueryRow(s.d.Rewrite(query), args...)
}

// rawExecer 把 Store 自身适配成 execer：无事务的写路径（如 UpsertHostByName）
// 也要用 lastInsertID 的方言分支，因此需要一个非事务的 execer。
func (s *Store) rawExecer() execer { return &storeExecer{s: s} }

// storeExecer 是 execer 的非事务实现（方言改写 + 直连）。
type storeExecer struct{ s *Store }

func (e *storeExecer) Exec(query string, args ...any) (sql.Result, error) {
	return e.s.exec(query, args...)
}
func (e *storeExecer) Query(query string, args ...any) (*sql.Rows, error) {
	return e.s.query(query, args...)
}
func (e *storeExecer) QueryRow(query string, args ...any) *sql.Row {
	return e.s.queryRow(query, args...)
}

// lastInsertID 回读刚插入行的自增主键。
//
// PostgreSQL 的驱动不支持 Result.LastInsertId，INSERT 必须以 `RETURNING id`
// 结尾并走 queryRow；SQLite 与 MySQL（含 MariaDB）用 LastInsertId 更省事，
// 也避免依赖 MySQL 8.0.19+ 才有的 INSERT ... RETURNING。
//
// insertSQL 必须是**不带** RETURNING 的 INSERT；args 与其占位符一一对应。
func (s *Store) lastInsertID(r execer, insertSQL string, args ...any) (int64, error) {
	if s.d.Name() == "postgres" {
		var id int64
		if err := r.QueryRow(insertSQL+" RETURNING id", args...).Scan(&id); err != nil {
			return 0, err
		}
		return id, nil
	}
	res, err := r.Exec(insertSQL, args...)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// migrate 建表并执行增量迁移。
//
// 与历史实现的区别：DDL 逐条执行（PG 驱动的 Exec 不接受一次多条语句），
// 且对 SQLite 专有写法做方言改写（自增主键、类型、冲突子句）。
func (s *Store) migrate() error {
	if _, err := s.exec(`CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL)`); err != nil {
		return err
	}
	var version int
	err := s.queryRow(`SELECT version FROM schema_version`).Scan(&version)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		// 全新库：MySQL/PG 无「空表 SELECT 返回 0 行」之外的坑，插入 0 行即可
		if _, err := s.exec(`INSERT INTO schema_version (version) VALUES (?)`, 0); err != nil {
			return err
		}
	}
	// 网络库（MySQL/PG）没有历史包袱：新建库先执行一份基线（当前 schema 的
	// 等价物），把版本号直接推到基线末尾，避免重放 SQLite 专有的历史迁移。
	// 存量 SQLite 库走原路逐条重放（版本号与对象状态强相关，不能走捷径）。
	if version == 0 {
		base := baselineFor(s.d.Name())
		if len(base) > 0 {
			if err := s.applyBaseline(base); err != nil {
				return err
			}
			version = baselineCount(s.d.Name())
		}
	}
	for v := version; v < migrationCount(s.d.Name()); v++ {
		if err := s.applyMigration(v); err != nil {
			return fmt.Errorf("migration v%d: %w", v+1, err)
		}
	}
	return nil
}

// applyMigration 在单事务里执行第 v 条迁移（0 基）并推进版本号。
func (s *Store) applyMigration(v int) error {
	stmts, err := s.prepareMigration(migrationFor(s.d.Name(), v))
	if err != nil {
		return err
	}
	tx, err := s.raw.Begin()
	if err != nil {
		return err
	}
	t := &txExecer{tx: tx, d: s.d}
	for _, st := range stmts {
		if _, err := t.Exec(st); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	if _, err := t.Exec(`UPDATE schema_version SET version = ?`, v+1); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
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
	t, err := s.raw.Begin()
	if err != nil {
		return err
	}
	if err := fn(&txExecer{tx: t, d: s.d}); err != nil {
		_ = t.Rollback()
		return err
	}
	return t.Commit()
}

// txExecer 是事务内的 execer 实现：与 Store 的 exec/query/queryRow 一样先做
// 方言改写再执行，保证事务内外 SQL 走同一条翻译路径。
type txExecer struct {
	tx *sql.Tx
	d  dialect.Dialect
}

func (t *txExecer) Exec(query string, args ...any) (sql.Result, error) {
	return t.tx.Exec(t.d.Rewrite(query), args...)
}

func (t *txExecer) Query(query string, args ...any) (*sql.Rows, error) {
	return t.tx.Query(t.d.Rewrite(query), args...)
}

func (t *txExecer) QueryRow(query string, args ...any) *sql.Row {
	return t.tx.QueryRow(t.d.Rewrite(query), args...)
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

// isUniqueErr 是否是唯一约束冲突。各库判定方式不同（SQLite 是错误串、
// PG 是 SQLSTATE 23505、MySQL 是错误码 1062），统一问方言。
func (s *Store) isUniqueErr(err error) bool { return s.d.IsUniqueErr(err) }

// IsUniqueErr 报告错误是否唯一约束冲突：web 层用它区分"用户可见的重名冲突"
// （400）与基础设施错误（500 脱敏，理由见 bizErr 注释）。
//
// 这里无法问方言（没有 Store 实例），故按三库特征做**并集**判定：SQLite 错误
// 串、PG 的 SQLSTATE 23505、MySQL 错误码 1062 都认。代价是理论上可能把某个
// 库的其它 23505 误判为重名——但 23505 在 PG 里就是 unique_violation 的语义。
func IsUniqueErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed") ||
		strings.Contains(msg, "constraint failed: PRIMARY KEY") ||
		strings.Contains(msg, "SQLSTATE 23505") ||
		regexp.MustCompile(`(?i)(Error 1062|Duplicate entry)`).MatchString(msg)
}

// dupErr 把唯一约束冲突翻译为可读的"已存在"语义（业务校验类，经 Bizf 标记
// ——web 层原样回 400 而非脱敏 500）。判定走方言，三库通用。
func (s *Store) dupErr(err error, what, name string) error {
	if s.isUniqueErr(err) {
		return Bizf("%s %q already exists", what, name)
	}
	return err
}
