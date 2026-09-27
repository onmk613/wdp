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

// FindSub 按名查找子 chart（支持嵌套引用）。解析视角恒为根 chart：
// lint（lint.go）与 executor（expand.go）都以根调用本方法，根的直接子
// chart 优先命中，未命中再自根整树递归——因此某个子 chart 自己的同名
// 子 chart 不会被"就近"选中（子 chart 无独立解析入口）。这一口径必须
// 保持：lint 与 executor 若一侧改成从引用所在 chart 就近解析，另一侧
// 仍从根解析，lint 通过的引用会在执行期解析到另一个实例。
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

// CheckVersionConstraint 校验 got 是否满足 want 约束（semver 语法，如
// ^1.2 / >=1.0,<2.0）。子 chart 解析（ResolveSub）与裸 playbook 的
// ChartRefs 解析（executor/expand.go）共用本入口，保证版本语义不分叉
// ——此前两处各写一份逻辑，全靠人肉保持一致。
// kind 是错误文案里的主语（"subchart"/"chart"），随调用方语境。
func CheckVersionConstraint(kind, name, got, want string) error {
	v, err := semver.NewVersion(got)
	if err != nil {
		return fmt.Errorf("%s %s version %q is not a semantic version, cannot apply constraint %q", kind, name, got, want)
	}
	rng, err := semver.NewConstraint(want)
	if err != nil {
		return fmt.Errorf("failed to parse version constraint %q: %w", want, err)
	}
	if !rng.Check(v) {
		return fmt.Errorf("%s %s version %s does not satisfy constraint %q", kind, name, got, want)
	}
	return nil
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
		if err := CheckVersionConstraint("subchart", name, sub.Meta.Version, constraint); err != nil {
			return nil, err
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
