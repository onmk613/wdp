package store

// 增量迁移线（v18 起，三库通用）。
//
// 写法约定（见 internal/store/dialect/rewrite.go 顶部）：
//   - 占位符 `?`、字符串拼接 `||`、聚合 `group_concat(x, ',')`
//   - 冲突忽略 `INSERT OR IGNORE`、Upsert `ON CONFLICT(c) DO UPDATE SET x = excluded.x`
//   - 加列不要写 ALTER TABLE：写成 `-- +wdp:add-column <表> <列> <定义>`，
//     由 prepareMigration 探测列是否存在后再决定是否执行（只有 PG 支持
//     ADD COLUMN IF NOT EXISTS，另两库必须先探测）。
//   - 列类型只用 INTEGER / TEXT / REAL 三种词（方言各自映射：MySQL 的
//     TEXT 不能带缺省值，故长文本列显式写 TEXT 并在需要缺省时改用 VARCHAR）。
//
// 新增迁移追加到本数组末尾，**不要改动已发布的条目**：存量库按版本号重放，
// 改动历史条目会让「升级库」与「新建库」schema 分叉。
//
// sqliteLegacyVersions 是历史 SQLite 迁移线的长度：SQLite 库的版本号从这条
// 线继续往下走；网络库新建时由基线一次性推到同一版本号，之后两条线合流。
const sqliteLegacyVersions = 17

var portableMigrations = []string{}
