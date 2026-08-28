package chart

// 子 chart 的查找与版本约束解析：执行、lint、template 预览统一走
// ResolveSub 入口，保证版本语义不分叉。

import (
	"fmt"
	"slices"
	"strings"

	"github.com/Masterminds/semver/v3"
)

// FindSub 按名递归查找子 chart（支持嵌套引用）。
// 遍历按子 chart 名排序：同名子 chart 分布在不同分支时结果确定，
// 不随 map 迭代顺序漂移（同一份 chart 两次解析命中同一实例）。
func (c *Chart) FindSub(name string) *Chart {
	if sub, ok := c.Subs[name]; ok {
		return sub
	}
	for _, n := range sortedSubNames(c.Subs) {
		if found := c.Subs[n].FindSub(name); found != nil {
			return found
		}
	}
	return nil
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
	sub := c.FindSub(name)
	if sub == nil {
		return nil, fmt.Errorf("subchart %q not found", name)
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
