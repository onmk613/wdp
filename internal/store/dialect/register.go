package dialect

// 方言注册。三个实现都是编译期常量对象，无状态、可并发调用。
//
// init 期注册（与 internal/module 的注册表同约定）：store 层只需 import 本包，
// 三个方言即在册；新增方言在此登记一行。
func init() {
	Register(sqliteDialect{})
	Register(postgresDialect{})
	Register(mysqlDialect{})
}
