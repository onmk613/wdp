package module

import (
	"fmt"
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

func (m *GroupByModule) Name() string { return "group_by" }

func (m *GroupByModule) Desc() string {
	return i18n.T("Create groups dynamically from variables/facts (for wildcard host selection in later plays)", "按变量/facts 动态建组（供后续 play 的 hosts 通配选择）")
}

// Run 产出组名（name/free-form 均已由 executor 渲染，此处仅组合 prefix）。
func (m *GroupByModule) Run(_ *RunContext, args map[string]any, free string) *Result {
	name := strings.TrimSpace(free)
	if n, ok := argStr(args, "name"); ok && n != "" {
		name = strings.TrimSpace(n)
	}
	if name == "" {
		return Fail("group_by requires a group name (name parameter or free-form)")
	}
	group := name
	if prefix, ok := argStr(args, "prefix"); ok && prefix != "" {
		group = prefix + "-" + name
	}
	return &Result{Groups: []string{group}, Msg: fmt.Sprintf("joined dynamic group %s", group)}
}

func (m *GroupByModule) Params() []ParamDoc {
	return []ParamDoc{
		{Name: "name", Type: "string", Desc: i18n.T("group name expression (the template rendering result is the group name; a free-form shorthand also works)", "组名表达式（模板渲染结果即组名；也可用 free-form 简写）")},
		{Name: "(free-form)", Type: "string", Desc: i18n.T("shorthand for the group name expression: `group_by: 'os_{{ .os.family }}'`", "组名表达式的简写：`group_by: 'os_{{ .os.family }}'`")},
		{Name: "prefix", Type: "string", Desc: i18n.T("optional prefix (final group name = prefix-name)", "可选前缀（最终组名 = prefix-名称）")},
	}
}

func (m *GroupByModule) Example() string {
	return i18n.T(`- name: Create groups dynamically by OS family
  group_by: 'os_{{ .os.family }}'

- name: Reference them with a wildcard in the next play
  hosts: "os_*"
`, `- name: 按系统族动态建组
  group_by: 'os_{{ .os.family }}'

- name: 下个 play 用通配引用
  hosts: "os_*"
`)
}
