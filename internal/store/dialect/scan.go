package dialect

import "strings"

// SQL 扫描：所有方言改写（占位符、token、聚合、标识符）都必须**跳过字符串
// 字面量、注释与标识符引用**，否则 SQL 里出现的 `?`、`--`、`||` 会被误改。
// 这里提供一份统一的扫描器，三个方言共用，避免各写一份漏掉某种引号形式。
//
// 覆盖的语法（按三库并集）：
//   - 单引号串 ''：SQL 标准，`''` 是转义的单引号；MySQL 另外支持反斜杠转义
//   - 双引号串 ""：SQL 标准标识符引用（SQLite/PG），MySQL 缺省是字符串
//   - 反引号串 ``：MySQL/SQLite 标识符引用
//   - 行注释 -- 与 #
//   - 块注释 /* */（MySQL 要求其后跟空格或 ! 才算注释，这里按通用处理）
//   - PG 的 $tag$ ... $tag$ 美元引用串

// scanEvent 是扫描过程中回传给改写函数的事件。
type scanEvent struct {
	// kind 取值：plain（普通 SQL 文本）、literal（字面量/注释/引用标识符整体）。
	kind string
	text string
}

// scanSQL 把 SQL 拆成「普通文本」与「不可改写片段」两类事件序列。
func scanSQL(sql string) []scanEvent {
	var out []scanEvent
	var plain strings.Builder

	flush := func() {
		if plain.Len() > 0 {
			out = append(out, scanEvent{kind: "plain", text: plain.String()})
			plain.Reset()
		}
	}

	i, n := 0, len(sql)
	for i < n {
		c := sql[i]

		// PG 美元引用：$tag$ ... $tag$（tag 可空）。必须在普通字符之前判断。
		if c == '$' {
			if end, ok := dollarQuotedEnd(sql, i); ok {
				flush()
				out = append(out, scanEvent{kind: "literal", text: sql[i:end]})
				i = end
				continue
			}
		}

		// 字符串/标识符引用串
		if c == '\'' || c == '"' || c == '`' {
			end := quotedEnd(sql, i, c)
			flush()
			out = append(out, scanEvent{kind: "literal", text: sql[i:end]})
			i = end
			continue
		}

		// 行注释
		if c == '-' && i+1 < n && sql[i+1] == '-' {
			end := lineCommentEnd(sql, i)
			flush()
			out = append(out, scanEvent{kind: "literal", text: sql[i:end]})
			i = end
			continue
		}
		if c == '#' {
			end := lineCommentEnd(sql, i)
			flush()
			out = append(out, scanEvent{kind: "literal", text: sql[i:end]})
			i = end
			continue
		}

		// 块注释
		if c == '/' && i+1 < n && sql[i+1] == '*' {
			end := strings.Index(sql[i+2:], "*/")
			if end < 0 {
				end = n
			} else {
				end = i + 2 + end + 2
			}
			flush()
			out = append(out, scanEvent{kind: "literal", text: sql[i:end]})
			i = end
			continue
		}

		plain.WriteByte(c)
		i++
	}
	flush()
	return out
}

// quotedEnd 返回从 i 处的引号开始、到该引用串结束（含结尾引号）的下标。
// 处理 `”` 形式的转义（SQL 标准）与 MySQL 的反斜杠转义。
func quotedEnd(sql string, i int, quote byte) int {
	n := len(sql)
	j := i + 1
	for j < n {
		switch sql[j] {
		case '\\':
			// MySQL 反斜杠转义：跳过下一个字符。对 SQLite/PG 这是多余的保护
			// （它们不支持反斜杠转义），最坏情况只是把后续字符多算进串内，
			// 而串内本来就不改写。
			j += 2
			continue
		case quote:
			// 连续两个引号是转义，串未结束
			if j+1 < n && sql[j+1] == quote {
				j += 2
				continue
			}
			return j + 1
		}
		j++
	}
	return n
}

// lineCommentEnd 返回行注释结束下标（不含换行符本身，换行归还普通文本）。
func lineCommentEnd(sql string, i int) int {
	if k := strings.IndexByte(sql[i:], '\n'); k >= 0 {
		return i + k
	}
	return len(sql)
}

// dollarQuotedEnd 识别 PG 的 $tag$ ... $tag$。i 处为 '$'，返回结束下标。
// tag 可为空（即 `$$`），此时紧邻的两个 '$' 就是开标签——漏掉这一支会把
// `$$ raw $$` 当成普通文本，里面的 `?` 随后被误当占位符改写。
func dollarQuotedEnd(sql string, i int) (int, bool) {
	n := len(sql)
	j := i + 1
	if j < n && sql[j] == '$' {
		j++ // 空 tag：$$ 即为开标签
	} else {
		for j < n && sql[j] != '$' && sql[j] != ' ' && sql[j] != '\t' && sql[j] != '\n' {
			c := sql[j]
			ok := c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
				(j > i+1 && c >= '0' && c <= '9')
			if !ok {
				return 0, false
			}
			j++
		}
		if j >= n || sql[j] != '$' {
			return 0, false // 没有闭合的 '$'，不是美元引用
		}
		j++ // 吃掉闭合的 '$'
	}
	tag := sql[i:j] // 开标签含两端 '$'
	k := strings.Index(sql[j:], tag)
	if k < 0 {
		return 0, false // 未闭合：按普通文本处理，避免吞掉后续语句
	}
	return j + k + len(tag), true
}

// rewritePlain 按事件序列重建 SQL：plain 片段交给 fn 处理，literal 原样保留。
func rewritePlain(sql string, fn func(string) string) string {
	var out strings.Builder
	for _, ev := range scanSQL(sql) {
		if ev.kind == "plain" {
			out.WriteString(fn(ev.text))
			continue
		}
		out.WriteString(ev.text)
	}
	return out.String()
}

// rewriteRunes 逐字符处理普通片段（占位符转换与 token 替换共用）。
func rewriteRunes(sql string, fn func(i int, c byte) (replacement string, skip int, handled bool)) string {
	return rewritePlain(sql, func(plain string) string {
		var out strings.Builder
		for i := 0; i < len(plain); {
			if rep, skip, ok := fn(i, plain[i]); ok {
				out.WriteString(rep)
				i += 1 + skip
				continue
			}
			out.WriteByte(plain[i])
			i++
		}
		return out.String()
	})
}
