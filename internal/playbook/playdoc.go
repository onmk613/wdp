package playbook

// play 级字段表（编辑器 schema 端点的数据源）。与 playKeys 同包放置：
// 文档长在解析器旁边，TestPlayFieldsReconcile 用 playKeys 对账防漂移——
// 新增 play 键不写文档、或文档捏造不存在的键，测试都会失败。

import "wdp/internal/model"

// PlayFieldSections 返回 play 级字段的分组表。
func PlayFieldSections() []model.FieldSection {
	return []model.FieldSection{
		{
			Title: "play 标识与主机选择",
			Fields: []model.FieldDoc{
				{Name: "name", Type: "string", Default: "-", Desc: "play 名称（输出与部署记录里标识这批任务）", GoField: "Name"},
				{Name: "hosts", Type: "string", Default: "all", Desc: "主机选择模式：组名 / 主机名列表（逗号分隔）/ all；chart 惯例是与应用同名的组", GoField: "Hosts"},
			},
			Example: `
- name: 部署 web
  hosts: webservers      # 组名，或 web1,web2 / all
`,
		},
		{
			Title: "变量与执行环境",
			Fields: []model.FieldDoc{
				{Name: "vars", Type: "map", Default: "-", Desc: "play 级变量（本 play 全部任务可见，值支持模板）", GoField: "Vars"},
				{Name: "environment", Type: "map", Default: "-", Desc: "play 级环境变量（任务级可覆盖）", GoField: "Environment"},
				{Name: "become", Type: "bool", Default: "false", Desc: "整批任务提权（宽容 yes/no/on/off）", GoField: "Become"},
				{Name: "become_user", Type: "string", Default: "root", Desc: "提权目标用户", GoField: "BecomeUser"},
			},
			Example: `
- hosts: webservers
  vars:
    app_port: 8080
  environment:
    LANG: C
  become: true
`,
		},
		{
			Title: "批次与策略",
			Fields: []model.FieldDoc{
				{Name: "serial", Type: "string | int | list", Default: "一批", Desc: `分批大小："5" / "10%" / "5,10,20"（最后一个数字重复用到底）`, GoField: "Serial"},
				{Name: "strategy", Type: "map", Default: "-", Desc: "部署策略：type（linear|rolling|canary）+ batch + gate（批间健康门）+ auto_rollback（失败自动回滚）", GoField: "Strategy"},
			},
			Example: `
- hosts: webservers
  serial: "25%"
  strategy:
    type: rolling
    gate: {shell: 'curl -sf http://localhost:8080/health', retries: 10, delay: 3}
    auto_rollback: true
`,
		},
		{
			Title: "任务列表",
			Fields: []model.FieldDoc{
				{Name: "tasks", Type: "list<task>", Default: "-", Desc: "主任务列表（按序执行，单键 map 模块语法）", GoField: "Tasks"},
				{Name: "handlers", Type: "list<task>", Default: "-", Desc: "处理器列表：notify 触发，play 末尾统一 flush。约定集中放 handlers/ 目录（片段 = 裸任务列表，每项一个 handler），play 里 `- include: handlers/x.yaml` 引入", GoField: "Handlers"},
			},
			Example: `
- hosts: webservers
  tasks:
    - name: 下发配置
      template: {src: app.conf.tpl, dest: /etc/app/app.conf}
      notify: reload app
  # handlers 约定目录：handlers/reload.yaml（片段是裸任务列表），
  # play 里 include 引入；少量 handler 也可直接内联
  handlers:
    - include: handlers/reload.yaml
    - name: 内联回退
      shell: 'systemctl reload app'
`,
		},
	}
}
