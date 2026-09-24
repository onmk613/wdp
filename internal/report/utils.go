package report

import (
	"strings"

	"wdp/internal/fmtutil"
)

// firstLine 取首行（去首尾空白），主机清单的错误摘要用。
func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s
}

// truncateOut 截断长输出。按显示宽度截断（fmtutil.TruncateDisplay）：
// 按字节切片（s[:max]）会撕开 UTF-8 多字节字符产生 mojibake，中文
// 输出场景（本项目常态）尤甚。
func truncateOut(s string, max int) string {
	return fmtutil.TruncateDisplay(s, max)
}
