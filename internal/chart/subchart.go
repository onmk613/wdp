package chart

// 子 chart 的查找与版本约束解析：执行、lint、template 预览统一走
// ResolveSub 入口，保证版本语义不分叉。

import (
	"fmt"
	"slices"
	"strings"

	"github.com/Masterminds/semver/v3"
)

// MaxChartDepth 是 chart 引用展开的深度上限：合法的组件组合远小于此值，
// 超过即视为环引用并报错（而非栈溢出崩溃）。executor 展开与可逆性评估
// 共用本上限。
const MaxChartDepth = 32

// FindSub 按名递归查找子 chart（支持嵌套引用）。词法作用域优先：先查
// 自身 Subs（就近遮蔽，与 Go 变量遮蔽同语义），未命中再向下递归。
// 跨分支出现同名子 chart 时报错并列出候选路径——此前按字典序静默选
// 一个，会把版本约束校验到错误的实例上。
func (c *Chart) FindSub(name string) (*Chart, error) {
	if sub, ok := c.Subs[name]; ok {
		return sub, nil
	}
	var found *Chart
	var paths []string
	var walk func(n *Chart, path string)
	walk = func(n *Chart, path string) {
		for _, cn := range sortedSubNames(n.Subs) {
			p := path + "." + cn
			if cn == name {
				if found == nil {
					found = n.Subs[cn]
				}
				paths = append(paths, p)
			}
			walk(n.Subs[cn], p)
		}
	}
	walk(c, c.Meta.Name)
	if len(paths) > 1 {
		return nil, fmt.Errorf("subchart %q is ambiguous across branches (candidates: %s); reference it from the branch that owns it or rename to disambiguate",
			name, strings.Join(paths, ", "))
	}
	if found == nil {
		return nil, fmt.Errorf("subchart %q not found", name)
	}
	return found, nil
}

// sortedSubNames 返回子 chart 名的有序列表（map 迭代随机，
// 需要确定性的遍历统一走这里）。
func sortedSubNames(subs map[string]*Chart) []string {
	names := make([]string, 0, len(subs))
	for n := range subs {
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}

// ResolveSub 解析子 chart 引用：`jdk` 或 `jdk@1.2.0`（版本约束，semver 语法，
// 如 jdk@^1.2 / jdk@>=1.0,<2.0）。执行、lint、template 预览统一走本入口，
// 保证版本语义不分叉。
func (c *Chart) ResolveSub(ref string) (*Chart, error) {
	name, constraint, constrained := strings.Cut(ref, "@")
	sub, err := c.FindSub(name)
	if err != nil {
		return nil, err
	}
	if constrained && constraint != "" {
		v, err := semver.NewVersion(sub.Meta.Version)
		if err != nil {
			return nil, fmt.Errorf("subchart %s version %q is not a semantic version, cannot apply constraint %q",
				name, sub.Meta.Version, constraint)
		}
		rng, err := semver.NewConstraint(constraint)
		if err != nil {
			return nil, fmt.Errorf("failed to parse version constraint %q: %w", constraint, err)
		}
		if !rng.Check(v) {
			return nil, fmt.Errorf("subchart %s version %s does not satisfy constraint %q", name, sub.Meta.Version, constraint)
		}
	}
	return sub, nil
}

// CollectHelpers 汇集自身与全部子 chart 的 _helpers.tpl（父在前，子重名覆盖；
// 兄弟子 chart 之间按名字典序遍历，覆盖顺序确定、渲染可复现）。
func (c *Chart) CollectHelpers() string {
	parts := []string{}
	if c.Helpers != "" {
		parts = append(parts, c.Helpers)
	}
	var walk func(sub *Chart)
	walk = func(sub *Chart) {
		if sub.Helpers != "" {
			parts = append(parts, sub.Helpers)
		}
		for _, n := range sortedSubNames(sub.Subs) {
			walk(sub.Subs[n])
		}
	}
	for _, n := range sortedSubNames(c.Subs) {
		walk(c.Subs[n])
	}
	return strings.Join(parts, "\n")
}
