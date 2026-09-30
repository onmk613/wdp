package store

import (
	"database/sql"
	"fmt"
	"strings"

	"wdp/internal/store/dialect"
)

// 迁移加载与应用。
//
// 设计：迁移文本用**方言中立的写法**（见 dialect/rewrite.go 顶部约定），
// 落库前经方言改写。需要按方言分支的只有两处：
//
//  1. 建表基线：自增主键、文本/浮点列类型三库写法不同（dialect 的
//     AutoIncPK/Text/Real 给答案），因此基线迁移由 migrationFor 按方言选。
//  2. 加列：只有 PostgreSQL 支持 ADD COLUMN IF NOT EXISTS，另两库必须先探测
//     列是否存在，故 `{{addColumn table col def}}` 由 prepareMigration 展开。
//
// 其余迁移（建表、建索引、插数据）三库通用，只写一份。

// addColumnTokenPrefix 是加列指令的前缀：`-- +wdp:add-column table col def`。
// 用 SQL 注释形式书写，SQLite 直接执行时会被当注释忽略（历史库重放安全），
// 而 prepareMigration 会把它展开为真正的 DDL。
const addColumnTokenPrefix = "-- +wdp:add-column "

// migrationFor 返回第 v 条迁移的文本（0 基）。
//
// 建表基线（v0/v1 等）在 migrations 里给的是 SQLite 写法；MySQL 与 PostgreSQL
// 的基线不同（自增主键与列类型），由 dialectBaseline 生成。
func migrationFor(dialectName string, v int) string {
	if dialectName == "sqlite" {
		if v < 0 || v >= len(sqliteMigrations) {
			return ""
		}
		return sqliteMigrations[v]
	}
	// 网络库：v < sqliteLegacyVersions 的版本位由基线一次性覆盖（见
	// applyBaseline），这里只处理基线之后的新迁移。
	idx := v - sqliteLegacyVersions
	if idx < 0 || idx >= len(portableMigrations) {
		return ""
	}
	return portableMigrations[idx]
}

// baselineFor 返回某方言的基线语句（SQLite 无基线，走历史迁移线）。
func baselineFor(dialectName string) []string {
	switch dialectName {
	case "postgres":
		return postgresBaseline
	case "mysql":
		return mysqlBaseline
	default:
		return nil
	}
}

// prepareMigration 把迁移文本展开为可逐条执行的语句列表：
//   - 按顶层分号切分成单条语句（PG 驱动的 Exec 不接受一次多条）
//   - 处理 `-- +wdp:add-column` 指令：探测列是否存在，缺则加
func (s *Store) prepareMigration(text string) ([]string, error) {
	raw := dialect.SplitStatements(text)
	out := make([]string, 0, len(raw))
	for _, st := range raw {
		if !strings.HasPrefix(strings.TrimSpace(st), addColumnTokenPrefix) {
			out = append(out, st)
			continue
		}
		spec := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(st), addColumnTokenPrefix))
		table, col, def, ok := cutAddColumnSpec(spec)
		if !ok {
			return nil, fmt.Errorf("invalid add-column directive: %q", st)
		}
		exists, err := s.d.ColumnExists(&txQueryer{s: s}, table, col)
		if err != nil {
			return nil, fmt.Errorf("probe column %s.%s: %w", table, col, err)
		}
		if exists {
			continue // 历史库已有该列：跳过（等价于 ADD COLUMN IF NOT EXISTS）
		}
		out = append(out, s.d.AddColumn(table, col, def))
	}
	return out, nil
}

// cutAddColumnSpec 解析 `table col definition...`。
func cutAddColumnSpec(spec string) (table, col, def string, ok bool) {
	fields := strings.SplitN(spec, " ", 3)
	if len(fields) < 3 {
		return "", "", "", false
	}
	return fields[0], fields[1], fields[2], true
}

// txQueryer 是 ColumnExists 探测所需的 Queryer 适配器：探测必须走**同一个
// 连接**（探测结果直接决定本事务里是否发 DDL），所以用 Store 上的方法。
type txQueryer struct{ s *Store }

func (q *txQueryer) Query(query string, args ...any) (*sql.Rows, error) {
	return q.s.query(query, args...)
}

func (q *txQueryer) QueryRow(query string, args ...any) *sql.Row {
	return q.s.queryRow(query, args...)
}

// migrationCount 返回某方言的迁移总条数（= 版本号上限）。
//
// SQLite 用历史线长度；网络库用基线占位 + 通用增量线。两条线在"基线末尾"
// 对齐到同一个版本号，因此之后的 portableMigrations 对三库含义一致。
func migrationCount(dialectName string) int {
	if dialectName == "sqlite" {
		return len(sqliteMigrations) + len(portableMigrations)
	}
	return baselineCount(dialectName) + len(portableMigrations)
}

// baselineCount 返回某方言基线的"版本位"数。基线以单事务执行，但占用的版本号
// 与 SQLite 历史线对齐（SQLite 基线为 0——它走历史迁移线）。
func baselineCount(dialectName string) int {
	if dialectName == "sqlite" {
		return 0
	}
	return sqliteLegacyVersions
}

// applyBaseline 在新库上执行方言基线（单事务，版本号推到基线末尾）。
func (s *Store) applyBaseline(stmts []string) error {
	tx, err := s.raw.Begin()
	if err != nil {
		return err
	}
	t := &txExecer{tx: tx, d: s.d}
	for _, st := range stmts {
		if _, err := t.Exec(st); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("baseline: %w", err)
		}
	}
	if _, err := t.Exec(`UPDATE schema_version SET version = ?`, sqliteLegacyVersions); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}
