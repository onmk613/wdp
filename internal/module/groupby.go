package module

import (
	"fmt"
	"sort"
	"strings"

	"wdp/internal/i18n"
)

func init() {
	Register(&GroupByModule{})
}

// GroupByModule 按主机变量/facts 值动态建组。
//
//	group_by: name-{{ .os.family }}        # free-form 渲染后为组名
//	group_by:
//	  name: 'tier-{{ .tier | default "web" }}'
//	  prefix: dyn                          # 可选前缀（组名 = prefix-name）
//
// 典型用法：setup 之后 `group_by: os_{{ .os.family }}`，后续 play 用
// `hosts: os_debian` 精准分发布署逻辑。组在当前批次末尾聚合进 inventory，
// hosts 选择与 .groups 内置变量从下一批次/play 起可见。
type GroupByModule struct{}

// Name 模块名。
func (m *GroupByModule) Name() string { return "group_by" }

// Desc 模块说明。
func (m *GroupByModule) Desc() string {
	return i18n.T("build dynamic groups from vars/facts (used by later plays host selection)", "按变量/facts 值动态建组（配合后续 play 的 hosts 选择）")
}

// Run 产出组名（name/free-form 均已由 executor 渲染，此处仅组合 prefix）。
func (m *GroupByModule) Run(rc *RunContext, args map[string]any, free string) *Result {
	name := strings.TrimSpace(free)
	if n, ok := argStr(args, "name"); ok && n != "" {
		name = strings.TrimSpace(n)
	}
	if name == "" {
		return Fail("group_by 需要组名（name 参数或 free-form）")
	}
	group := name
	if prefix, ok := argStr(args, "prefix"); ok && prefix != "" {
		group = prefix + "-" + name
	}
	return &Result{Groups: []string{group}, Msg: fmt.Sprintf("加入动态组 %s", group)}
}

// SortGroups 排序去重组名列表（executor 聚合辅助）。
func SortGroups(groups []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(groups))
	for _, g := range groups {
		if !seen[g] {
			seen[g] = true
			out = append(out, g)
		}
	}
	sort.Strings(out)
	return out
}

// Params 参数文档。
func (m *GroupByModule) Params() []ParamDoc {
	return []ParamDoc{
		{Name: "name", Type: "string", Desc: "组名表达式（模板渲染后为组名；亦可用 free-form）"},
		{Name: "(free-form)", Type: "string", Desc: "组名表达式（name 的简写形式）"},
		{Name: "prefix", Type: "string", Desc: "可选前缀（组名 = prefix-name）"},
	}
}

// Example 示例任务。
func (m *GroupByModule) Example() string {
	return `- name: 按系统家族动态分组
  group_by: 'os_{{ .os.family }}'

- name: 下一 play 通配引用
  hosts: "os_*"
`
}
