package dialect

import (
	"database/sql"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	mysqldrv "github.com/go-sql-driver/mysql"
)

// mysqlDialect 是 MySQL / MariaDB 方言。
//
// 关键差异（相对规范 SQL）：
//   - `||` 是逻辑或，字符串拼接必须写 CONCAT(...)
//   - INSERT OR IGNORE → INSERT IGNORE
//   - ON CONFLICT .. excluded.x → ON DUPLICATE KEY UPDATE .. VALUES(x)
//   - 唯一冲突是错误码 1062（ER_DUP_ENTRY）
//   - 建表用 AUTO_INCREMENT + 定长/变长类型，TEXT 不能作主键或索引前缀之外的键
type mysqlDialect struct{}

func (mysqlDialect) Name() string { return "mysql" }

func (mysqlDialect) Driver() string { return "mysql" }

// DSN 生成 MySQL 连接串。缺省 utf8mb4（旧参考实现用 utf8，等价于 utf8mb3，
// 存不下 emoji 与部分 CJK 扩展字符——wdp 的 task 名/命令输出会带这类字符）。
// TLS 缺省不强制（MySQL 的 tls 参数需要服务端证书配置，盲开会导致存量自建库
// 连不上）；密码保护靠「不进命令行 + 日志遮罩」，见 store.ParseDSN 与 server。
func (mysqlDialect) DSN(cfg Config) string {
	my := mysqldrv.NewConfig()
	host := cfg.Host
	if host == "" {
		host = "127.0.0.1"
	}
	port := cfg.Port
	if port == 0 {
		port = 3306
	}
	my.User = cfg.User
	if my.User == "" {
		my.User = "root"
	}
	my.Passwd = cfg.Password
	my.Net = "tcp"
	my.Addr = net.JoinHostPort(host, strconv.Itoa(port))
	my.DBName = cfg.DBName
	if my.DBName == "" {
		my.DBName = "wdp"
	}
	my.Params = map[string]string{
		"charset":   "utf8mb4",
		"collation": "utf8mb4_general_ci",
		"parseTime": "true",
		"loc":       "UTC",
	}
	for k, v := range cfg.Params {
		my.Params[k] = v
	}
	my.AllowNativePasswords = true
	return my.FormatDSN()
}

func (mysqlDialect) Configure(db *sql.DB) {
	db.SetMaxOpenConns(16)
	db.SetMaxIdleConns(8)
	db.SetConnMaxLifetime(time.Hour)
	db.SetConnMaxIdleTime(10 * time.Minute)
}

func (mysqlDialect) Rewrite(sql string) string {
	sql = rewriteConcatForMySQL(sql)
	sql = rewriteInsertIgnoreMySQL(sql)
	sql = rewriteUpsertForMySQL(sql)
	return sql // 占位符就是 `?`，无需转换
}

func (mysqlDialect) GroupConcat(expr, sep string) string {
	// MySQL 的 group_concat 需要显式 SEPARATOR 关键字
	return fmt.Sprintf("group_concat(%s SEPARATOR %s)", expr, quoteLiteral(sep))
}

func (mysqlDialect) InsertIgnore(table string, cols []string) string {
	return fmt.Sprintf("INSERT IGNORE INTO %s (%s) VALUES (%s)",
		table, strings.Join(cols, ", "), placeholders(len(cols)))
}

func (mysqlDialect) Upsert(table string, cols, conflict, set []string) string {
	// conflict 列在 MySQL 由唯一键（UNIQUE/PRIMARY KEY）承担，语句里不出现
	items := make([]string, 0, len(set))
	for _, s := range set {
		items = append(items, strings.ReplaceAll(s, "excluded.", "VALUES(")+")")
	}
	return fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s) ON DUPLICATE KEY UPDATE %s",
		table, strings.Join(cols, ", "), placeholders(len(cols)), strings.Join(items, ", "))
}

func (mysqlDialect) AutoIncPK() string { return "BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY" }

// Text MySQL 的 TEXT 不能带缺省值（8.0.13 之前）且不能直接建唯一索引，
// 因此变长文本列用 VARCHAR(255)；长文本列（脚本快照等）在迁移里显式写 TEXT。
func (mysqlDialect) Text() string { return "VARCHAR(255)" }
func (mysqlDialect) Real() string { return "DOUBLE" }

// IsUniqueErr 判定错误码 1062（ER_DUP_ENTRY）。
func (mysqlDialect) IsUniqueErr(err error) bool {
	var myErr *mysqldrv.MySQLError
	if errors.As(err, &myErr) {
		return myErr.Number == 1062
	}
	return err != nil && strings.Contains(err.Error(), "Error 1062")
}

func (mysqlDialect) ColumnExists(q Queryer, table, column string) (bool, error) {
	var n int
	err := q.QueryRow(
		`SELECT count(*) FROM information_schema.columns
		  WHERE table_schema = DATABASE() AND table_name = ? AND column_name = ?`,
		strings.ToLower(table), strings.ToLower(column)).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

func (mysqlDialect) AddColumn(table, column, definition string) string {
	return fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, definition)
}
