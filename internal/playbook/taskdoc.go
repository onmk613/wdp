package playbook

// 任务控制属性字段表（`wdp schema task` 数据源）。与 taskKeys 同包放置：
// 文档长在解析器旁边，TestTaskFieldsReconcile 双向对账防漂移——新增
// 控制键不写文档、或文档捏造不存在的键，测试都会失败。

import "wdp/internal/model"

// TaskFieldSections 返回任务控制属性的字段分组表。
func TaskFieldSections() []model.FieldSection {
	return []model.FieldSection{
		{
			Title: "任务标识与模块调用",
			Fields: []model.FieldDoc{
				{Name: "name", Type: "string", Default: "模块名", Desc: "任务展示名（结果输出与错误信息用它标识）", GoField: "Name"},
				{Name: "args", Type: "map", Default: "-", Desc: "模块参数的显式写法；与简写（free-form 值）互斥", GoField: "Args"},
				{Name: "chart", Type: "模块键", Default: "-", Desc: "`chart: <子chart名>`：引用子 chart，展开为该相位的任务序列（写在模块位置，不是控制键）", GoField: "ChartRef"},
				{Name: "include", Type: "模块键", Default: "-", Desc: "`include: tasks/x.yaml`：引入任务片段，Load 期静态展开（写在模块位置）", GoField: "Include"},
			},
			Example: `
- name: 部署配置
  template: {src: app.conf.tpl, dest: /etc/app/app.conf}   # 模块键 + 简写/map 参数
- name: 引用子 chart
  chart: jdk
  vars: {version: "17"}          # 仅 chart 引用任务可用
- name: 引入片段
  include: tasks/web.yaml
`,
		},
		{
			Title: "条件与循环（执行流）",
			Fields: []model.FieldDoc{
				{Name: "when", Type: "string | list", Default: "-", Desc: "条件（Go 模板表达式，渲染结果经 Truthy 判定：空/false/0/no/off/[]/{}/nil 为假）；列表为多条件 AND，任一为假跳过", GoField: "When"},
				{Name: "loop", Type: "list", Default: "-", Desc: "循环项，逐项执行并注入 item 变量；单个模板元素渲染结果为列表语法时展开为多项", GoField: "Loop"},
				{Name: "with_items", Type: "list", Default: "-", Desc: "loop 的兼容别名（两者同时给出时 loop 优先）", GoField: "Loop"},
				{Name: "loop_control", Type: "map", Default: "loop_var: item", Desc: "循环控制；loop_var 自定义循环变量名（嵌套循环必需）", GoField: "LoopVar"},
				{Name: "until", Type: "string", Default: "-", Desc: "轮询条件（模板表达式，.result 引用本轮结果），满足即停；默认最多 3 次尝试", GoField: "Until"},
				{Name: "retries", Type: "int", Default: "0", Desc: "重试次数：until 轮询的最大额外尝试数（未设 until 时为失败重试，0 = 不重试）", GoField: "Retries"},
				{Name: "delay", Type: "int", Default: "0", Desc: "重试/轮询间隔秒数（0 不等待）", GoField: "DelaySec"},
				{Name: "timeout", Type: "int", Default: "全局任务超时", Desc: "任务超时秒数（0 = 用 wdp.cfg task-timeout，-1 = 不限）", GoField: "TimeoutSec"},
			},
			Example: `
- name: 只在生产且实例数达标时执行
  shell: 'echo ok'
  when:
    - '{{ eq .env "prod" }}'
    - '{{ if ge .app.replicas 2 }}yes{{ end }}'
- name: 逐实例部署
  shell: 'echo instance {{ .item }}'
  loop: ["1", "2"]
  register: inst        # loop 的 register 含 results 逐项数组
`,
		},
		{
			Title: "执行上下文",
			Fields: []model.FieldDoc{
				{Name: "environment", Type: "map", Default: "-", Desc: "任务级环境变量（值支持模板；覆盖 play 级 environment）", GoField: "Environment"},
				{Name: "become", Type: "bool", Default: "继承 play", Desc: "任务级提权覆盖", GoField: "Become"},
				{Name: "become_user", Type: "string", Default: "root", Desc: "提权目标用户", GoField: "BecomeUser"},
				{Name: "delegate_to", Type: "string", Default: "-", Desc: "委托执行：任务改在指定主机（或 localhost）上执行，变量域保持原主机，结果归属原主机", GoField: "DelegateTo"},
				{Name: "run_once", Type: "bool", Default: "false", Desc: "整批只在一台主机执行，结果复制到全部主机", GoField: "RunOnce"},
				{Name: "ignore_errors", Type: "bool", Default: "false", Desc: "失败不中断该主机后续任务（结果仍标记 failed）", GoField: "IgnoreErrors"},
				{Name: "hook", Type: "string", Default: "-", Desc: "生命周期钩子 pre_<phase>/post_<phase>，在该相位 play 的 pre/post 时机执行（deploy 相位沿用 install 词干）", GoField: "Hook"},
			},
			Example: `
- name: 在负载均衡上摘除节点（委托，变量仍是本机）
  shell: 'lbctl remove {{ .inventory_hostname }}'
  delegate_to: lbtask
- name: 全批只做一次的准备工作
  shell: 'prepare-cluster'
  run_once: true
`,
		},
		{
			Title: "结果与呈现",
			Fields: []model.FieldDoc{
				{Name: "register", Type: "string", Default: "-", Desc: "把结果注册为变量（stdout/rc/changed/failed/skipped 等字段），后续任务与 when 可引用；跳过的任务也注册 skipped 形态", GoField: "Register"},
				{Name: "notify", Type: "string | list", Default: "-", Desc: "任务 changed 且未失败时触发的 handler 名（play 末尾统一 flush）", GoField: "Notify"},
				{Name: "tags", Type: "string | list", Default: "-", Desc: "标签：--tags 只跑 / --skip-tags 跳过", GoField: "Tags"},
				{Name: "changed_when", Type: "string", Default: "-", Desc: "模板表达式，覆盖 changed 判定（如让 shell 的特定输出算无变更）", GoField: "ChangedWhen"},
				{Name: "failed_when", Type: "string", Default: "-", Desc: "模板表达式，覆盖 failed 判定（.result 引用结果）", GoField: "FailedWhen"},
				{Name: "output", Type: "string", Default: "full", Desc: "输出控制：full | none | oneline | head=N | tail=N（只控展示不控 register 数据）", GoField: "Output"},
				{Name: "no_log", Type: "bool", Default: "false", Desc: "等价 output=none：stdout/stderr/msg 不回显（register 数据不受影响），敏感命令用", GoField: "NoLog"},
			},
			Example: `
- name: 健康检查直到通过
  shell: 'curl -sf http://localhost:8080/health'
  register: health
  until: '{{ eq .result.rc 0 }}'
  retries: 10
  delay: 3
  changed_when: '{{ eq .result.rc 0 }}'   # 探测不算变更，不触发 handler
  no_log: false
`,
		},
		{
			Title: "chart 引用专属",
			Fields: []model.FieldDoc{
				{Name: "vars", Type: "map", Default: "-", Desc: "chart 引用处注入子 chart 的变量（优先级高于子树同名值）；仅 `chart:` 引用任务可用", GoField: "ChartVars"},
				{Name: "tasks_from", Type: "string", Default: "deploy", Desc: "chart 引用的入口相位名（执行子 chart 的 <phase>.yaml）；仅 `chart:` 引用任务可用", GoField: "TasksFrom"},
			},
			Example: `
- name: 用指定版本部署 jdk 子 chart 的安装相位
  chart: jdk
  vars: {version: "17", mirror: "https://mirror.internal"}
  tasks_from: install
`,
		},
		{
			Title: "任务组（容错块）",
			Fields: []model.FieldDoc{
				{Name: "block", Type: "list<task>", Default: "-", Desc: "顺序执行的任务组；组内失败转 rescue（组级 when/ignore_errors 等对整组生效）", GoField: "Block"},
				{Name: "rescue", Type: "list<task>", Default: "-", Desc: "block 失败时执行（须与 block 同现）", GoField: "Rescue"},
				{Name: "always", Type: "list<task>", Default: "-", Desc: "无论成败恒执行（须与 block 同现）", GoField: "Always"},
			},
			Example: `
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
`,
		},
	}
}
