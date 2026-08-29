package report

import "strings"

// firstLine 取首行（去首尾空白），主机清单的错误摘要用。
func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s
}

// truncateOut 截断长输出。
func truncateOut(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
