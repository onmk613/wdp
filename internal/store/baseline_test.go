package store

import (
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestBaselineMatchesMigrations 基线必须与「跑完历史迁移的 schema」等价。
//
// 做法：把基线语句喂给一份新的 SQLite 库（SQLite 能执行三种基线的绝大部分
// 语法变体？不能——PG 的 BIGSERIAL/MySQL 的 AUTO_INCREMENT 它不认，因此这里
// 只对**结构信息**做对账：从两份 schema 里各自抽出「表 → 列集合」，逐表比对。
//
// 由于网络库基线由生成器从迁移结果导出，本测试的作用是防止日后「只改迁移、
// 忘了重新生成基线」——那是升级库与新库 schema 分叉的经典成因。
func TestBaselineMatchesMigrations(t *testing.T) {
	want := sqliteSchemaShape(t)

	for _, tc := range []struct {
		dialect string
		stmts   []string
	}{
		{"postgres", postgresBaseline},
		{"mysql", mysqlBaseline},
	} {
		got := parseBaselineShape(t, tc.dialect, tc.stmts)
		if len(got) != len(want) {
			t.Fatalf("%s 基线表数 %d，迁移产物 %d", tc.dialect, len(got), len(want))
		}
		var tables []string
		for tbl := range want {
			tables = append(tables, tbl)
		}
		sort.Strings(tables)
		for _, tbl := range tables {
			wantCols, gotCols := want[tbl], got[tbl]
			if gotCols == nil {
				t.Errorf("%s 基线缺表 %s", tc.dialect, tbl)
				continue
			}
			if len(gotCols) != len(wantCols) {
				t.Errorf("%s.%s 列数 %d，迁移产物 %d\n  基线: %v\n  迁移: %v",
					tc.dialect, tbl, len(gotCols), len(wantCols), gotCols, wantCols)
				continue
			}
			for i := range wantCols {
				if gotCols[i] != wantCols[i] {
					t.Errorf("%s.%s 第 %d 列 = %q，迁移产物 %q", tc.dialect, tbl, i, gotCols[i], wantCols[i])
				}
			}
		}
	}
}

// sqliteSchemaShape 返回迁移后 SQLite 库的「表 → 有序列名」。
func sqliteSchemaShape(t *testing.T) map[string][]string {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "shape.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	out := map[string][]string{}
	rows, err := s.raw.Query(`SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' AND name <> 'schema_version' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, n)
	}
	rows.Close()
	for _, tbl := range tables {
		cols, err := s.raw.Query(`PRAGMA table_info(` + tbl + `)`)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for cols.Next() {
			var cid, nn, pk int
			var name, typ string
			var dflt any
			if err := cols.Scan(&cid, &name, &typ, &nn, &dflt, &pk); err != nil {
				t.Fatal(err)
			}
			names = append(names, name)
		}
		cols.Close()
		out[tbl] = names
	}
	return out
}

// parseBaselineShape 从基线 DDL 文本里解析「表 → 有序列名」，忽略约束行。
func parseBaselineShape(t *testing.T, dialect string, stmts []string) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	for _, st := range stmts {
		up := strings.ToUpper(strings.TrimSpace(st))
		if !strings.HasPrefix(up, "CREATE TABLE") {
			continue
		}
		open := strings.IndexByte(st, '(')
		if open < 0 {
			t.Fatalf("%s 基线 DDL 形态异常: %s", dialect, st)
		}
		head := strings.Fields(st[:open])
		table := head[len(head)-1]
		body := st[open+1 : strings.LastIndexByte(st, ')')]
		var cols []string
		for _, line := range splitCommaTopLevel(body) {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			fl := strings.Fields(line)
			if len(fl) == 0 {
				continue
			}
			// 约束行首词可能被空格切开（`UNIQUE(user_id, app_key)` →
			// "UNIQUE(user_id,"），故按**行首前缀**判定而非整词比对。
			if isConstraintLine(line) {
				continue
			}
			cols = append(cols, strings.Trim(fl[0], "`\""))
		}
		out[table] = cols
	}
	return out
}

// splitCommaTopLevel 按顶层逗号切分 DDL 列定义（跳过括号内的逗号）。
func splitCommaTopLevel(s string) []string {
	var out []string
	var cur strings.Builder
	depth := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		// 先按括号调整深度再判逗号：`UNIQUE(a, b)` 里的逗号在 depth>0，
		// 不能被当作列定义分隔符（此前漏了「先增后判」，约束行被切碎）。
		if c == '(' {
			depth++
		}
		if c == ',' && depth == 0 {
			out = append(out, cur.String())
			cur.Reset()
			continue
		}
		if c == ')' && depth > 0 {
			depth--
		}
		cur.WriteByte(c)
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// isConstraintLine 报告该列定义行是否是表级约束（非普通列）。
func isConstraintLine(line string) bool {
	up := strings.ToUpper(strings.TrimSpace(line))
	for _, kw := range []string{"UNIQUE", "PRIMARY", "CHECK", "FOREIGN", "CONSTRAINT"} {
		if strings.HasPrefix(up, kw) {
			return true
		}
	}
	return false
}
