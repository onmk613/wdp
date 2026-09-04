// Package chart 实现 Helm 风格的部署包：chart.yaml + values.yaml + deploy.yaml
// + _helpers.tpl + templates/ + files/ + envs/ + charts/（子 chart）。
package chart

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Merge 按 Helm 语义深合并：override 写入 base 之上。
// map 递归合并；标量与列表整体替换；显式 null 删除 base 中的键。
// 返回新 map，不修改入参。
func Merge(base, override map[string]any) map[string]any {
	out := make(map[string]any, len(base)+len(override))
	maps.Copy(out, base)
	for k, v := range override {
		if v == nil {
			delete(out, k)
			continue
		}
		if om, ok := v.(map[string]any); ok {
			if bm, ok := out[k].(map[string]any); ok {
				out[k] = Merge(bm, om)
				continue
			}
		}
		out[k] = v
	}
	return out
}

type pathSeg struct {
	key   string
	idx   int
	hasID bool // 段带 [n] 下标
}

// SubScope 计算子 chart 引用展开时的作用域 values（低 → 高）：
// 子 chart 默认 values → 父作用域 <子chart名> 子树 → global（跨层共享，
// 拷贝隔离；子 chart 自带的 global 默认值保留，父声明同名键时父胜出）。
// 执行器与 template 预览共用本规则；引用 vars（task 级）由调用方在其上继续叠加。
func SubScope(sub *Chart, parentScope map[string]any) map[string]any {
	scope := deepCopyValues(sub.Values)
	if tree, ok := parentScope[sub.Meta.Name].(map[string]any); ok {
		scope = Merge(scope, tree)
	}
	// global 合并而非整体替换：子 chart 自带 global 默认值不能因父 chart
	// 是否声明 global 而时有时无（组件行为被调用者的无关配置改变）
	if g, ok := parentScope["global"].(map[string]any); ok {
		subGlobal := map[string]any{}
		if own, ok := scope["global"].(map[string]any); ok {
			subGlobal = own
		}
		scope["global"] = Merge(subGlobal, g)
	}
	return scope
}

// deepCopyValues 深拷贝 values（map/[]any 递归，标量直接复制）。
// 合并与 --set 写入前深拷贝，避免嵌套 map 与 chart 默认 values / 父作用域
// 共享引用而被原地写污染（浅拷贝只隔离顶层键）。
func deepCopyValues(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = deepCopyAny(v)
	}
	return out
}

func deepCopyAny(v any) any {
	switch x := v.(type) {
	case map[string]any:
		return deepCopyValues(x)
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = deepCopyAny(e)
		}
		return out
	default:
		return v
	}
}

// SetPath 按点路径写入值，支持单层列表下标："a.b[0].c"。
// 中间路径不存在时自动创建；类型冲突时报错。返回根 map（即入参 root）。
// map 末段写 nil（--set k=null）时删除该键，与 -f 覆盖文件的 null 删除语义
// 及 README 承诺一致；列表元素写 nil 仍为占位赋值（删除会移动下标，语义不明）。
func SetPath(root map[string]any, path string, value any) (map[string]any, error) {
	segs, err := parsePath(path)
	if err != nil {
		return nil, err
	}
	cur := root
	for i, s := range segs {
		last := i == len(segs)-1
		if !s.hasID {
			if last {
				if value == nil {
					delete(cur, s.key)
					return root, nil
				}
				cur[s.key] = value
				return root, nil
			}
			next, ok := cur[s.key].(map[string]any)
			if !ok {
				if v, exists := cur[s.key]; exists && v != nil {
					return nil, fmt.Errorf("--set %q: %q is already a non-map value (%T)", path, s.key, cur[s.key])
				}
				next = map[string]any{}
				cur[s.key] = next
			}
			cur = next
			continue
		}
		// 段带 [n]：cur[s.key] 视作列表
		var list []any
		if v, ok := cur[s.key].([]any); ok {
			list = v
		} else if v, exists := cur[s.key]; exists && v != nil {
			return nil, fmt.Errorf("--set %q: %q is already a non-list value (%T)", path, s.key, cur[s.key])
		}
		if s.idx > 4096 {
			return nil, fmt.Errorf("--set %q: list index %d exceeds the 4096 cap (typo? indices pre-allocate memory)", path, s.idx)
		}
		for len(list) <= s.idx {
			list = append(list, nil)
		}
		if last {
			list[s.idx] = value
			cur[s.key] = list
			return root, nil
		}
		next, ok := list[s.idx].(map[string]any)
		if !ok {
			if v := list[s.idx]; v != nil {
				return nil, fmt.Errorf("--set %q: %s[%d] is already a non-map value (%T)", path, s.key, s.idx, v)
			}
			next = map[string]any{}
			list[s.idx] = next
		}
		cur[s.key] = list
		cur = next
	}
	return root, nil
}

// parsePath 解析 "a.b[0].c" 为段序列。
func parsePath(path string) ([]pathSeg, error) {
	if path == "" {
		return nil, errors.New("empty path")
	}
	var segs []pathSeg
	i, n := 0, len(path)
	for i < n {
		start := i
		for i < n && path[i] != '.' && path[i] != '[' {
			i++
		}
		key := path[start:i]
		if key == "" {
			return nil, fmt.Errorf("invalid path %q (empty key segment)", path)
		}
		s := pathSeg{key: key}
		if i < n && path[i] == '[' {
			j := strings.IndexByte(path[i:], ']')
			if j < 0 {
				return nil, fmt.Errorf("invalid path %q (missing ])", path)
			}
			v, err := strconv.Atoi(path[i+1 : i+j])
			if err != nil || v < 0 {
				return nil, fmt.Errorf("invalid index %q", path[i+1:i+j])
			}
			s.idx, s.hasID = v, true
			i += j + 1
			if i < n && path[i] == '[' {
				return nil, fmt.Errorf("path %q contains multi-level indices, not supported yet", path)
			}
		}
		segs = append(segs, s)
		if i < n {
			if path[i] != '.' {
				return nil, fmt.Errorf("invalid path %q (expected . or [)", path)
			}
			i++
			if i == n {
				return nil, fmt.Errorf("invalid path %q (ends with .)", path)
			}
		}
	}
	return segs, nil
}

// ParseSet 解析 --set 参数："a.b.c=v"。值做类型推断：
// 整数/浮点/true/false/null 按字面量，其余保持字符串。
func ParseSet(pair string) (string, any, error) {
	k, v, ok := strings.Cut(pair, "=")
	if !ok {
		return "", nil, fmt.Errorf("--set requires k=v form, got %q", pair)
	}
	k = strings.TrimSpace(k)
	if k == "" {
		return "", nil, fmt.Errorf("--set key is empty: %q", pair)
	}
	return k, inferType(v), nil
}

// ApplySet 把一个 --set 参数应用进 values。
func ApplySet(values map[string]any, pair string) (map[string]any, error) {
	k, v, err := ParseSet(pair)
	if err != nil {
		return nil, err
	}
	return SetPath(values, k, v)
}

// inferType 推断字符串字面量类型。
func inferType(s string) any {
	switch s {
	case "true":
		return true
	case "false":
		return false
	case "null", "~":
		return nil
	case "":
		return ""
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return n
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return f
	}
	return s
}

// LoadValuesYAML 解析 values YAML 内容（空内容返回空 map）。
func LoadValuesYAML(data []byte) (map[string]any, error) {
	if len(strings.TrimSpace(string(data))) == 0 {
		return map[string]any{}, nil
	}
	var m map[string]any
	if err := yaml.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	if m == nil {
		return map[string]any{}, nil
	}
	return m, nil
}

// BuildValues 构建最终 values：默认 values → 依序合并 -f 文件 → --set 参数。
// 默认 values 先深拷贝：--set 的点路径写入是原地写，浅拷贝会让嵌套 map
// 与 c.Values 共享引用而污染 chart 默认值。
func (c *Chart) BuildValues(files []string, sets []string) (map[string]any, error) {
	return ApplyOverrides(c.Values, files, sets)
}

// ApplyOverrides 在基线 values 上依序合并 -f 文件与 --set 参数（基线先深
// 拷贝）。部署相位以 chart 默认 values 为基线；非部署相位（uninstall 等）
// 以主机 marker 还原的 values 为基线——显式 -f/--set 是对实际部署入参的
// 覆盖，不是对 values.yaml 默认值的覆盖。
func ApplyOverrides(base map[string]any, files []string, sets []string) (map[string]any, error) {
	merged := deepCopyValues(base)
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			return nil, fmt.Errorf("failed to read values file: %w", err)
		}
		ov, err := LoadValuesYAML(data)
		if err != nil {
			return nil, fmt.Errorf("failed to parse %s: %w", f, err)
		}
		merged = Merge(merged, ov)
	}
	for _, s := range sets {
		var err error
		if merged, err = ApplySet(merged, s); err != nil {
			return nil, err
		}
	}
	return merged, nil
}
