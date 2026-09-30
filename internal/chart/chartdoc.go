package chart

// chart.yaml 字段表（`wdp schema chart` 数据源）。与 meta.go 的 Meta 结构
// 同包放置：文档长在解析器旁边，对账测试（TestChartFieldsReconcile）用
// 反射校验"每个文档键真的对应 Meta 的 yaml tag"，新增字段忘写文档或字段
// 改名都会被点名。

import (
	"wdp/internal/i18n"
	"wdp/internal/model"
)

// ChartFieldSections 返回 chart.yaml 字段的分组表（含 phases 值的属性表）。
func ChartFieldSections() []model.FieldSection {
	return []model.FieldSection{
		{
			Title: i18n.T("Identity and metadata", "标识与元数据"),
			Fields: []model.FieldDoc{
				{Name: "name", Type: "string", Default: i18n.T("- (required)", "-（必需）"), Desc: i18n.T("chart name: starts with a letter/digit, may contain . _ - (embedded into the remote marker path; path separators and .. are rejected at load time)", "chart 名：字母/数字开头，可用 . _ -（拼进远端 marker 路径，路径分隔符与 .. 在加载期拒绝）"), GoField: "Name"},
				{Name: "version", Type: "string", Default: "-", Desc: i18n.T("version: a sub-chart reference such as jdk@^1.2 is validated as a semver constraint", "版本：子 chart 引用 jdk@^1.2 的版本约束按 semver 校验"), GoField: "Version"},
				{Name: "description", Type: "string", Default: "-", Desc: i18n.T("description text (display only)", "说明文本（仅展示）"), GoField: "Description"},
			},
			Example: i18n.T(`
name: myapp
version: 0.1.0
description: my app
`, `
name: myapp
version: 0.1.0
description: 我的应用
`),
		},
		{
			Title: i18n.T("values contract", "values 契约"),
			Fields: []model.FieldDoc{
				{Name: "required", Type: "list", Default: "-", Desc: i18n.T("values dot-paths the caller must provide (a.b[0] form supported); a deployment phase errors out when one is missing", "必须由调用方提供的 values 点路径（支持 a.b[0] 写法）；部署相位缺失即报错"), GoField: "Required"},
				{Name: "inventory_override", Type: "list", Default: "-", Desc: i18n.T("top-level values keys that a same-named inventory variable may override (a whitelist; for keys not listed, values always win)", "允许被 inventory 同名变量覆盖的顶层 values 键（白名单；未列入时同名 values 恒赢）"), GoField: "InventoryOverride"},
				{Name: "sensitive_values", Type: "list", Default: "-", Desc: i18n.T("sensitive values dot-paths: replaced with <redacted> when the marker is written and excluded from the drift digest (phases that restore from the marker, such as uninstall, get the placeholder value)", "敏感 values 点路径：marker 落盘时替换为 <redacted> 且不参与 drift 摘要（卸载等从 marker 还原的相位会拿到占位值）"), GoField: "SensitiveValues"},
			},
			Example: `
required: [app.name, db.host]
inventory_override: [tier]
sensitive_values: [db.password]
`,
		},
		{
			Title: i18n.T("release marker and dry run", "release marker 与预演"),
			Fields: []model.FieldDoc{
				{Name: "marker_dir", Type: "string", Default: "/var/lib/wdp", Desc: i18n.T("marker directory on the target host (must be a clean absolute path and not the filesystem root)", "目标机 marker 目录（必须是干净的绝对路径，且不是文件系统根）"), GoField: "MarkerDir"},
				{Name: "no_marker", Type: "bool", Default: "false", Desc: i18n.T("true = write no release marker (which in turn rules out uninstall/drift)", "true = 不写 release marker（随之无法 uninstall/drift）"), GoField: "NoMarker"},
				{Name: "check_mode", Type: "bool|string", Default: "false", Desc: i18n.T("supported/true = declares that the chart's own script modules (modules/) support --check dry runs; when undeclared, script modules are skipped under check", "supported/true = 声明 chart 自带脚本模块（modules/）支持 --check 预演；未声明时脚本模块在 check 下跳过"), GoField: "CheckMode"},
			},
			Example: `
marker_dir: /var/lib/wdp
check_mode: supported
`,
		},
		{
			Title: i18n.T("Lifecycle phases (phases.<phase>)", "生命周期相位（phases.<相位名>）"),
			Fields: []model.FieldDoc{
				{Name: "phases", Type: "map", Default: "-", Desc: i18n.T("phase attribute declarations: keys are phase names (each needs a matching <phase>.yaml), values are the four attributes in the table below", "相位属性声明：键是相位名（须有对应 <相位名>.yaml），值见下表的四个属性"), GoField: "Phases"},
			},
			Example: i18n.T(`
phases:
  update: {release: true}            # update counts as a deployment (writes the marker / records it / confirms before deploying)
  backup: {record: true}             # record the deployment only
  purge:  {clears_marker: true}      # clear the marker on success
  status: {values_from: marker}      # the read-only phase reads the deployed inputs instead
`, `
phases:
  update: {release: true}            # update 视同一次部署（写 marker/记记录/部署前确认）
  backup: {record: true}             # 只记部署记录
  purge:  {clears_marker: true}      # 成功后清除 marker
  status: {values_from: marker}      # 只读相位改读已部署入参
`),
		},
		{
			Title: i18n.T("Phase attributes (the value of phases.<name>)", "相位属性（phases.<名> 的值）"),
			Fields: []model.FieldDoc{
				{Name: "release", Type: "bool", Default: i18n.T("false (built in as true for deploy)", "false（deploy 内置 true）"), Desc: i18n.T("deployment semantics: required validation, reversibility confirmation, marker write on success, deployment record", "部署语义：required 校验、可逆性确认、成功后写 marker、记部署记录")},
				{Name: "record", Type: "bool", Default: i18n.T("false (built in as true for deploy/uninstall)", "false（deploy/uninstall 内置 true）"), Desc: i18n.T("record the deployment (implied by release)", "记部署记录（release 隐含本项）")},
				{Name: "clears_marker", Type: "bool", Default: i18n.T("false (built in as true for uninstall)", "false（uninstall 内置 true）"), Desc: i18n.T("clear the release marker on success", "成功后清除 release marker")},
				{Name: "values_from", Type: "string", Default: i18n.T("derived per phase: clears_marker phases use marker, the rest use chart", "按相位推导：clears_marker 相位取 marker，其余取 chart"), Desc: i18n.T("values source: chart (values.yaml + -f/--set) | marker (each host's deployed inputs + -f/--set)", "values 来源：chart（values.yaml + -f/--set）| marker（各主机已部署入参 + -f/--set）")},
			},
		},
	}
}

// PhaseFieldNames 返回相位属性的 yaml 键名（对账测试用）。
func PhaseFieldNames() []string {
	return []string{"release", "record", "clears_marker", "values_from"}
}
