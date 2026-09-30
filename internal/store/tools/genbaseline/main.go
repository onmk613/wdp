// 命令 genbaseline 从「跑完全部迁移的 SQLite schema」导出 PostgreSQL / MySQL 建表基线。
//
// 用法：
//
//	go run ./internal/store/tools/genbaseline            # 只生成 .sql 到临时目录
//	go run ./internal/store/tools/genbaseline -o dir     # 指定输出目录
//	go run ./internal/store/tools/genbaseline -write     # 同时改写 internal/store/baseline.go
//
// 为什么用生成而不是手抄：基线要与「历史迁移跑完后的 schema」严格一致，
// 差一列都会让新建的 MySQL/PG 库与升级来的 SQLite 库行为分叉——那是最难查的
// 一类偏差。生成 + internal/store/baseline_test.go 的对账测试把这件事钉死。
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"wdp/internal/store"
)

func main() {
	out := flag.String("o", "", "输出目录（缺省：系统临时目录下 wdp-baseline）")
	write := flag.Bool("write", false, "同时改写 internal/store/baseline.go")
	flag.Parse()

	dir := *out
	if dir == "" {
		d, err := os.MkdirTemp("", "wdp-baseline")
		must(err)
		dir = d
	}
	must(os.MkdirAll(dir, 0o755))

	// 打开一个临时库并跑完迁移：schema 的「真值」只能从实际迁移结果拿。
	tmp, err := os.MkdirTemp("", "wdp-gen")
	must(err)
	defer os.RemoveAll(tmp)

	s, err := store.Open(filepath.Join(tmp, "gen.db"))
	must(err)
	defer s.Close()

	tables := sqliteTables(s)
	stmts := map[string][]string{}
	for _, dialect := range []string{"postgres", "mysql"} {
		var outStmts []string
		for _, tbl := range tables {
			ddl, err := genTable(s, dialect, tbl)
			must(err)
			outStmts = append(outStmts, ddl)
		}
		idx, err := genIndexes(s)
		must(err)
		outStmts = append(outStmts, idx...)
		stmts[dialect] = outStmts

		path := filepath.Join(dir, dialect+".sql")
		must(os.WriteFile(path, []byte(strings.Join(outStmts, ";\n")+"\n"), 0o644))
		fmt.Printf("%s: %d 条语句 → %s\n", dialect, len(outStmts), path)
	}

	if *write {
		must(rewriteBaselineGo(stmts))
		fmt.Println("已改写 internal/store/baseline.go")
	}
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "genbaseline:", err)
		os.Exit(1)
	}
}

// sqliteTables 返回除 schema_version 外的全部表名（有序）。
func sqliteTables(s *store.Store) []string {
	rows, err := s.DB().Query(`SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' AND name <> 'schema_version' ORDER BY name`)
	must(err)
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		must(rows.Scan(&n))
		names = append(names, n)
	}
	must(rows.Err())
	sort.Strings(names)
	return names
}

type column struct {
	name, typ, dflt string
	notnull, pk     bool
}

// genTable 生成单表的建表语句（列来自 PRAGMA table_info，唯一约束来自原始 DDL）。
func genTable(s *store.Store, dialect, tbl string) (string, error) {
	rows, err := s.DB().Query(`PRAGMA table_info(` + tbl + `)`)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var cols []column
	for rows.Next() {
		var cid, nn, pk int
		var name, typ string
		var dflt any
		if err := rows.Scan(&cid, &name, &typ, &nn, &dflt, &pk); err != nil {
			return "", err
		}
		d := ""
		if dflt != nil {
			d = fmt.Sprint(dflt)
		}
		cols = append(cols, column{name, typ, d, nn == 1, pk == 1})
	}
	if err := rows.Err(); err != nil {
		return "", err
	}

	var ddl string
	if err := s.DB().QueryRow(`SELECT sql FROM sqlite_master WHERE type='table' AND name=?`, tbl).Scan(&ddl); err != nil {
		return "", err
	}
	autoinc := strings.Contains(ddl, "AUTOINCREMENT")
	uniques := extractUniques(ddl)

	var b strings.Builder
	fmt.Fprintf(&b, "CREATE TABLE %s (\n", tbl)
	lines := make([]string, 0, len(cols)+len(uniques))
	for _, c := range cols {
		lines = append(lines, "  "+colLine(dialect, c, autoinc, textInUnique(ddl, c.name)))
	}
	for _, u := range uniques {
		lines = append(lines, "  "+u)
	}
	b.WriteString(strings.Join(lines, ",\n"))
	b.WriteString("\n)")
	return b.String(), nil
}

func colLine(dialect string, c column, autoinc, inUnique bool) string {
	ident := quoteIdent(dialect, c.name)
	if c.pk && autoinc {
		switch dialect {
		case "postgres":
			return ident + " BIGSERIAL PRIMARY KEY"
		case "mysql":
			return ident + " BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY"
		}
	}
	out := ident + " " + mapType(dialect, c.typ, c.dflt, inUnique)
	if c.notnull || c.pk {
		out += " NOT NULL"
	}
	if c.dflt != "" && !strings.EqualFold(c.dflt, "null") {
		out += " DEFAULT " + c.dflt
	}
	if c.pk {
		out += " PRIMARY KEY"
	}
	return out
}

// mapType 把 SQLite 的宽松类型名映射到目标库。
//
// MySQL 的两条硬约束决定了这里的特例：TEXT 不能带缺省值，也不能直接进唯一
// 索引（需前缀长度）。因此「带缺省」或「出现在唯一约束里」的文本列一律用
// VARCHAR(255)——wdp 的这类列都是名字/键/短标签，255 足够。
func mapType(dialect, typ, dflt string, inUnique bool) string {
	up := strings.ToUpper(typ)
	switch {
	case strings.Contains(up, "INT"):
		return "BIGINT"
	case strings.Contains(up, "REAL"), strings.Contains(up, "DOUBLE"), strings.Contains(up, "FLOAT"):
		if dialect == "postgres" {
			return "DOUBLE PRECISION"
		}
		return "DOUBLE"
	default:
		if dialect == "mysql" && (dflt != "" || inUnique) {
			return "VARCHAR(255)"
		}
		return "TEXT"
	}
}

// extractUniques 从 SQLite 原始 DDL 里取出表级约束（唯一与复合主键）。
func extractUniques(ddl string) []string {
	var out []string
	up := strings.ToUpper(ddl)
	for _, kw := range []string{"UNIQUE(", "PRIMARY KEY("} {
		for i := 0; ; {
			k := strings.Index(up[i:], kw)
			if k < 0 {
				break
			}
			k += i
			end := strings.IndexByte(ddl[k:], ')')
			if end < 0 {
				break
			}
			inner := ddl[k+len(kw) : k+end]
			if kw == "UNIQUE(" {
				out = append(out, "UNIQUE("+inner+")")
			} else {
				out = append(out, "PRIMARY KEY("+inner+")")
			}
			i = k + end
		}
	}
	return out
}

// textInUnique 报告该列是否出现在唯一约束/复合主键里。
func textInUnique(ddl, col string) bool {
	up := strings.ToUpper(ddl)
	for _, kw := range []string{"UNIQUE(", "PRIMARY KEY("} {
		for i := 0; ; {
			k := strings.Index(up[i:], kw)
			if k < 0 {
				break
			}
			k += i
			end := strings.IndexByte(ddl[k:], ')')
			if end < 0 {
				break
			}
			for _, c := range strings.Split(ddl[k+len(kw):k+end], ",") {
				if strings.EqualFold(strings.TrimSpace(c), col) {
					return true
				}
			}
			i = k + end
		}
	}
	return false
}

// mysqlReserved 是 MySQL 的保留字列名（需反引号引用；SQLite/PG 下裸写即可）。
var mysqlReserved = map[string]bool{
	"user": true, "key": true, "group": true, "order": true, "status": true,
	"level": true, "value": true, "system": true, "rank": true, "usage": true,
}

func quoteIdent(dialect, name string) string {
	if dialect == "mysql" && mysqlReserved[strings.ToLower(name)] {
		return "`" + name + "`"
	}
	return name
}

// genIndexes 取出索引 DDL（三库语法在 wdp 用到的形态上一致）。
func genIndexes(s *store.Store) ([]string, error) {
	rows, err := s.DB().Query(`SELECT sql FROM sqlite_master WHERE type='index' AND sql IS NOT NULL ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var sql string
		if err := rows.Scan(&sql); err != nil {
			return nil, err
		}
		out = append(out, sql)
	}
	return out, rows.Err()
}
