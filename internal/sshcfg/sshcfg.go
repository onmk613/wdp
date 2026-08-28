package sshcfg

// ~/.ssh/config 子集解析：按 OpenSSH 首匹配语义提取 IdentityFile / User /
// Port，消除"交互 ssh 能通、wdp 不通"的环境差异（交互 ssh 走了用户配置，
// wdp 此前完全不看）。
//
// 对齐 OpenSSH ssh_config 语义：
//   - 标量参数（User/Port）取文件顺序中第一个匹配 Host 块的值；
//   - IdentityFile 跨全部匹配块累积；一旦配置了 IdentityFile，即不再
//     追加内置默认密钥（authMethods 侧实现）；
//   - 首个 Host 之前的指令为全局块，对所有主机生效；
//   - Match 块不求值（criteria 依赖交互/网络状态，无法可靠求值），跳过；
//   - Include 原地展开（支持 glob、~ 展开；相对路径按包含文件目录解释，
//     顶层即 ~/.ssh/config 时等价于相对 ~/.ssh；带环与深度防护）。
//
// 优先级（高→低）：inventory 主机条目显式键 > ~/.ssh/config > wdp.cfg
// 默认值/内置默认——对应 OpenSSH 的 命令行 > 用户配置 > 内置默认。
// 环境变量 WDP_SSH_CONFIG 可覆盖配置文件路径（"none" 显式禁用）。

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"wdp/internal/model"
)

// sshConfigParams 是 ~/.ssh/config 中影响 wdp 连接参数的子集。
type sshConfigParams struct {
	user          string
	port          int // 0 = 未指定
	identityFiles []string
}

// FillFromSSHConfig 用 ~/.ssh/config 解析结果补全 SSH 连接参数：显式指定
// 的字段（inventory 主机条目）保持不动，未显式指定的字段允许 ssh config
// 覆盖调用方预填的默认值。仅影响 conn 为 ssh/push 的主机（push 自举走
// SSH，参数同样生效）；agent/local 通道与 SSH 参数无关。
func FillFromSSHConfig(h *model.Host, explicitUser, explicitPort, explicitKeyPath bool) {
	if h.Conn != "ssh" && h.Conn != "push" {
		return
	}
	params := loadSSHConfigParams(h.Name, h.Address)
	if !explicitUser && params.user != "" {
		h.User = params.user
	}
	if !explicitPort && params.port != 0 {
		h.Port = params.port
	}
	if !explicitKeyPath && len(params.identityFiles) > 0 {
		h.IdentityFiles = params.identityFiles
	}
}

// sshConfigPath 返回用户 ssh 配置路径（空 = 不解析）。WDP_SSH_CONFIG
// 优先（"none" 显式禁用），缺省 ~/.ssh/config。
func sshConfigPath() string {
	if p := os.Getenv("WDP_SSH_CONFIG"); p != "" {
		if p == "none" {
			return ""
		}
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".ssh", "config")
}

// loadSSHConfigParams 解析用户 ssh 配置并按主机标识（inventory 主机名与
// 连接地址，任一匹配即算命中）归集参数。文件不存在时返回零值。
func loadSSHConfigParams(targets ...string) sshConfigParams {
	path := sshConfigPath()
	if path == "" {
		return sshConfigParams{}
	}
	lower := make(map[string]bool, len(targets))
	for _, t := range targets {
		if t = strings.ToLower(strings.TrimSpace(t)); t != "" {
			lower[t] = true
		}
	}
	if len(lower) == 0 {
		return sshConfigParams{}
	}
	f := &cfgFold{targets: lower, seenFiles: map[string]bool{}, seenKeys: map[string]bool{}}
	f.walkFile(path, blockCtx{kind: blockGlobal}, 0)
	return f.params
}

// 块状态：全局区（首个 Host 前，匹配全部主机）/ Host 块 / Match 块（跳过）。
const (
	blockGlobal = iota
	blockHost
	blockMatch
)

type blockCtx struct {
	kind     int
	patterns []string // blockHost 时的 Host 模式列表
}

// cfgFold 按文件顺序折叠 ssh 配置指令：标量参数首个命中值生效，
// IdentityFile 累积。状态在 Include 边界原样传递（被包含文件的指令
// 视作出现在 Include 位置）。
type cfgFold struct {
	targets   map[string]bool
	params    sshConfigParams
	seenFiles map[string]bool // 已展开文件（绝对路径），防 Include 环
	seenKeys  map[string]bool // IdentityFile 去重
}

// walkFile 逐行处理单个配置文件，cur 为进入该文件时所处的块状态。
func (f *cfgFold) walkFile(path string, cur blockCtx, depth int) {
	if depth > 16 {
		fmt.Fprintf(os.Stderr, "[ssh] %s: include depth exceeds 16, skipped\n", path)
		return
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = filepath.Clean(path)
	}
	if f.seenFiles[abs] {
		return // Include 环（或重复包含）：只展开首次
	}
	f.seenFiles[abs] = true

	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "[ssh] %s: %v\n", path, err)
		}
		return
	}
	for i, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(strings.TrimRight(raw, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		tokens := tokenizeConfigLine(line)
		if len(tokens) == 0 {
			continue
		}
		keyword, args := strings.ToLower(tokens[0]), tokens[1:]
		badLine := func(reason string) {
			fmt.Fprintf(os.Stderr, "[ssh] %s:%d skipped malformed line (%s): %s\n",
				path, i+1, reason, truncateLine(line, 80))
		}
		switch keyword {
		case "host":
			if len(args) == 0 {
				badLine("missing pattern")
				continue
			}
			cur = blockCtx{kind: blockHost, patterns: args}
		case "match":
			cur = blockCtx{kind: blockMatch} // criteria 不求值，块内指令跳过
		case "include":
			f.walkIncludes(path, args, cur, depth)
		case "user":
			if !f.ctxMatches(cur) || f.params.user != "" {
				continue
			}
			if len(args) != 1 {
				badLine("user takes one argument")
				continue
			}
			f.params.user = args[0]
		case "port":
			if !f.ctxMatches(cur) || f.params.port != 0 {
				continue
			}
			if len(args) != 1 {
				badLine("port takes one argument")
				continue
			}
			n, perr := strconv.Atoi(args[0])
			if perr != nil || n < 1 || n > 65535 {
				badLine("invalid port")
				continue
			}
			f.params.port = n
		case "identityfile":
			if !f.ctxMatches(cur) {
				continue
			}
			if len(args) != 1 {
				badLine("identityfile takes one argument")
				continue
			}
			p := expandTilde(args[0])
			if f.seenKeys[p] {
				continue
			}
			f.seenKeys[p] = true
			f.params.identityFiles = append(f.params.identityFiles, p)
		}
	}
}

// walkIncludes 原地展开 Include 参数：glob、~ 展开，相对路径按包含文件
// 所在目录解释。Include 无论外层 Host 块是否匹配都会展开（与 OpenSSH
// 一致：被包含文件自身的 Host 块独立生效）。
func (f *cfgFold) walkIncludes(from string, args []string, cur blockCtx, depth int) {
	for _, arg := range args {
		p := expandTilde(arg)
		if !filepath.IsAbs(p) {
			p = filepath.Join(filepath.Dir(from), p)
		}
		matches, err := filepath.Glob(p)
		if err != nil || len(matches) == 0 {
			if !hasGlobMeta(p) && err == nil {
				fmt.Fprintf(os.Stderr, "[ssh] include %s: no such file, skipped\n", p)
			}
			continue
		}
		for _, m := range matches {
			f.walkFile(m, cur, depth+1)
		}
	}
}

// ctxMatches 判断当前块对该组主机标识是否生效。
func (f *cfgFold) ctxMatches(ctx blockCtx) bool {
	switch ctx.kind {
	case blockGlobal:
		return true
	case blockHost:
		for t := range f.targets {
			if hostMatchesPatterns(t, ctx.patterns) {
				return true
			}
		}
		return false
	default: // blockMatch
		return false
	}
}

// hostMatchesPatterns 按 OpenSSH 语义判断主机是否命中 Host 模式列表：
// 任一取反模式（! 前缀）命中即整体不匹配（优先于正向命中），
// 否则需至少一个正向模式命中。模式与主机均按小写比较，支持 * 与 ?。
func hostMatchesPatterns(target string, patterns []string) bool {
	positive := false
	for _, p := range patterns {
		neg := strings.HasPrefix(p, "!")
		p = strings.TrimPrefix(p, "!")
		if wildcardMatch(strings.ToLower(p), target) {
			if neg {
				return false
			}
			positive = true
		}
	}
	return positive
}

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
