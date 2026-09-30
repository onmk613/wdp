package store

// 数据库驱动注册。
//
// 三个驱动都在这里 blank import：store 层按方言名调用 sql.Open，缺一个注册就会
// 在运行期报 `sql: unknown driver`（编译期看不出来）。测试与生产共用这一份注册，
// 避免"测试能连、线上连不上"这类只在部署时暴露的问题。
import (
	_ "github.com/go-sql-driver/mysql" // mysql 方言
	_ "github.com/jackc/pgx/v5/stdlib" // postgres 方言（pgx 的 database/sql 适配层）
	_ "modernc.org/sqlite"             // sqlite 方言（纯 Go，CGO_ENABLED=0 交叉编译）
)
