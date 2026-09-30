package dialect

import (
	"strings"
	"unicode"
)

// 词法单元（token）与括号深度：MySQL 要把 `a || b` 折成 CONCAT(a, b)，而
// 折叠边界必须落在「同一括号深度、且不是函数参数逗号」的位置上——纯字符串
// 替换做不到这件事（`CONCAT('x', ?, 'y')` 里的逗号是参数分隔符，不是 SQL
// 列表分隔符）。因此这里给出一个轻量词法器，只识别折叠所需的信息：
// 字面量整体、括号、逗号、`||`、以及其余「原子」文本。
//
// 不做完整 SQL 解析：只保证「不会把字面量或注释里的字符当成语法」。

type tokKind int

const (
	tokAtom    tokKind = iota // 普通文本（含函数名、列名、运算符）
	tokLiteral                // 字符串/标识符引用、注释、美元引用串：不可改写
	tokComma                  // 顶层参数分隔符
	tokOpen                   // (
	tokClose                  // )
	tokConcat                 // ||
)

type token struct {
	kind  tokKind
	text  string
	start int
	end   int
}

// tokenize 把 SQL 切成词法单元（字面量与注释整体成 token）。
func tokenize(sql string) []token {
	var out []token
	i, n := 0, len(sql)
	for i < n {
		start := i
		c := sql[i]

		// 不可改写片段整体成 token
		if c == '$' {
			if end, ok := dollarQuotedEnd(sql, i); ok {
				out = append(out, token{tokLiteral, sql[i:end], i, end})
				i = end
				continue
			}
		}
		if c == '\'' || c == '"' || c == '`' {
			end := quotedEnd(sql, i, c)
			out = append(out, token{tokLiteral, sql[i:end], i, end})
			i = end
			continue
		}
		if c == '-' && i+1 < n && sql[i+1] == '-' {
			end := lineCommentEnd(sql, i)
			out = append(out, token{tokLiteral, sql[i:end], i, end})
			i = end
			continue
		}
		if c == '#' {
			end := lineCommentEnd(sql, i)
			out = append(out, token{tokLiteral, sql[i:end], i, end})
			i = end
			continue
		}
		if c == '/' && i+1 < n && sql[i+1] == '*' {
			end := i + 2
			if k := strings.Index(sql[end:], "*/"); k >= 0 {
				end += k + 2
			} else {
				end = n
			}
			out = append(out, token{tokLiteral, sql[i:end], i, end})
			i = end
			continue
		}

		// 结构性字符
		switch c {
		case '(':
			out = append(out, token{tokOpen, "(", i, i + 1})
			i++
			continue
		case ')':
			out = append(out, token{tokClose, ")", i, i + 1})
			i++
			continue
		case ',':
			out = append(out, token{tokComma, ",", i, i + 1})
			i++
			continue
		case '|':
			if i+1 < n && sql[i+1] == '|' {
				out = append(out, token{tokConcat, "||", i, i + 2})
				i += 2
				continue
			}
		}

		// 原子文本：累积到下一个结构性字符或不可改写片段的起点。
		// 注意终止集合必须与上面的「整 token 特判」一致，否则某个字符会
		// 既不被当作结构字符、又让原子扫描空转（曾因 `?` 不在集合里导致
		// 该字符后的整段文本被留在一个空原子之外）。
		for i < n {
			ch := sql[i]
			if ch == '(' || ch == ')' || ch == ',' || ch == '\'' || ch == '"' || ch == '`' || ch == '#' {
				break
			}
			if ch == '?' { // 占位符：与参数绑定位置一一对应，单独成 token
				break
			}
			if ch == '|' && i+1 < n && sql[i+1] == '|' {
				break
			}
			if ch == '-' && i+1 < n && sql[i+1] == '-' {
				break
			}
			if ch == '/' && i+1 < n && sql[i+1] == '*' {
				break
			}
			if ch == '$' && isDollarQuoteStart(sql, i) {
				break
			}
			i++
		}
		if i == start { // 单字符 token（如 `?`）：前进一步避免死循环
			i++
		}
		out = append(out, token{tokAtom, sql[start:i], start, i})
	}
	return out
}

// isDollarQuoteStart 报告 sql[i] 处是否是一个美元引用串的起点（`$$` 或
// `$tag$`）。普通 `$`（如标识符里的美元符号）不该终止原子扫描。
func isDollarQuoteStart(sql string, i int) bool {
	_, ok := dollarQuotedEnd(sql, i)
	return ok
}

// isWordRune 报告字符是否属于 SQL 标识符/关键字字符。
func isWordRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '$'
}

// trimSpaceLeft/Right 返回去掉空白后的下标（用于把 token 边界对齐到可见文本）。
func trimSpaceLeft(s string, i int) int {
	for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == '\n' || s[i] == '\r') {
		i++
	}
	return i
}

func trimSpaceRight(s string, i int) int {
	for i > 0 && (s[i-1] == ' ' || s[i-1] == '\t' || s[i-1] == '\n' || s[i-1] == '\r') {
		i--
	}
	return i
}
