package playbook

// play 级字段表（编辑器 schema 端点的数据源）。与 playKeys 同包放置：
// 文档长在解析器旁边，TestPlayFieldsReconcile 用 playKeys 对账防漂移——
// 新增 play 键不写文档、或文档捏造不存在的键，测试都会失败。

import (
	"wdp/internal/i18n"
	"wdp/internal/model"
)

// PlayFieldSections 返回 play 级字段的分组表。
func PlayFieldSections() []model.FieldSection {
	return []model.FieldSection{
		{
			Title: i18n.T("play identity and host selection", "play 标识与主机选择"),
			Fields: []model.FieldDoc{
				{Name: "name", Type: "string", Default: "-", Desc: i18n.T("play name (identifies this batch of tasks in output and deployment records)", "play 名称（输出与部署记录里标识这批任务）"), GoField: "Name"},
				{Name: "hosts", Type: "string", Default: "all", Desc: i18n.T("host selection mode: group name / host name list (comma-separated) / all; the chart convention is a group named after the application", "主机选择模式：组名 / 主机名列表（逗号分隔）/ all；chart 惯例是与应用同名的组"), GoField: "Hosts"},
			},
			Example: i18n.T(`
- name: deploy web
  hosts: webservers      # a group name, or web1,web2 / all
`, `
- name: 部署 web
  hosts: webservers      # 组名，或 web1,web2 / all
`),
		},
		{
			Title: i18n.T("Variables and execution environment", "变量与执行环境"),
			Fields: []model.FieldDoc{
				{Name: "vars", Type: "map", Default: "-", Desc: i18n.T("play-level variables (visible to every task in this play; values may be templates)", "play 级变量（本 play 全部任务可见，值支持模板）"), GoField: "Vars"},
				{Name: "environment", Type: "map", Default: "-", Desc: i18n.T("play-level environment variables (task-level can override)", "play 级环境变量（任务级可覆盖）"), GoField: "Environment"},
				{Name: "become", Type: "bool", Default: "false", Desc: i18n.T("privilege escalation for the whole batch (lenient yes/no/on/off)", "整批任务提权（宽容 yes/no/on/off）"), GoField: "Become"},
				{Name: "become_user", Type: "string", Default: "root", Desc: i18n.T("target user for privilege escalation", "提权目标用户"), GoField: "BecomeUser"},
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
			Title: i18n.T("Batching and strategy", "批次与策略"),
			Fields: []model.FieldDoc{
				{Name: "serial", Type: "string | int | list", Default: i18n.T("one batch", "一批"), Desc: i18n.T(`batch size: "5" / "10%" / "5,10,20" (the last number repeats to the end)`, `分批大小："5" / "10%" / "5,10,20"（最后一个数字重复用到底）`), GoField: "Serial"},
				{Name: "strategy", Type: "map", Default: "-", Desc: i18n.T("deployment strategy: type (linear|rolling|canary) + batch + gate (health gate between batches) + auto_rollback (automatic rollback on failure)", "部署策略：type（linear|rolling|canary）+ batch + gate（批间健康门）+ auto_rollback（失败自动回滚）"), GoField: "Strategy"},
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
			Title: i18n.T("Task lists", "任务列表"),
			Fields: []model.FieldDoc{
				{Name: "tasks", Type: "list<task>", Default: "-", Desc: i18n.T("main task list (executed in order, single-key map module syntax)", "主任务列表（按序执行，单键 map 模块语法）"), GoField: "Tasks"},
				{Name: "handlers", Type: "list<task>", Default: "-", Desc: i18n.T("handler list: triggered by notify and flushed once at the end of the play. Convention: keep them together in a handlers/ directory (a fragment is a bare task list, one handler per entry) and pull it in with `- include: handlers/x.yaml`", "处理器列表：notify 触发，play 末尾统一 flush。约定集中放 handlers/ 目录（片段 = 裸任务列表，每项一个 handler），play 里 `- include: handlers/x.yaml` 引入"), GoField: "Handlers"},
			},
			Example: i18n.T(`
- hosts: webservers
  tasks:
    - name: push the configuration
      template: {src: app.conf.tpl, dest: /etc/app/app.conf}
      notify: reload app
  # handler convention directory: handlers/reload.yaml (a fragment is a bare task list),
  # pulled in with include from the play; a few handlers can also be inlined
  handlers:
    - include: handlers/reload.yaml
    - name: inline fallback
      shell: 'systemctl reload app'
`, `
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
`),
		},
	}
}
