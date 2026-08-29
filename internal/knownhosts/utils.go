package knownhosts

// truncateLine 截断超长行（告警展示用）。
func truncateLine(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
