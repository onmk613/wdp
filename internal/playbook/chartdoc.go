package playbook

// chart 引用任务的文档（`wdp module chart` 与 web 编辑器 /api/modules 的
// 伪模块条目共用同一来源）。chart 不是注册表里的内置模块（引用由执行器
// 展开为任务序列），所以文档在这里单独维护，与 TaskFieldSections 的
// chart 字段条目同包放置。

import (
	"fmt"
	"strings"

	"wdp/internal/module"
)

// ChartRefParams 返回 chart 引用 map 形态的参数文档。
func ChartRefParams() []module.ParamDoc {
	return []module.ParamDoc{
		{Name: "name", Type: "string", Desc: "引用名（必需）：chart 内 = charts/ 子 chart 名；裸 playbook = 同级 charts/<name>/ 目录。支持 jdk@^1.2 版本约束（semver）"},
		{Name: "values", Type: "map", Desc: "内联变量覆盖（与 vars 同义，最高优先级；chart 模式叠加在父作用域 <引用名> 子树/global 之上）"},
		{Name: "values_from", Type: "list", Desc: "values 覆盖文件（相对 playbook/chart 根目录，依序合并，后者胜出；优先级在作用域子树之上、内联 values 之下）"},
		{Name: "hosts", Type: "string", Desc: "主机过滤选择器：当前 play 批次与该选择器取交集，不在集合的主机跳过该引用（不跨 play 重选主机）"},
		{Name: "phase", Type: "string", Default: "deploy", Desc: "入口相位名（与 tasks_from 同义；uninstall/status/自定义相位）"},
	}
}

// ChartRefExample 返回可直接粘贴的示例任务。
func ChartRefExample() string {
	return `- name: 部署 nginx（chart 引用，唯一 map 形态）
  chart:
    name: nginx                     # chart 内 = 子 chart 名；裸 playbook = 同级 charts/ 目录
    values: {replicas: 2}           # 内联覆盖（可选）
    values_from: [values/prod.yaml] # 覆盖文件，依序合并（可选）
    hosts: webservers               # 当前批次内只对 web 组生效（可选）
    phase: deploy                   # 入口相位（可选，缺省 deploy）
`
}

// ChartRefDoc 渲染 chart 引用的完整文档（与 module.Snippet 同构：
// 名称描述 + 参数表 + 示例任务；`wdp module chart` 与 web 文档面板共用）。
func ChartRefDoc() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "# chart — 引用 chart 展开为任务序列（chart 内 = charts/ 子 chart；裸 playbook = 同级 charts/ 目录）\n")
	sb.WriteString("parameters:\n")
	for _, p := range ChartRefParams() {
		def := p.Default
		if def == "" {
			def = "-"
		}
		// 与 module.Snippet 同构：Enum 值域前置进说明列（"|" 分隔防歧义）
		desc := p.Desc
		if len(p.Enum) > 0 {
			desc = strings.Join(p.Enum, "|") + " — " + desc
		}
		fmt.Fprintf(&sb, "  %-14s %-6s %s %-8s %s\n", p.Name, p.Type, "default", def, desc)
	}
	sb.WriteString("example task:\n")
	sb.WriteString(ChartRefExample())
	return sb.String()
}
