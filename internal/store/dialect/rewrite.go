package dialect

import (
	"regexp"
	"strconv"
	"strings"
)

// 规范 SQL 的识别模式。
//
// 约定（重要）：store 层的 SQL 一律按 **SQLite 写法**书写，即：
//
//	字符串拼接  a || b            —— MySQL 不支持 || 作拼接，由该方言单独重写
//	聚合        group_concat(x, ',')
//	冲突忽略    INSERT OR IGNORE INTO t (c) VALUES (...)
//	Upsert      ... ON CONFLICT(c) DO UPDATE SET x = excluded.x
//	自增回读    RETURNING id
//	占位符      ?
//
// 落库前由各方言的 Rewrite 翻译为本库写法。这样 350 处 SQL 只有一份可读文本，
// 方言差异集中在本包，且能用纯字符串断言测试（不需要真库）。
var (
	// group_concat(expr, sep)：MySQL 同名同序，只需改名到 PG 的 string_agg。
	reGroupConcatCall = regexp.MustCompile(`(?is)\bgroup_concat\s*\(`)

	// INSERT OR IGNORE INTO t (cols) VALUES (...)  [;]
	// cols 允许嵌套括号（列名里没有括号，但保守起见用 [^)]*）。
	reInsertIgnore = regexp.MustCompile(`(?is)\bINSERT\s+OR\s+IGNORE\s+INTO\s+([A-Za-z_][A-Za-z0-9_]*)\s*\(([^)]*)\)\s*VALUES\s*\(([^)]*)\)`)

	// ON CONFLICT(cols) DO UPDATE SET <赋值列表>（到语句尾或换行结束）
	reOnConflictUpdate = regexp.MustCompile(`(?is)\bON\s+CONFLICT\s*\(([^)]*)\)\s*DO\s+UPDATE\s+SET\s+(.+?)(?:;|$)`)
)

// rewriteConcatForMySQL 把 `a || b || c` 折成 `CONCAT(a, b, c)`。
// 三库中只有 MySQL 不支持 `||` 作字符串拼接（它是逻辑或）。
//
// 为什么不能用字符串替换：折叠链的边界取决于它在语句里的**作用域**——
//
//	VALUES (a || ?, b)        ← 逗号是列表分隔符，链只到 `a || ?`
//	CONCAT('x', ? || 'y')     ← 链只到 `? || 'y'`，不能吃掉前面的参数
//	SELECT a || b FROM t      ← 链到 b 为止，FROM 是关键字边界
//
// 因此按词法单元推导作用域：先算每个 token 处的括号深度，再在「深度不小于
// 当前、且未被更深括号包住」的范围内按逗号/最外层闭括号切段。
func rewriteConcatForMySQL(sql string) string {
	if !strings.Contains(sql, "||") {
		return sql
	}
	toks := tokenize(sql)
	depth := make([]int, len(toks))
	d := 0
	for i, tk := range toks {
		if tk.kind == tokClose && d > 0 {
			d--
		}
		depth[i] = d
		if tk.kind == tokOpen {
			d++
		}
	}

	var out strings.Builder
	last := 0
	for i := 0; i < len(toks); i++ {
		if toks[i].kind != tokConcat {
			continue
		}
		level := depth[i]

		// 链首：向左吸收同层 token，遇到关键字或出层即停；纯空白 token 跳过
		// （它由随后的裁剪去掉，不该终止链）。
		lo := i
		for lo > 0 {
			prev := toks[lo-1]
			if depth[lo-1] < level {
				break // 出了本层（包含本链的开括号）
			}
			if prev.kind == tokComma && depth[lo-1] == level {
				break // 同层列表分隔符：本链不能跨到上一个参数
			}
			if prev.kind == tokClose && depth[lo-1] >= level {
				break // 同层闭括号：链首不会跨过它
			}
			if prev.kind == tokAtom {
				if strings.TrimSpace(prev.text) == "" {
					lo--
					continue
				}
				if atomEndsChain(prev.text) {
					break
				}
			}
			lo--
		}
		// 跳过纯空白 token 后确定链首位置
		for lo < i && toks[lo].kind == tokAtom && strings.TrimSpace(toks[lo].text) == "" {
			lo++
		}
		loStart := toks[lo].start
		if toks[lo].kind == tokAtom {
			if cut, ok := cutAtomKeyword(sql[loStart:toks[lo].end]); ok {
				loStart += cut
				loStart = trimSpaceLeft(sql, loStart)
			}
		}

		// 链尾：向右吸收同层 token，遇到逗号/出层/关键字即停。
		hi := i
		endPos := toks[i].end
		for hi+1 < len(toks) {
			nx := toks[hi+1]
			if depth[hi+1] < level {
				break
			}
			if nx.kind == tokComma && depth[hi+1] == level {
				break
			}
			if nx.kind == tokAtom {
				if strings.TrimSpace(nx.text) == "" {
					hi++
					continue
				}
				if atomEndsChain(nx.text) {
					break
				}
			}
			hi++
			endPos = toks[hi].end
		}
		// 末段可能要按关键字截断：被吸收的原子可能把「操作数 + 后续语句」切成
		// 一个 token（如 ` id FROM hosts WHERE id = ?`）。取**最后一个含关键字
		// 的原子**（通常是词法器为占位符单列的那个 `?` 之前的元素），在关键字
		// 处截断并裁掉尾随空白。
		for k := hi; k >= i; k-- {
			if toks[k].kind != tokAtom {
				continue
			}
			cut, ok := cutAtomKeyword(sql[toks[k].start:toks[k].end])
			if !ok {
				continue
			}
			endPos = trimSpaceRight(sql, toks[k].start+cut)
			break
		}

		// 按 || 切操作数
		var parts []string
		segStart := loStart
		for k := lo; k <= hi; k++ {
			if toks[k].kind == tokConcat {
				parts = append(parts, strings.TrimSpace(sql[segStart:toks[k].start]))
				segStart = toks[k].end
			}
		}
		parts = append(parts, strings.TrimSpace(sql[segStart:endPos]))
		if len(parts) < 2 {
			continue
		}
		if loStart < last { // 与前一条链重叠：写入位置必须单调向前
			continue
		}

		out.WriteString(sql[last:loStart])
		out.WriteString("CONCAT(" + strings.Join(parts, ", ") + ")")
		last = endPos
		i = hi
	}
	out.WriteString(sql[last:])
	return out.String()
}

// atomEndsChain 报告原子文本是否终止拼接链（SELECT/FROM/VALUES 等关键字）。
// 多词原子（含空白的整段文本）只要不整体等于关键字就不终止——链的其它边界
// 由逗号、括号与深度决定。
func atomEndsChain(text string) bool {
	if strings.TrimSpace(text) == "" {
		return false // 纯空白不是边界，由调用方跳过
	}
	// 只有**开头**是关键字才终止链。原子中间的关键字（` id FROM hosts...`）
	// 是「操作数 + 后续语句」被词法器切在一起，由随后的 cutAtomKeyword 截断
	// 处理——若在这里就判为边界，操作数会被整段丢掉。
	up := strings.ToUpper(strings.TrimLeft(text, " \t\n\r"))
	for w := range chainStopWords {
		if !strings.HasPrefix(up, w) {
			continue
		}
		if len(up) == len(w) || isSpaceByte(up[len(w)]) {
			return true
		}
	}
	return false
}

// cutAtomKeyword 找出原子文本里最早出现的语句关键字，返回应保留的字节数。
//
// 词法器会把「关键字 + 空白 + 操作数」之前的整段文本切成一个原子，例如
// `... || id FROM hosts WHERE id = ?` 是一个原子——折叠拼接链时必须在此
// 截断，否则会把语句骨架折进 CONCAT()。
//
// 匹配要求词边界（关键字后是空白或串尾），避免把 `id` 这类标识符里的
// 子串误判成关键字（`id` 本身也在停用表里，靠边界判定区分）。
func cutAtomKeyword(text string) (int, bool) {
	up := strings.ToUpper(text)
	best := -1
	for w := range chainStopWords {
		for from := 0; ; {
			k := strings.Index(up[from:], w)
			if k < 0 {
				break
			}
			pos := from + k
			after := pos + len(w)
			if after >= len(up) || isSpaceByte(up[after]) {
				if best < 0 || pos < best {
					best = pos
				}
				break
			}
			from = pos + 1
		}
	}
	if best < 0 {
		return 0, false
	}
	if best == 0 {
		// 整段就是关键字（可能带尾随空白）：全部剔除
		return len(text), true
	}
	return best, true
}

func isSpaceByte(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

// chainStopWords 是遇到就停止吸收的关键字。
var chainStopWords = map[string]bool{
	"SELECT": true, "FROM": true, "WHERE": true, "AND": true, "OR": true, "NOT": true,
	"INSERT": true, "INTO": true, "VALUES": true, "UPDATE": true, "SET": true,
	"DELETE": true, "ON": true, "AS": true, "ORDER": true, "GROUP": true, "BY": true,
	"LIMIT": true, "OFFSET": true, "JOIN": true, "LEFT": true, "RIGHT": true,
	"INNER": true, "OUTER": true, "DUPLICATE": true, "KEY": true, "CONFLICT": true,
	"DO": true, "NOTHING": true, "RETURNING": true, "CASE": true, "WHEN": true,
	"THEN": true, "ELSE": true, "END": true, "IS": true, "NULL": true, "IN": true,
	"LIKE": true, "EXISTS": true, "UNION": true, "ALL": true, "DESC": true, "ASC": true,
}

// rewriteGroupConcat 把 group_concat 改写为指定函数名（PG 的 string_agg 与
// MySQL 的 group_concat 参数顺序一致，无需调整实参）。
//
// 只改普通片段：字面量里出现 group_concat 字样（审计明细里存 SQL 片段、
// 错误信息等）不得被改。
func rewriteGroupConcat(sql, fn string) string {
	return rewritePlain(sql, func(plain string) string {
		return reGroupConcatCall.ReplaceAllString(plain, fn+"(")
	})
}

// rewriteInsertIgnorePG 把 INSERT OR IGNORE 改写为 `INSERT ... ON CONFLICT DO NOTHING`。
func rewriteInsertIgnorePG(sql string) string {
	return rewritePlain(sql, func(plain string) string {
		return reInsertIgnore.ReplaceAllString(plain, "INSERT INTO $1 ($2) VALUES ($3) ON CONFLICT DO NOTHING")
	})
}

// rewriteInsertIgnoreMySQL 把 INSERT OR IGNORE 改写为 `INSERT IGNORE`。
func rewriteInsertIgnoreMySQL(sql string) string {
	return rewritePlain(sql, func(plain string) string {
		return reInsertIgnore.ReplaceAllString(plain, "INSERT IGNORE INTO $1 ($2) VALUES ($3)")
	})
}

// rewriteUpsertForMySQL 把 `ON CONFLICT(cols) DO UPDATE SET a = excluded.a`
// 改写为 MySQL 的 `ON DUPLICATE KEY UPDATE a = VALUES(a)`。
// VALUES(col) 取本语句将要插入的值，语义与 excluded 等价；该写法在
// MySQL 8.0.20+ 标记为 deprecated 但仍受支持（兼容 5.7 与 8.x）。
func rewriteUpsertForMySQL(sql string) string {
	return reOnConflictUpdate.ReplaceAllStringFunc(sql, func(m string) string {
		sub := reOnConflictUpdate.FindStringSubmatch(m)
		setExpr := strings.TrimSpace(sub[2])
		setExpr = strings.ReplaceAll(setExpr, "excluded.", "VALUES(")
		// 逐个赋值项补全 VALUES( 的右括号
		items := strings.Split(setExpr, ",")
		for i, it := range items {
			if strings.Contains(it, "VALUES(") && strings.Count(it, "(") > strings.Count(it, ")") {
				items[i] = strings.TrimRight(it, " \t\r\n") + ")"
			}
		}
		tail := ""
		if strings.HasSuffix(strings.TrimSpace(m), ";") {
			tail = ";"
		}
		return "ON DUPLICATE KEY UPDATE " + strings.Join(items, ",") + tail
	})
}

// replacePlaceholders 把 `?` 依次替换为 next 生成的占位符（跳过字面量与注释）。
func replacePlaceholders(sql string, next func(int) string) string {
	n := 0
	return rewriteRunes(sql, func(_ int, c byte) (string, int, bool) {
		if c != '?' {
			return "", 0, false
		}
		n++
		return next(n), 0, true
	})
}

// stripConcatToken 把拼接 token 还原为 SQL 标准算子 `||`（SQLite 与 PG 通用），
// 并规整两侧空白。token 是给「不支持 || 的方言」留的改写锚点——三个方言的
// 建表/查询文本都写 token，落库前各自处理：SQLite/PG 收敛为 ||，MySQL 折成
// CONCAT()。这样同一份 SQL 三库可读，且 MySQL 那条路不会把 || 误当逻辑或。
func stripConcatToken(sql string) string {
	if !strings.Contains(sql, TokenConcat) {
		return sql
	}
	parts := strings.Split(sql, TokenConcat)
	for i := range parts {
		switch i {
		case 0:
			parts[i] = strings.TrimRight(parts[i], " \t\r\n")
		case len(parts) - 1:
			parts[i] = strings.TrimLeft(parts[i], " \t\r\n")
		default:
			parts[i] = strings.TrimSpace(parts[i])
		}
	}
	return strings.Join(parts, " || ")
}

// concatJoinAll 是 stripConcatToken 的别名（语义相同，调用点按可读性选用）。
func concatJoinAll(sql string) string { return stripConcatToken(sql) }

// dollarPlaceholder 生成 PG 风格占位符 $n。
func dollarPlaceholder(n int) string { return "$" + strconv.Itoa(n) }

// splitTopLevel 按不在字面量/注释内的分隔符切分文本，分隔符附在前一段尾部。
func splitTopLevel(text string, sep byte) []string {
	var out []string
	var cur strings.Builder
	for _, ev := range scanSQL(text) {
		if ev.kind == "literal" {
			cur.WriteString(ev.text)
			continue
		}
		for i := 0; i < len(ev.text); i++ {
			cur.WriteByte(ev.text[i])
			if ev.text[i] == sep {
				out = append(out, cur.String())
				cur.Reset()
			}
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// SplitStatements 按顶层分号切分 SQL 文本，返回非空的单条语句。
//
// 为什么要它：PostgreSQL 的驱动不接受一次 Exec 多条语句，而 SQLite/MySQL 可以，
// 因此迁移与 DDL 一律先切分再逐条执行（三库同一路径，不给某一库留特例）。
// 切分跳过字符串字面量、注释与 PG 美元引用里的分号。
func SplitStatements(text string) []string {
	var out []string
	for _, part := range splitTopLevel(text, ';') {
		if strings.TrimSpace(part) != "" {
			out = append(out, strings.TrimSpace(part))
		}
	}
	return out
}
