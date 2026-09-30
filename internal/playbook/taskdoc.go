package playbook

// 任务控制属性字段表（`wdp schema task` 数据源）。与 taskKeys 同包放置：
// 文档长在解析器旁边，TestTaskFieldsReconcile 双向对账防漂移——新增
// 控制键不写文档、或文档捏造不存在的键，测试都会失败。

import (
	"wdp/internal/i18n"
	"wdp/internal/model"
)

// TaskFieldSections 返回任务控制属性的字段分组表。
func TaskFieldSections() []model.FieldSection {
	return []model.FieldSection{
		{
			Title: i18n.T("Task identity and module invocation", "任务标识与模块调用"),
			Fields: []model.FieldDoc{
				{Name: "name", Type: "string", Default: i18n.T("module name", "模块名"), Desc: i18n.T("task display name (identifies it in result output and error messages)", "任务展示名（结果输出与错误信息用它标识）"), GoField: "Name"},
				{Name: "args", Type: "map", Default: "-", Desc: i18n.T("explicit form of module parameters; mutually exclusive with the shorthand (free-form value)", "模块参数的显式写法；与简写（free-form 值）互斥"), GoField: "Args"},
				{Name: "chart", Type: "模块键", Default: "-", Desc: i18n.T("the only map form: `chart: {name: <reference name>, values: …, values_from: …, hosts: …, phase: …}` — a chart reference expands into a task sequence and interleaves with ordinary module tasks in order. A chart references sub-charts under charts/; a bare playbook references the sibling charts/ directory (written in the module position, not as a control key)", "`chart: {name: <引用名>, values: …, values_from: …, hosts: …, phase: …}` map 形态（唯一）：引用 chart 展开为任务序列，与普通模块任务任意混排按序执行。chart 内引用 charts/ 子 chart；裸 playbook 引用同级 charts/ 目录（写在模块位置，不是控制键）"), GoField: "ChartRef"},
				{Name: "values", Type: "map", Default: "-", Desc: i18n.T("chart references only: inline variable overrides (synonym for vars, highest priority; in chart mode they layer on top of the parent scope's <reference name> subtree/global)", "仅 chart 引用：内联变量覆盖（与 vars 同义，最高优先级；chart 模式叠加在父作用域 <引用名> 子树/global 之上）"), GoField: "ChartVars"},
				{Name: "values_from", Type: "string | list", Default: "-", Desc: i18n.T("chart references only: values override files (relative to the playbook/chart root, merged in order; priority above the scope subtree and below inline values)", "仅 chart 引用：values 覆盖文件（相对 playbook/chart 根目录，依序合并；优先级在作用域子树之上、内联 values 之下）"), GoField: "ChartValuesFrom"},
				{Name: "hosts", Type: "string", Default: i18n.T("current play batch", "当前 play 批次"), Desc: i18n.T("chart references only: host filter selector — intersected with the current play batch, and hosts outside the set skip this reference (no host re-selection across plays; batch/serial/rollback semantics stay at play level)", "仅 chart 引用：主机过滤选择器——当前 play 批次与该选择器取交集，不在集合的主机跳过该引用（不跨 play 重选主机，批次/串行/回滚语义保持在 play 级）"), GoField: "ChartHosts"},
				{Name: "phase", Type: "string", Default: "deploy", Desc: i18n.T("chart references only: entry phase name (synonym for tasks_from; uninstall/status/custom phase)", "仅 chart 引用：入口相位名（与 tasks_from 同义；uninstall/status/自定义相位）"), GoField: "TasksFrom"},
				{Name: "include", Type: "模块键", Default: "-", Desc: i18n.T("`include: tasks/x.yaml`: pulls in a task fragment, statically expanded at load time (written in the module position)", "`include: tasks/x.yaml`：引入任务片段，Load 期静态展开（写在模块位置）"), GoField: "Include"},
			},
			Example: i18n.T(`
- name: deploy configuration
  template: {src: app.conf.tpl, dest: /etc/app/app.conf}   # module key + shorthand/map parameters
- name: reference a chart (the only map form)
  chart:
    name: jdk
    values: {version: "17"}         # inline overrides (synonym for vars)
- name: reference a chart (full configuration in one place)
  chart:
    name: nginx
    values_from: [values/prod.yaml]  # values override file (relative to the playbook directory)
    hosts: webservers               # within the current batch, only the web group
    phase: deploy                   # entry phase (default deploy)
- shell: systemctl is-active nginx  # ordinary tasks and chart references interleave freely
- name: pull in a fragment
  include: tasks/web.yaml
`, `
- name: 部署配置
  template: {src: app.conf.tpl, dest: /etc/app/app.conf}   # 模块键 + 简写/map 参数
- name: 引用 chart（唯一 map 形态）
  chart:
    name: jdk
    values: {version: "17"}         # 内联覆盖（与 vars 同义）
- name: 引用 chart（完整配置集中）
  chart:
    name: nginx
    values_from: [values/prod.yaml]  # values 覆盖文件（相对 playbook 目录）
    hosts: webservers               # 当前批次内只对 web 组生效
    phase: deploy                   # 入口相位（缺省 deploy）
- shell: systemctl is-active nginx  # 普通任务与 chart 引用任意混排
- name: 引入片段
  include: tasks/web.yaml
`),
		},
		{
			Title: i18n.T("Conditions and loops (execution flow)", "条件与循环（执行流）"),
			Fields: []model.FieldDoc{
				{Name: "when", Type: "string | list", Default: "-", Desc: i18n.T("condition (a Go template expression; the rendered result goes through Truthy: empty/false/0/no/off/[]/{}/nil are false); a list is ANDed across conditions, and any false entry skips the task", "条件（Go 模板表达式，渲染结果经 Truthy 判定：空/false/0/no/off/[]/{}/nil 为假）；列表为多条件 AND，任一为假跳过"), GoField: "When"},
				{Name: "loop", Type: "list", Default: "-", Desc: i18n.T("loop items, executed one by one with the item variable injected; a single template element that renders to list syntax expands into multiple items", "循环项，逐项执行并注入 item 变量；单个模板元素渲染结果为列表语法时展开为多项"), GoField: "Loop"},
				{Name: "loop_control", Type: "map", Default: "loop_var: item", Desc: i18n.T("loop control; loop_var sets a custom loop variable name (required for nested loops)", "循环控制；loop_var 自定义循环变量名（嵌套循环必需）"), GoField: "LoopVar"},
				{Name: "until", Type: "string", Default: "-", Desc: i18n.T("polling condition (a template expression, with .result referring to the current attempt's result) that stops as soon as it holds; at most 3 attempts by default", "轮询条件（模板表达式，.result 引用本轮结果），满足即停；默认最多 3 次尝试"), GoField: "Until"},
				{Name: "retries", Type: "int", Default: "0", Desc: i18n.T("retry count: maximum extra attempts for until polling (without until, retries on failure; 0 = no retry)", "重试次数：until 轮询的最大额外尝试数（未设 until 时为失败重试，0 = 不重试）"), GoField: "Retries"},
				{Name: "delay", Type: "int", Default: "0", Desc: i18n.T("interval in seconds between retries/polls (0 = no wait)", "重试/轮询间隔秒数（0 不等待）"), GoField: "DelaySec"},
				{Name: "timeout", Type: "int", Default: i18n.T("global task timeout", "全局任务超时"), Desc: i18n.T("task timeout in seconds (0 = use wdp.cfg task-timeout, -1 = unlimited)", "任务超时秒数（0 = 用 wdp.cfg task-timeout，-1 = 不限）"), GoField: "TimeoutSec"},
			},
			Example: i18n.T(`
- name: run only in production and with enough instances
  shell: 'echo ok'
  when:
    - '{{ eq .env "prod" }}'
    - '{{ if ge .app.replicas 2 }}yes{{ end }}'
- name: deploy instance by instance
  shell: 'echo instance {{ .item }}'
  loop: ["1", "2"]
  register: inst        # register on a loop holds a per-item results array
`, `
- name: 只在生产且实例数达标时执行
  shell: 'echo ok'
  when:
    - '{{ eq .env "prod" }}'
    - '{{ if ge .app.replicas 2 }}yes{{ end }}'
- name: 逐实例部署
  shell: 'echo instance {{ .item }}'
  loop: ["1", "2"]
  register: inst        # loop 的 register 含 results 逐项数组
`),
		},
		{
			Title: i18n.T("Execution context", "执行上下文"),
			Fields: []model.FieldDoc{
				{Name: "environment", Type: "map", Default: "-", Desc: i18n.T("task-level environment variables (values may be templates; overrides the play-level environment)", "任务级环境变量（值支持模板；覆盖 play 级 environment）"), GoField: "Environment"},
				{Name: "become", Type: "bool", Default: i18n.T("inherits play", "继承 play"), Desc: i18n.T("task-level privilege escalation override", "任务级提权覆盖"), GoField: "Become"},
				{Name: "become_user", Type: "string", Default: "root", Desc: i18n.T("target user for privilege escalation", "提权目标用户"), GoField: "BecomeUser"},
				{Name: "delegate_to", Type: "string", Default: "-", Desc: i18n.T("delegated execution: the task runs on the given host (or localhost), while the variable scope and result ownership stay with the original host", "委托执行：任务改在指定主机（或 localhost）上执行，变量域保持原主机，结果归属原主机"), GoField: "DelegateTo"},
				{Name: "run_once", Type: "bool", Default: "false", Desc: i18n.T("run on one host only for the whole batch, with the result copied to every host", "整批只在一台主机执行，结果复制到全部主机"), GoField: "RunOnce"},
				{Name: "ignore_errors", Type: "bool", Default: "false", Desc: i18n.T("a failure does not stop later tasks on that host (the result is still marked failed)", "失败不中断该主机后续任务（结果仍标记 failed）"), GoField: "IgnoreErrors"},
				{Name: "hook", Type: "string", Default: "-", Desc: i18n.T("lifecycle hook pre_<phase>/post_<phase>, run at the pre/post point of the play for that phase (the deploy phase uses the install stem)", "生命周期钩子 pre_<phase>/post_<phase>，在该相位 play 的 pre/post 时机执行（deploy 相位沿用 install 词干）"), GoField: "Hook"},
			},
			Example: i18n.T(`
- name: drain the node on the load balancer (delegated; variables stay with this host)
  shell: 'lbctl remove {{ .inventory_hostname }}'
  delegate_to: lbtask
- name: one-time preparation for the whole batch
  shell: 'prepare-cluster'
  run_once: true
`, `
- name: 在负载均衡上摘除节点（委托，变量仍是本机）
  shell: 'lbctl remove {{ .inventory_hostname }}'
  delegate_to: lbtask
- name: 全批只做一次的准备工作
  shell: 'prepare-cluster'
  run_once: true
`),
		},
		{
			Title: i18n.T("Results and presentation", "结果与呈现"),
			Fields: []model.FieldDoc{
				{Name: "register", Type: "string", Default: "-", Desc: i18n.T("registers the result as a variable (fields such as stdout/rc/changed/failed/skipped) that later tasks and when can reference; skipped tasks register a skipped form too", "把结果注册为变量（stdout/rc/changed/failed/skipped 等字段），后续任务与 when 可引用；跳过的任务也注册 skipped 形态"), GoField: "Register"},
				{Name: "notify", Type: "string | list", Default: "-", Desc: i18n.T("name of the handler triggered when the task is changed and not failed (flushed once at the end of the play)", "任务 changed 且未失败时触发的 handler 名（play 末尾统一 flush）"), GoField: "Notify"},
				{Name: "tags", Type: "string | list", Default: "-", Desc: i18n.T("tags: --tags runs these only / --skip-tags skips them", "标签：--tags 只跑 / --skip-tags 跳过"), GoField: "Tags"},
				{Name: "changed_when", Type: "string", Default: "-", Desc: i18n.T("template expression overriding the changed verdict (e.g. treat a particular shell output as no change)", "模板表达式，覆盖 changed 判定（如让 shell 的特定输出算无变更）"), GoField: "ChangedWhen"},
				{Name: "failed_when", Type: "string", Default: "-", Desc: i18n.T("template expression overriding the failed verdict (.result refers to the result)", "模板表达式，覆盖 failed 判定（.result 引用结果）"), GoField: "FailedWhen"},
				{Name: "output", Type: "string", Default: "full", Desc: i18n.T("output control: full | none | oneline | head=N | tail=N (affects display only, not registered data)", "输出控制：full | none | oneline | head=N | tail=N（只控展示不控 register 数据）"), GoField: "Output"},
				{Name: "no_log", Type: "bool", Default: "false", Desc: i18n.T("equivalent to output=none: stdout/stderr/msg are not echoed (registered data is unaffected); use it for sensitive commands", "等价 output=none：stdout/stderr/msg 不回显（register 数据不受影响），敏感命令用"), GoField: "NoLog"},
			},
			Example: i18n.T(`
- name: health check until it passes
  shell: 'curl -sf http://localhost:8080/health'
  register: health
  until: '{{ eq .result.rc 0 }}'
  retries: 10
  delay: 3
  changed_when: '{{ eq .result.rc 0 }}'   # a probe is not a change and triggers no handler
  no_log: false
`, `
- name: 健康检查直到通过
  shell: 'curl -sf http://localhost:8080/health'
  register: health
  until: '{{ eq .result.rc 0 }}'
  retries: 10
  delay: 3
  changed_when: '{{ eq .result.rc 0 }}'   # 探测不算变更，不触发 handler
  no_log: false
`),
		},
		{
			Title: i18n.T("chart reference only", "chart 引用专属"),
			Fields: []model.FieldDoc{
				{Name: "vars", Type: "map", Default: "-", Desc: i18n.T("variables injected into the sub-chart at the chart reference (priority above same-named values in the subtree); available only on `chart:` reference tasks", "chart 引用处注入子 chart 的变量（优先级高于子树同名值）；仅 `chart:` 引用任务可用"), GoField: "ChartVars"},
				{Name: "tasks_from", Type: "string", Default: "deploy", Desc: i18n.T("entry phase name of the chart reference (runs the sub-chart's <phase>.yaml); available only on `chart:` reference tasks", "chart 引用的入口相位名（执行子 chart 的 <phase>.yaml）；仅 `chart:` 引用任务可用"), GoField: "TasksFrom"},
			},
			Example: i18n.T(`
- name: deploy the jdk sub-chart's install phase at a pinned version
  chart: {name: jdk}
  vars: {version: "17", mirror: "https://mirror.internal"}
  tasks_from: install
`, `
- name: 用指定版本部署 jdk 子 chart 的安装相位
  chart: {name: jdk}
  vars: {version: "17", mirror: "https://mirror.internal"}
  tasks_from: install
`),
		},
		{
			Title: i18n.T("Task groups (error-handling blocks)", "任务组（容错块）"),
			Fields: []model.FieldDoc{
				{Name: "block", Type: "list<task>", Default: "-", Desc: i18n.T("a task group executed in order; a failure inside moves to rescue (group-level when/ignore_errors and the like apply to the whole group)", "顺序执行的任务组；组内失败转 rescue（组级 when/ignore_errors 等对整组生效）"), GoField: "Block"},
				{Name: "rescue", Type: "list<task>", Default: "-", Desc: i18n.T("runs when block fails (must appear together with block)", "block 失败时执行（须与 block 同现）"), GoField: "Rescue"},
				{Name: "always", Type: "list<task>", Default: "-", Desc: i18n.T("always runs, success or failure (must appear together with block)", "无论成败恒执行（须与 block 同现）"), GoField: "Always"},
			},
			Example: i18n.T(`
- name: change + verify + fall back
  block:
    - name: push the configuration
      template: {src: app.conf.tpl, dest: /etc/app/app.conf}
      notify: reload app
  rescue:
    - name: roll back on failure
      shell: 'restore-from-backup'
  always:
    - name: clean up temporary files
      file: {path: /tmp/app.staging, state: absent}
`, `
- name: 变更 + 验证 + 兜底
  block:
    - name: 下发配置
      template: {src: app.conf.tpl, dest: /etc/app/app.conf}
      notify: reload app
  rescue:
    - name: 失败时回滚现场
      shell: 'restore-from-backup'
  always:
    - name: 清理临时文件
      file: {path: /tmp/app.staging, state: absent}
`),
		},
	}
}
