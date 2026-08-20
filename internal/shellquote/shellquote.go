// Package shellquote 提供嵌入 sh 脚本的字符串安全转义。
//
// 这是全项目唯一的 shell 注入防线：所有拼装远端脚本的位置必须经由
// Quote，禁止在各自包内复制实现——转义逻辑若有修正只需落在此处即可全局生效。
package shellquote

import "strings"

// Quote 把任意字符串转为安全的单引号字面量。
// 内部的 ' 以 '\”（结束引号 + 转义引号 + 重开引号）拼接，sh 解析后语义不变。
func Quote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
