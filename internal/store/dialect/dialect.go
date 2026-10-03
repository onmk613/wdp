// Package dialect 承载 wdp 持久层的数据库方言差异。
//
// 设计取向：**不引入 ORM**。store 层是 4300 行手写 SQL，改成 GORM 之类的
// 抽象等于重写整个持久层且丢掉落库 SQL 的可审计性；这里只把「三个库真的
// 不一样」的那几处抽出来，其余 SQL 保持一处一份：
//
//	占位符      ?        → PG 要 $1..$n
//	字符串拼接  ||       → MySQL 要 CONCAT()
//	聚合        group_concat → PG string_agg
//	冲突忽略    INSERT OR IGNORE → PG ON CONFLICT DO NOTHING / MySQL INSERT IGNORE
//	Upsert      ON CONFLICT .. excluded.x → MySQL 用 VALUES(x)
//	自增主键    INTEGER PRIMARY KEY AUTOINCREMENT → PG BIGSERIAL / MySQL BIGINT AUTO_INCREMENT
//	自增回读    LastInsertId() → 统一改 RETURNING id（PG 驱动不支持前者）
//	唯一冲突    "UNIQUE constraint failed" 错误串 → PG/MySQL 各有 SQLSTATE/错误码
//	加列        ALTER TABLE ADD COLUMN → 只有 PG 支持 IF NOT EXISTS，需先探测
//
// SQL 里出现 `{{concat}}` 这类 token 的，由各实现替换为该库的写法；token 集合
// 刻意保持极小（见下），新增 token 必须三个实现同时给出答案，否则编译/对账
// 测试会拦下。
package dialect

import (
	"database/sql"
	"errors"
	"fmt"
)

// 方言内使用、由各实现替换的 SQL token。刻意只有两个：其余差异（自增主键、
// 冲突忽略、聚合）用 Dialect 的方法生成，避免拼写错误导致的静默漏改。
const (
	// TokenConcat 字符串拼接算子：SQLite/PG 是 `||`，MySQL 是 CONCAT(a, b)。
	// 用法：`a {{concat}} '-' {{concat}} b`（MySQL 会重排为 CONCAT(a, '-', b)）。
	TokenConcat = "{{concat}}"
)

// Dialect 是一个数据库方言。实现必须无状态、可并发调用。
type Dialect interface {
	// Name 方言名（sqlite / postgres / mysql），用于日志与错误信息。
	Name() string
	// Driver 是 database/sql 的驱动名。
	Driver() string
	// DSN 由已解析的连接配置生成驱动专用连接串。
	DSN(cfg Config) string

	// Configure 设置连接池参数。SQLite 必须限单连接（单写者模型下语句与
	// 事务天然互斥），网络库则按并发度给池。
	Configure(db *sql.DB)

	// Rewrite 把 SQL 规范化为本方言写法：转换占位符、替换 token、按需改写
	// 聚合与冲突子句之外的部分。实现必须跳过字符串字面量与注释。
	Rewrite(sql string) string

	// GroupConcat 生成 `GROUP_CONCAT` 等价表达式（含分隔符与 COALESCE 惯例）。
	GroupConcat(expr, sep string) string
	// InsertIgnore 生成「插入并忽略唯一冲突」语句。
	InsertIgnore(table string, cols []string) string
	// Upsert 生成「按 conflict 列 upsert，更新 set 列」语句：set 是
	// "col = <新值表达式>" 的列表，其中目标列用 excluded 前缀（MySQL 由
	// 实现翻译为 VALUES(col)）。
	Upsert(table string, cols, conflict, set []string) string
	// AutoIncPK 返回自增主键的列定义（建表基线用）。
	AutoIncPK() string
	// Text 返回变长文本列类型（建表基线用）。
	Text() string
	// Real 返回浮点列类型（建表基线用）。
	Real() string

	// IsUniqueErr 报告错误是否唯一约束冲突（web 层据此把重名回 400 而非
	// 500 脱敏，见 store.IsUniqueErr）。
	IsUniqueErr(err error) bool
	// ColumnExists 报告表上是否已有该列（ALTER TABLE ADD COLUMN 的可移植
	// 前提：只有 PG 支持 IF NOT EXISTS，另两个必须先探测）。
	ColumnExists(q Queryer, table, column string) (bool, error)
	// AddColumn 生成向已有表追加列的语句。
	AddColumn(table, column, definition string) string
}

// Queryer 是方言探测列存在性所需的最小查询接口（*sql.DB 与 *sql.Tx 都满足）。
type Queryer interface {
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

// Config 是解析后的连接配置（由 store 从库地址解析产出）。
type Config struct {
	// Path 是 SQLite 的库文件路径（仅 sqlite 方言使用）。
	Path string
	// Host / Port / User / Password / DBName 是网络库的连接要素。
	Host     string
	Port     int
	User     string
	Password string
	DBName   string
	// Params 是连接串附加参数（sslmode、charset、busy_timeout 等）。
	Params map[string]string
}

// registry 方言注册表。仅 init() 期写入，运行期只读（与 module 包同约定）。
var registry = map[string]Dialect{}

// Register 注册方言（仅 init() 期调用）。
func Register(d Dialect) { registry[d.Name()] = d }

// Get 按名取方言。
func Get(name string) (Dialect, error) {
	d, ok := registry[name]
	if !ok {
		return nil, fmt.Errorf("unsupported database dialect %q (supported: sqlite, postgres, mysql)", name)
	}
	return d, nil
}

// ErrNoColumn 在列存在性探测无法判定时返回（调用方应报错而非静默跳过）。
var ErrNoColumn = errors.New("cannot determine column existence")
