package sshcfg

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// wildcardMatch 实现 * / ? 通配匹配（不含 [] 字符类，ssh_config 罕用）。
func wildcardMatch(pattern, s string) bool {
	var sb strings.Builder
	sb.WriteString("^")
	for _, r := range pattern {
		switch r {
		case '*':
			sb.WriteString(".*")
		case '?':
			sb.WriteString(".")
		default:
			sb.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	sb.WriteString("$")
	re, err := regexp.Compile(sb.String())
	return err == nil && re.MatchString(s)
}

// tokenizeConfigLine 切分指令行：空白分隔，双引号内可含空格，
// 支持 OpenSSH 的 "Keyword=Value" 等价写法。
func tokenizeConfigLine(line string) []string {
	var tokens []string
	var cur strings.Builder
	inQuote, hasToken := false, false
	flush := func() {
		if hasToken {
			tokens = append(tokens, cur.String())
			cur.Reset()
			hasToken = false
		}
	}
	for _, r := range line {
		switch {
		case r == '"':
			inQuote = !inQuote
			hasToken = true
		case (r == ' ' || r == '\t') && !inQuote:
			flush()
		default:
			cur.WriteRune(r)
			hasToken = true
		}
	}
	flush()
	// Keyword=Value：等价 Keyword Value
	if len(tokens) > 0 {
		if k, v, ok := strings.Cut(tokens[0], "="); ok {
			tokens = append([]string{k, v}, tokens[1:]...)
		}
	}
	return tokens
}

// expandTilde 展开 ~/ 与 ~ 为用户主目录（其余 ~user 形式不动）。
func expandTilde(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}

// hasGlobMeta 判断路径是否含 glob 元字符（区分"无匹配"与"字面路径缺失"）。
func hasGlobMeta(p string) bool {
	return strings.ContainsAny(p, "*?[")
}

// truncateLine 截断超长行（告警展示用）。
func truncateLine(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
