package model

// Play 是 playbook 中的一个执行单元：选定一批主机，按顺序执行任务列表。
type Play struct {
	Name        string            // play 名称（可选）
	Hosts       string            // 主机选择模式，如 all / webservers / web1,web2
	Vars        map[string]any    // play 级变量
	Environment map[string]string // play 级环境变量
	Become      bool              // 是否提权
	BecomeUser  string            // 提权目标用户，缺省 root
	Serial      string            // 分批大小："5"（绝对数）/"10%"（百分比）/"5,10,20"（逐批尺寸，最后一个重复），空 = 一批
	Strategy    *Strategy         // 部署策略（nil = 传统线性语义）
	Tasks       []*Task           // 主任务列表
	Handlers    []*Task           // 处理器（notify 触发，play 末尾 flush）
}

// Strategy 是 play 级部署策略：分批节奏 + 批间健康门 + 失败自动回滚。
//
//	type: rolling        # linear | rolling | canary
//	batch: "10%"         # 每批主机数（百分比或绝对数，缺省 25%）
//	gate:                # 批次完成且 handlers flush 后的健康门
//	  shell: 'curl -sf http://localhost:8080/health'
//	  until: '{{ if eq .result.rc 0 }}ok{{ end }}'   # 缺省即 rc==0
//	  retries: 10
//	  delay: 3
//	auto_rollback: true  # 批次失败或门未过 → 回滚该批变更（文件快照恢复）
type Strategy struct {
	Type         string // linear | rolling | canary
	Batch        string // 每批大小："10%" 或 "3"（空 = 25%）
	Gate         *Task  // 健康门（shell 任务，复用 until 轮询机制）
	AutoRollback bool   // 失败自动回滚（文件类变更快照恢复）
}

// Task 是单个任务。模块名作为 YAML key（Ansible 风格），其余为控制属性。
type Task struct {
	Name      string         // 任务名（可选，缺省用模块名）
	Module    string         // 模块名，如 shell / copy；chart 引用时为 "chart"
	ChartRef  string         // 子 chart 引用名（`chart: jdk`），非空时展开为子 chart 任务序列
	ChartVars map[string]any // chart 引用处附加注入的变量（优先级高于子树值）
	Args      map[string]any // 模块参数
	FreeForm  string         // 简写形式的模块参数，如 `shell: uptime` 中的 "uptime"

	When         []string          // 条件（模板表达式，多条件 AND）
	Loop         []any             // 循环项，执行时注入 item 变量
	Register     string            // 结果注册到的变量名
	Notify       []string          // 触发的 handler 名称
	Tags         []string          // 标签
	Environment  map[string]string // 任务级环境变量（覆盖 play 级）
	IgnoreErrors bool              // 失败不中断该主机后续任务
	Retries      int               // 失败重试次数（0 = 不重试）
	DelaySec     int               // 重试间隔秒数
	TimeoutSec   int               // 任务超时秒数（0 = 用全局默认，-1 = 不限）

	Become     *bool  // 任务级提权覆盖（nil = 继承 play）
	BecomeUser string // 提权目标用户

	ChangedWhen string // 模板表达式，覆盖 changed 判定
	FailedWhen  string // 模板表达式，覆盖 failed 判定
	Until       string // until 轮询条件（.result 引用本轮结果），满足即停

	// 展示与编排扩展
	Output     string // 展示控制：full|none|oneline|head=N|tail=N（只控展示不控数据）
	NoLog      bool   // 等价 output=none：stdout/stderr/msg 不回显（register 数据不受影响）
	DelegateTo string // 委托执行：任务在指定主机（或 localhost）上执行，结果归属当前主机
	RunOnce    bool   // 整批只在一台主机执行，结果复制到全部主机
	LoopVar    string // 自定义循环变量名（loop_control.loop_var，缺省 item；嵌套 loop 必需）
	Hook       string // 生命周期钩子：pre_install|post_install|pre_uninstall|post_uninstall

	Block  []*Task // block 组：顺序执行，失败转 rescue
	Rescue []*Task // rescue 组：block 失败时执行
	Always []*Task // always 组：恒执行

	IsHandler bool // 标记该 Task 是 handler（解析时使用）
}

// Label 返回任务展示名。
func (t *Task) Label() string {
	if t.Name != "" {
		return t.Name
	}
	return t.Module
}
