package dialect

import (
	"database/sql"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// postgresDialect 是 PostgreSQL 方言。
//
// 关键差异（相对规范 SQL）：
//   - 占位符 $1..$n（`?` 在 PG 里是 JSON/JSONB 运算符，不能直接复用）
//   - group_concat → string_agg
//   - INSERT OR IGNORE → ON CONFLICT DO NOTHING
//   - 无 LastInsertId：store 层统一用 RETURNING id 回读自增主键
//   - 唯一冲突是 SQLSTATE 23505
//   - ALTER TABLE ADD COLUMN 支持 IF NOT EXISTS，但仍走 ColumnExists 探测以
//     保持三库同一路径（探测失败即报错，不静默跳过）
type postgresDialect struct{}

func (postgresDialect) Name() string { return "postgres" }

func (postgresDialect) Driver() string { return "pgx" }

// DSN 生成 PG 连接串。缺省 sslmode=require：远程库不走明文（这是「安全加工」
// 的一条——旧参考实现缺省 sslmode=disable，这里刻意反过来）。本地 socket 或
// 确有需要的场景可由 URL 参数显式覆盖为 disable/prefer。
func (postgresDialect) DSN(cfg Config) string {
	params := map[string]string{"sslmode": "require"}
	for k, v := range cfg.Params {
		params[k] = v
	}
	host := cfg.Host
	if host == "" {
		host = "127.0.0.1"
	}
	port := cfg.Port
	if port == 0 {
		port = 5432
	}
	user := cfg.User
	if user == "" {
		user = "postgres"
	}
	dbname := cfg.DBName
	if dbname == "" {
		dbname = "postgres"
	}
	u := &url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(user, cfg.Password),
		Host:   net.JoinHostPort(host, strconv.Itoa(port)),
		Path:   "/" + dbname,
	}
	q := u.Query()
	for k, v := range params {
		q.Set(k, v)
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// Configure 给网络库正常的连接池。MaxOpenConns 留 0（不限制）会在大批量并发
// 时把库打满，这里取一个温和上限并限制连接寿命（配合 PG 侧的 idle 回收）。
func (postgresDialect) Configure(db *sql.DB) {
	db.SetMaxOpenConns(16)
	db.SetMaxIdleConns(8)
	db.SetConnMaxLifetime(time.Hour)
	db.SetConnMaxIdleTime(10 * time.Minute)
}

func (postgresDialect) Rewrite(sql string) string {
	sql = concatJoinAll(stripConcatToken(sql))
	sql = rewriteGroupConcat(sql, "string_agg")
	sql = rewriteInsertIgnorePG(sql)
	sql = replacePlaceholders(sql, dollarPlaceholder)
	return sql
}

func (postgresDialect) GroupConcat(expr, sep string) string {
	return fmt.Sprintf("string_agg(%s, %s)", expr, quoteLiteral(sep))
}

func (postgresDialect) InsertIgnore(table string, cols []string) string {
	return fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s) ON CONFLICT DO NOTHING",
		table, strings.Join(cols, ", "), dollarPlaceholders(len(cols)))
}

func (postgresDialect) Upsert(table string, cols, conflict, set []string) string {
	return fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s) ON CONFLICT(%s) DO UPDATE SET %s",
		table, strings.Join(cols, ", "), dollarPlaceholders(len(cols)),
		strings.Join(conflict, ", "), strings.Join(set, ", "))
}

func (postgresDialect) AutoIncPK() string { return "BIGSERIAL PRIMARY KEY" }
func (postgresDialect) Text() string      { return "TEXT" }
func (postgresDialect) Real() string      { return "DOUBLE PRECISION" }

// IsUniqueErr 判定 SQLSTATE 23505（unique_violation）。
func (postgresDialect) IsUniqueErr(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "23505"
	}
	// 驱动被换掉时退回字符串判定，避免「重名冲突变成 500」这种静默降级。
	return err != nil && strings.Contains(err.Error(), "SQLSTATE 23505")
}

func (postgresDialect) ColumnExists(q Queryer, table, column string) (bool, error) {
	var n int
	err := q.QueryRow(
		`SELECT count(*) FROM information_schema.columns
		  WHERE table_name = $1 AND column_name = $2 AND table_schema = current_schema()`,
		strings.ToLower(table), strings.ToLower(column)).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// AddColumn PG 支持 IF NOT EXISTS，但这里不加：调用方一律先经 ColumnExists
// 探测，三条库走同一路径，避免只有 PG 才有的静默分支。
func (postgresDialect) AddColumn(table, column, definition string) string {
	return fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, definition)
}

// dollarPlaceholders 生成 $1..$n。
func dollarPlaceholders(n int) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = "$" + strconv.Itoa(i+1)
	}
	return strings.Join(parts, ", ")
}
