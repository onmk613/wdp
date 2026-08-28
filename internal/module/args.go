package module

// 任务参数的解析与派发前校验：k=v / 自由表单的归一、YAML 1.1 布尔
// 惯用写法兼容、权限位解析与未知键拦截（拼错键 fail-loud）。

import (
	"fmt"
	"io/fs"
	"slices"
	"strconv"
	"strings"
)

func argStr(args map[string]any, key string) (string, bool) {
	v, ok := args[key]
	if !ok || v == nil {
		return "", false
	}
	switch x := v.(type) {
	case string:
		return x, true
	default:
		return fmt.Sprint(x), true
	}
}

// argStrList 接受字符串（空白分割）或列表。
func argStrList(args map[string]any, key string) ([]string, bool) {
	v, ok := args[key]
	if !ok || v == nil {
		return nil, false
	}
	switch x := v.(type) {
	case string:
		fields := strings.Fields(x)
		if len(fields) == 0 {
			return nil, true
		}
		return fields, true
	case []any:
		out := make([]string, 0, len(x))
		for _, it := range x {
			out = append(out, fmt.Sprint(it))
		}
		return out, true
	default:
		return []string{fmt.Sprint(x)}, true
	}
}

// argBool 解析布尔参数。除 Go 原生 true/false 外，
// 兼容 YAML 1.1 惯用写法 yes/no/on/off/1/0（大小写不敏感）。
// 无法解析时返回 ok=false（视为未提供）；ValidateArgs 会在派发前对
// bool 类型参数做严格校验，因此这里不会把 "maybe" 静默当 false。
func argBool(args map[string]any, key string) (bool, bool) {
	v, ok := args[key]
	if !ok || v == nil {
		return false, false
	}
	switch x := v.(type) {
	case bool:
		return x, true
	case int:
		if x == 0 {
			return false, true
		}
		if x == 1 {
			return true, true
		}
		return false, false
	case float64: // YAML 解析器可能产出浮点
		if x == 0 {
			return false, true
		}
		if x == 1 {
			return true, true
		}
		return false, false
	case string:
		switch strings.ToLower(strings.TrimSpace(x)) {
		case "true", "yes", "on", "1":
			return true, true
		case "false", "no", "off", "0":
			return false, true
		}
	}
	return false, false
}

// argInt 解析整数参数（YAML 数值可能是 int/int64/float64，或字符串数字）。
func argInt(args map[string]any, key string) (int, bool) {
	v, ok := args[key]
	if !ok || v == nil {
		return 0, false
	}
	switch x := v.(type) {
	case int:
		return x, true
	case int64:
		return int(x), true
	case float64:
		return int(x), true
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(x))
		if err != nil {
			return 0, false
		}
		return n, true
	}
	return 0, false
}

// parseState 解析各模块重复的 state 参数样板：空值归一为 def，
// 值不在 allowed 内时返回 (原值, false)（调用方 fail-loud 并回显原值）。
func parseState(args map[string]any, def string, allowed ...string) (string, bool) {
	s, _ := argStr(args, "state")
	if s == "" {
		s = def
	}
	if slices.Contains(allowed, s) {
		return s, true
	}
	return s, false
}

// argMode 解析 mode 参数："0755" / 0755(yaml 八进制整数) / 493。
// 仅接受权限位（0..0o777）：setuid/setgid/sticky（4755/2755/1777 等）
// 会在全链路 Perm() 中被静默丢弃成完全错误的权限，这里显式拒绝
// （ok=false）而非静默改错——需要特殊权限位时用 shell 模块显式 chmod。
// yaml.v3 对含 8/9 的纯数字（如 0999）解析成 float64——八进制里 8/9
// 非法，一律拒绝；带引号的 "0999" 走字符串分支同样拒绝。
func argMode(args map[string]any, key string) (fs.FileMode, bool) {
	v, ok := args[key]
	if !ok || v == nil {
		return 0, false
	}
	switch x := v.(type) {
	case int:
		if x < 0 || x > 0o777 {
			return 0, false
		}
		return fs.FileMode(x), true
	case int64:
		if x < 0 || x > 0o777 {
			return 0, false
		}
		return fs.FileMode(x), true
	case float64:
		if x != float64(int64(x)) || x < 0 || x > 0o777 {
			return 0, false
		}
		return fs.FileMode(int64(x)), true
	case string:
		s := strings.TrimSpace(x)
		base := 10
		if strings.HasPrefix(s, "0") && len(s) > 1 {
			base = 8
		}
		n, err := strconv.ParseUint(s, base, 32)
		if err != nil || n > 0o777 {
			return 0, false
		}
		return fs.FileMode(n), true
	default:
		return 0, false
	}
}

// ValidateArgs 在模块派发前校验任务参数（executor 调用）：
//   - 未知键直接报错（附相似键建议）——杜绝 moed/ownerr 之类拼写错误静默失效；
//   - bool 类型参数拒绝无法解析的值（如 "maybe"）；
//   - mode 类型参数拒绝非法值；
//   - "(any)" 参数声明模块接受任意键（set_fact/add_host vars 等键值对型
//     模块）：未声明的键直接放行，值类型由模块自身解释（键值对的值可以是
//     标量/列表/映射，无法用单一类型约束）。
//
// free 非空表示任务使用了 free-form 写法（如 `command: ls -l`），
// 仅声明了 "(free-form)" 参数的模块接受该写法。
// 未实现 UsageProvider（无参数元数据）时跳过校验。
func ValidateArgs(m Module, args map[string]any, free string) error {
	params := Usage(m)
	if params == nil {
		return nil
	}
	allowed := make(map[string]ParamDoc, len(params))
	for _, p := range params {
		allowed[p.Name] = p
	}
	if free != "" {
		if _, ok := allowed["(free-form)"]; !ok {
			return fmt.Errorf("module %s does not accept free-form syntax", m.Name())
		}
	}
	_, hasAny := allowed["(any)"]
	for k, v := range args {
		doc, ok := allowed[k]
		if !ok {
			if !hasAny {
				if s := suggestKey(k, params); s != "" {
					return fmt.Errorf("module %s has unknown parameter %q (did you mean %q?)", m.Name(), k, s)
				}
				return fmt.Errorf("module %s has unknown parameter %q", m.Name(), k)
			}
			continue // "(any)" 只对键放行，值类型不校验
		}
		switch doc.Type {
		case "bool":
			if _, ok := argBool(args, k); !ok {
				return fmt.Errorf("module %s parameter %s requires a boolean (true/false/yes/no), got %v", m.Name(), k, v)
			}
		case "mode":
			if _, ok := argMode(args, k); !ok {
				return fmt.Errorf("module %s parameter %s requires a valid permission (e.g. \"0755\"), got %v", m.Name(), k, v)
			}
		case "map":
			if _, ok := v.(map[string]any); !ok {
				return fmt.Errorf("module %s parameter %s requires a map, got %T", m.Name(), k, v)
			}
		}
	}
	return nil
}

// suggestKey 返回与未知键最相似的合法参数名（编辑距离 ≤ 2 时给出建议）。
func suggestKey(unknown string, params []ParamDoc) string {
	best, bestDist := "", 3
	for _, p := range params {
		d := editDistance(unknown, p.Name)
		if d < bestDist {
			best, bestDist = p.Name, d
		}
	}
	return best
}

// editDistance 经典 Levenshtein 距离（参数名都很短，无需优化）。
func editDistance(a, b string) int {
	la, lb := len(a), len(b)
	dp := make([][]int, la+1)
	for i := range dp {
		dp[i] = make([]int, lb+1)
		dp[i][0] = i
	}
	for j := range lb + 1 {
		dp[0][j] = j
	}
	for i := 1; i <= la; i++ {
		for j := 1; j <= lb; j++ {
			cost := 0
			if a[i-1] != b[j-1] {
				cost = 1
			}
			dp[i][j] = min(dp[i-1][j]+1, dp[i][j-1]+1, dp[i-1][j-1]+cost)
		}
	}
	return dp[la][lb]
}
