package chart

// chart.yaml 字段表（`wdp schema chart` 数据源）。与 meta.go 的 Meta 结构
// 同包放置：文档长在解析器旁边，对账测试（TestChartFieldsReconcile）用
// 反射校验"每个文档键真的对应 Meta 的 yaml tag"，新增字段忘写文档或字段
// 改名都会被点名。

import "wdp/internal/model"

// ChartFieldSections 返回 chart.yaml 字段的分组表（含 phases 值的属性表）。
func ChartFieldSections() []model.FieldSection {
	return []model.FieldSection{
		{
			Title: "标识与元数据",
			Fields: []model.FieldDoc{
				{Name: "name", Type: "string", Default: "-（必需）", Desc: "chart 名：字母/数字开头，可用 . _ -（拼进远端 marker 路径，路径分隔符与 .. 在加载期拒绝）", GoField: "Name"},
				{Name: "version", Type: "string", Default: "-", Desc: "版本：子 chart 引用 jdk@^1.2 的版本约束按 semver 校验", GoField: "Version"},
				{Name: "description", Type: "string", Default: "-", Desc: "说明文本（仅展示）", GoField: "Description"},
			},
			Example: `
name: myapp
version: 0.1.0
description: 我的应用
`,
		},
		{
			Title: "values 契约",
			Fields: []model.FieldDoc{
				{Name: "required", Type: "list", Default: "-", Desc: "必须由调用方提供的 values 点路径（支持 a.b[0] 写法）；部署相位缺失即报错", GoField: "Required"},
				{Name: "inventory_override", Type: "list", Default: "-", Desc: "允许被 inventory 同名变量覆盖的顶层 values 键（白名单；未列入时同名 values 恒赢）", GoField: "InventoryOverride"},
				{Name: "sensitive_values", Type: "list", Default: "-", Desc: "敏感 values 点路径：marker 落盘时替换为 <redacted> 且不参与 drift 摘要（卸载等从 marker 还原的相位会拿到占位值）", GoField: "SensitiveValues"},
			},
			Example: `
required: [app.name, db.host]
inventory_override: [tier]
sensitive_values: [db.password]
`,
		},
		{
			Title: "release marker 与预演",
			Fields: []model.FieldDoc{
				{Name: "marker_dir", Type: "string", Default: "/var/lib/wdp", Desc: "目标机 marker 目录（必须是干净的绝对路径，且不是文件系统根）", GoField: "MarkerDir"},
				{Name: "no_marker", Type: "bool", Default: "false", Desc: "true = 不写 release marker（随之无法 uninstall/drift）", GoField: "NoMarker"},
				{Name: "check_mode", Type: "bool|string", Default: "false", Desc: "supported/true = 声明 chart 自带脚本模块（modules/）支持 --check 预演；未声明时脚本模块在 check 下跳过", GoField: "CheckMode"},
			},
			Example: `
marker_dir: /var/lib/wdp
check_mode: supported
`,
		},
		{
			Title: "生命周期相位（phases.<相位名>）",
			Fields: []model.FieldDoc{
				{Name: "phases", Type: "map", Default: "-", Desc: "相位属性声明：键是相位名（须有对应 <相位名>.yaml），值见下表的四个属性", GoField: "Phases"},
			},
			Example: `
phases:
  update: {release: true}            # update 视同一次部署（写 marker/记记录/部署前确认）
  backup: {record: true}             # 只记部署记录
  purge:  {clears_marker: true}      # 成功后清除 marker
  status: {values_from: marker}      # 只读相位改读已部署入参
`,
		},
		{
			Title: "相位属性（phases.<名> 的值）",
			Fields: []model.FieldDoc{
				{Name: "release", Type: "bool", Default: "false（deploy 内置 true）", Desc: "部署语义：required 校验、可逆性确认、成功后写 marker、记部署记录"},
				{Name: "record", Type: "bool", Default: "false（deploy/uninstall 内置 true）", Desc: "记部署记录（release 隐含本项）"},
				{Name: "clears_marker", Type: "bool", Default: "false（uninstall 内置 true）", Desc: "成功后清除 release marker"},
				{Name: "values_from", Type: "string", Default: "按相位推导：clears_marker 相位取 marker，其余取 chart", Desc: "values 来源：chart（values.yaml + -f/--set）| marker（各主机已部署入参 + -f/--set）"},
			},
		},
	}
}

// PhaseFieldNames 返回相位属性的 yaml 键名（对账测试用）。
func PhaseFieldNames() []string {
	return []string{"release", "record", "clears_marker", "values_from"}
}
