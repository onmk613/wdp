// Package shellquote 提供嵌入 sh 脚本的字符串安全转义与参数切分。
//
// 这是全项目唯一的 shell 注入防线：所有拼装远端脚本的位置必须经由
// Quote/Split，禁止在各自包内复制实现——转义逻辑若有修正只需落在此处即可全局生效。
package shellquote

import (
	"errors"
	"strings"
)

// Quote 把任意字符串转为安全的单引号字面量。
// 内部的 ' 以 '\”（结束引号 + 转义引号 + 重开引号）拼接，sh 解析后语义不变。
func Quote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ErrUnterminated 表示 Split 遇到未闭合的引号。
var ErrUnterminated = errors.New("shellquote: 引号未闭合")

// QuoteWords 把 free-form 参数串按 POSIX sh 词法切词后逐词 Quote，
// 再以空格连接——用于把用户书写的命令行参数安全嵌入 sh 命令：
// 元字符（$ ; | && 反引号等）一律字面传递，不再被远端 shell 解释。
// 切词失败（引号未闭合）返回错误，调用方应 fail-loud。
func QuoteWords(s string) (string, error) {
	fields, err := Split(s)
	if err != nil {
		return "", err
	}
	quoted := make([]string, len(fields))
	for i, f := range fields {
		quoted[i] = Quote(f)
	}
	return strings.Join(quoted, " "), nil
}

// Split 按 POSIX sh 词法把字符串切分为词：空格/制表符分隔，
// 单引号内全字面、双引号内允许 \ 转义（仅 " \ $ ` 换行）、
// 引号外反斜杠转义下一字符。与 shlex/POSIX 的常用子集对齐。
func Split(s string) ([]string, error) {
	var out []string
	var cur strings.Builder
	inWord := false
	for i := 0; i < len(s); {
		c := s[i]
		switch c {
		case ' ', '\t', '\n':
			if inWord {
				out = append(out, cur.String())
				cur.Reset()
				inWord = false
			}
			i++
		case '\'': // 单引号：内部全字面
			j := strings.IndexByte(s[i+1:], '\'')
			if j < 0 {
				return nil, ErrUnterminated
			}
			cur.WriteString(s[i+1 : i+1+j])
			inWord = true
			i += j + 2
		case '"': // 双引号：允许 \ 转义部分字符
			i++
			closed := false
			for i < len(s) {
				if s[i] == '\\' && i+1 < len(s) {
					switch s[i+1] {
					case '"', '\\', '$', '`', '\n':
						cur.WriteByte(s[i+1])
						i += 2
						continue
					default: // 其他 \x 保持字面两个字符
						cur.WriteByte('\\')
						cur.WriteByte(s[i+1])
						i += 2
						continue
					}
				}
				if s[i] == '"' {
					closed = true
					i++
					break
				}
				cur.WriteByte(s[i])
				i++
			}
			if !closed {
				return nil, ErrUnterminated
			}
			inWord = true
		case '\\': // 引号外：转义下一字符
			if i+1 >= len(s) {
				return nil, errors.New("shellquote: 行尾孤立反斜杠")
			}
			cur.WriteByte(s[i+1])
			inWord = true
			i += 2
		default:
			cur.WriteByte(c)
			inWord = true
			i++
		}
	}
	if inWord {
		out = append(out, cur.String())
	}
	return out, nil
}
