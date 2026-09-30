package dialect

import (
	"database/sql"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

// sqliteDialect 是 SQLite 方言。规范 SQL 即 SQLite 写法，因此 Rewrite 是恒等
// 变换——它同时充当「规范写法」的可执行定义：别的方言必须把规范写法翻译成
// 自己的（见 rewrite.go 顶部约定）。
type sqliteDialect struct{}

func (sqliteDialect) Name() string { return "sqlite" }

func (sqliteDialect) Driver() string { return "sqlite" }

// DSN 生成 SQLite 连接串。与历史行为一致：WAL + busy_timeout + 外键开关，
// 参数可经 Params 覆盖（如 synchronous）。
func (sqliteDialect) DSN(cfg Config) string {
	pragmas := []string{
		"busy_timeout(" + paramOr(cfg.Params, "busy_timeout", "5000") + ")",
		"journal_mode(" + paramOr(cfg.Params, "journal_mode", "WAL") + ")",
		"synchronous(" + paramOr(cfg.Params, "synchronous", "NORMAL") + ")",
		"foreign_keys(" + paramOr(cfg.Params, "foreign_keys", "1") + ")",
	}
	var extra []string
	for k, v := range cfg.Params {
		switch k {
		case "busy_timeout", "journal_mode", "synchronous", "foreign_keys", "_pragma":
			continue
		}
		extra = append(extra, k+"="+url.QueryEscape(v))
	}
	sort.Strings(extra)
	dsn := cfg.Path
	q := make([]string, 0, len(pragmas)+len(extra))
	for _, p := range pragmas {
		q = append(q, "_pragma="+p)
	}
	q = append(q, extra...)
	if len(q) > 0 {
		sep := "?"
		if strings.Contains(dsn, "?") {
			sep = "&"
		}
		dsn += sep + strings.Join(q, "&")
	}
	return dsn
}

// Configure 限单连接：SQLite 单写者模型下语句/事务天然互斥，免去 BUSY 处理。
// 代价是调用方不可在事务内或迭代 rows 期间发起嵌套查询（会死锁）。
func (sqliteDialect) Configure(db *sql.DB) { db.SetMaxOpenConns(1) }

func (sqliteDialect) Rewrite(sql string) string { return stripConcatToken(sql) }

func (sqliteDialect) GroupConcat(expr, sep string) string {
	return fmt.Sprintf("group_concat(%s, %s)", expr, quoteLiteral(sep))
}

func (sqliteDialect) InsertIgnore(table string, cols []string) string {
	return fmt.Sprintf("INSERT OR IGNORE INTO %s (%s) VALUES (%s)",
		table, strings.Join(cols, ", "), placeholders(len(cols)))
}

func (sqliteDialect) Upsert(table string, cols, conflict, set []string) string {
	return fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s) ON CONFLICT(%s) DO UPDATE SET %s",
		table, strings.Join(cols, ", "), placeholders(len(cols)),
		strings.Join(conflict, ", "), strings.Join(set, ", "))
}

func (sqliteDialect) AutoIncPK() string { return "INTEGER PRIMARY KEY AUTOINCREMENT" }
func (sqliteDialect) Text() string      { return "TEXT" }
func (sqliteDialect) Real() string      { return "REAL" }

// IsUniqueErr 判定唯一约束冲突。modernc 驱动以错误串暴露（无 SQLSTATE/错误码），
// 匹配 "UNIQUE constraint failed" 与主键冲突（PK 在 SQLite 里也走这条）。
func (sqliteDialect) IsUniqueErr(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "UNIQUE constraint failed") ||
		strings.Contains(s, "constraint failed: PRIMARY KEY")
}

// ColumnExists 经 PRAGMA table_info 探测（SQLite 无 information_schema）。
// PRAGMA 不接受占位符，表名因此必须通过标识符校验后再拼接。
func (sqliteDialect) ColumnExists(q Queryer, table, column string) (bool, error) {
	if !identRe.MatchString(table) {
		return false, fmt.Errorf("invalid table name %q", table)
	}
	rows, err := q.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			cid     int
			name    string
			ctype   string
			notnull int
			dflt    sql.NullString
			pk      int
		)
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return false, err
		}
		if strings.EqualFold(name, column) {
			return true, nil
		}
	}
	return false, rows.Err()
}

// AddColumn SQLite 不支持 IF NOT EXISTS，须先由 ColumnExists 探测。
func (sqliteDialect) AddColumn(table, column, definition string) string {
	return fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, definition)
}

// identRe 限定可安全拼接的标识符（表名/列名来自代码常量，这里做防御性校验：
// 任何人日后从输入拼表名都会被挡下）。
var identRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// placeholders 生成 n 个 `?` 占位（规范写法）。
func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?, ", n), ", ")
}

// quoteLiteral 把字符串转成 SQL 字面量（仅用于分隔符这类常量）。
func quoteLiteral(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// paramOr 取参数值，缺省用 def。
func paramOr(params map[string]string, key, def string) string {
	if v, ok := params[key]; ok && v != "" {
		return v
	}
	return def
}
