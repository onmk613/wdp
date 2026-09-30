package dialect

import (
	"strings"
	"testing"
)

// TestRewriteMatchesEveryDialect 钉住「同一份规范 SQL，三个方言各自译对」。
// 这是本包的核心契约：store 层只写一份 SQLite 写法的 SQL，翻译正确性全靠这里。
func TestRewriteMatchesEveryDialect(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want map[string]string // dialect → 期望输出
	}{
		{
			name: "占位符",
			in:   `SELECT id, name FROM hosts WHERE pool = ? AND status = ? LIMIT ?`,
			want: map[string]string{
				"sqlite":   `SELECT id, name FROM hosts WHERE pool = ? AND status = ? LIMIT ?`,
				"postgres": `SELECT id, name FROM hosts WHERE pool = $1 AND status = $2 LIMIT $3`,
				"mysql":    `SELECT id, name FROM hosts WHERE pool = ? AND status = ? LIMIT ?`,
			},
		},
		{
			name: "字面量里的问号不被当占位符",
			in:   `SELECT * FROM t WHERE note = 'why? because' AND x = ?`,
			want: map[string]string{
				"sqlite":   `SELECT * FROM t WHERE note = 'why? because' AND x = ?`,
				"postgres": `SELECT * FROM t WHERE note = 'why? because' AND x = $1`,
				"mysql":    `SELECT * FROM t WHERE note = 'why? because' AND x = ?`,
			},
		},
		{
			name: "注释里的问号不被当占位符",
			in:   "SELECT a FROM t -- 这里?是注释\nWHERE x = ?",
			want: map[string]string{
				"sqlite":   "SELECT a FROM t -- 这里?是注释\nWHERE x = ?",
				"postgres": "SELECT a FROM t -- 这里?是注释\nWHERE x = $1",
				"mysql":    "SELECT a FROM t -- 这里?是注释\nWHERE x = ?",
			},
		},
		{
			name: "聚合函数",
			in:   `SELECT group_concat(pool, ',') FROM host_pools WHERE host_id = ?`,
			want: map[string]string{
				"sqlite":   `SELECT group_concat(pool, ',') FROM host_pools WHERE host_id = ?`,
				"postgres": `SELECT string_agg(pool, ',') FROM host_pools WHERE host_id = $1`,
				"mysql":    `SELECT group_concat(pool, ',') FROM host_pools WHERE host_id = ?`,
			},
		},
		{
			name: "冲突忽略",
			in:   `INSERT OR IGNORE INTO host_pools (host_id, pool) VALUES (?, ?)`,
			want: map[string]string{
				"sqlite":   `INSERT OR IGNORE INTO host_pools (host_id, pool) VALUES (?, ?)`,
				"postgres": `INSERT INTO host_pools (host_id, pool) VALUES ($1, $2) ON CONFLICT DO NOTHING`,
				"mysql":    `INSERT IGNORE INTO host_pools (host_id, pool) VALUES (?, ?)`,
			},
		},
		{
			name: "upsert",
			in:   `INSERT INTO settings (id, data) VALUES (?, ?) ON CONFLICT(id) DO UPDATE SET data = excluded.data`,
			want: map[string]string{
				"sqlite":   `INSERT INTO settings (id, data) VALUES (?, ?) ON CONFLICT(id) DO UPDATE SET data = excluded.data`,
				"postgres": `INSERT INTO settings (id, data) VALUES ($1, $2) ON CONFLICT(id) DO UPDATE SET data = excluded.data`,
				"mysql":    `INSERT INTO settings (id, data) VALUES (?, ?) ON DUPLICATE KEY UPDATE data = VALUES(data)`,
			},
		},
		{
			name: "字符串拼接",
			in:   `SELECT 'a' || name || '-' || id FROM hosts WHERE id = ?`,
			want: map[string]string{
				"sqlite":   `SELECT 'a' || name || '-' || id FROM hosts WHERE id = ?`,
				"postgres": `SELECT 'a' || name || '-' || id FROM hosts WHERE id = $1`,
				"mysql":    `SELECT CONCAT('a', name, '-', id) FROM hosts WHERE id = ?`,
			},
		},
	}

	for _, d := range dialectsOf(t) {
		for _, tc := range cases {
			want, ok := tc.want[d.Name()]
			if !ok {
				t.Fatalf("用例 %q 缺少方言 %s 的期望值", tc.name, d.Name())
			}
			got := d.Rewrite(tc.in)
			if got != want {
				t.Errorf("[%s] %s\n  in   = %s\n  got  = %s\n  want = %s", d.Name(), tc.name, tc.in, got, want)
			}
		}
	}
}

// TestMySQLConcatChain MySQL 的 || 必须折成单个 CONCAT(...)，且不能把多参数
// 之间的逗号误当拼接链的一部分。
func TestMySQLConcatChain(t *testing.T) {
	in := `INSERT INTO t (a, b) VALUES ('x' || ? || 'y', 'z' || ?)`
	got := mysqlDialect{}.Rewrite(in)
	want := `INSERT INTO t (a, b) VALUES (CONCAT('x', ?, 'y'), CONCAT('z', ?))`
	if got != want {
		t.Errorf("\n got  = %s\n want = %s", got, want)
	}
}

// TestRewriteKeepsLiteralsIntact 确保改写不会动字符串字面量里的构造。
func TestRewriteKeepsLiteralsIntact(t *testing.T) {
	in := `SELECT * FROM t WHERE note = 'group_concat(a,b) || x' AND a = ?`
	got := postgresDialect{}.Rewrite(in)
	want := `SELECT * FROM t WHERE note = 'group_concat(a,b) || x' AND a = $1`
	if got != want {
		t.Errorf("\n got  = %s\n want = %s", got, want)
	}
}

// TestScanSQLHandlesQuoting 扫描器要认得三种引号、行注释、块注释与 PG 美元引用。
func TestScanSQLHandlesQuoting(t *testing.T) {
	cases := []struct {
		name       string
		sql        string
		literals   []string // 期望被识别为不可改写的片段
		placeholds int      // 期望识别出的占位符个数（postgres 改写后计 $n）
	}{
		{"单引号", `a = 'x ? y' AND b = ?`, []string{`'x ? y'`}, 1},
		{"双引号", `a = "col?name" AND b = ?`, []string{`"col?name"`}, 1},
		{"反引号", "a = `col?name` AND b = ?", []string{"`col?name`"}, 1},
		{"块注释", `a = 1 /* ? ? */ AND b = ?`, []string{`/* ? ? */`}, 1},
		{"行注释", "a = 1 -- ?\n AND b = ?", []string{"-- ?"}, 1},
		{"美元引用", `a = $$ raw ? text $$ AND b = ?`, []string{`$$ raw ? text $$`}, 1},
		{"美元引用带标签", `a = $tag$ ? $tag$ AND b = ?`, []string{`$tag$ ? $tag$`}, 1},
		{"转义单引号", `a = 'it''s ?' AND b = ?`, []string{`'it''s ?'`}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var lits []string
			for _, ev := range scanSQL(tc.sql) {
				if ev.kind == "literal" {
					lits = append(lits, ev.text)
				}
			}
			for _, want := range tc.literals {
				found := false
				for _, got := range lits {
					if got == want {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("未识别为不可改写片段: %q（识别到 %q）", want, lits)
				}
			}
			rewritten := postgresDialect{}.Rewrite(tc.sql)
			// 只数「普通片段」里的 $n（美元引用串自身含 $，不能算占位符）
			plainDollars := 0
			for _, ev := range scanSQL(rewritten) {
				if ev.kind == "plain" {
					plainDollars += strings.Count(ev.text, "$")
				}
			}
			if plainDollars != tc.placeholds {
				t.Errorf("占位符个数 = %d, want %d（改写后 %q）", plainDollars, tc.placeholds, rewritten)
			}
		})
	}
}

// TestDialectConstructs 三个方言的构造器（自增主键/聚合/冲突/加列）都必须给出答案。
func TestDialectConstructs(t *testing.T) {
	for _, d := range dialectsOf(t) {
		if d.AutoIncPK() == "" || d.Text() == "" || d.Real() == "" {
			t.Errorf("[%s] 建表类型构造器返回空", d.Name())
		}
		if s := d.GroupConcat("x", ","); !strings.Contains(s, "x") {
			t.Errorf("[%s] GroupConcat 未包含表达式: %s", d.Name(), s)
		}
		if s := d.InsertIgnore("t", []string{"a", "b"}); !strings.Contains(s, "t") {
			t.Errorf("[%s] InsertIgnore 未包含表名: %s", d.Name(), s)
		}
		if s := d.Upsert("t", []string{"a"}, []string{"a"}, []string{"b = excluded.b"}); !strings.Contains(s, "t") {
			t.Errorf("[%s] Upsert 未包含表名: %s", d.Name(), s)
		}
		if s := d.AddColumn("t", "c", "TEXT"); !strings.Contains(s, "ADD COLUMN") {
			t.Errorf("[%s] AddColumn 形态异常: %s", d.Name(), s)
		}
	}
}

// TestEveryDialectRegisters 三个方言都在册（漏注册会让 ParseDSN 在运行期才报错）。
func TestEveryDialectRegisters(t *testing.T) {
	for _, name := range []string{"sqlite", "postgres", "mysql"} {
		if _, err := Get(name); err != nil {
			t.Errorf("方言 %s 未注册: %v", name, err)
		}
	}
	if _, err := Get("oracle"); err == nil {
		t.Error("未支持的方言应报错")
	}
}

// dialectsOf 返回全部方言（保持稳定顺序，便于失败信息对照）。
func dialectsOf(t *testing.T) []Dialect {
	t.Helper()
	out := make([]Dialect, 0, 3)
	for _, name := range []string{"sqlite", "postgres", "mysql"} {
		d, err := Get(name)
		if err != nil {
			t.Fatalf("方言 %s 未注册: %v", name, err)
		}
		out = append(out, d)
	}
	return out
}
