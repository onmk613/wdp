package plan

// Meta 是 chart.yaml 的计划快照——chart.Meta 的 JSON 兼容镜像（字段名与
// 序列化字节完全一致：镜像不带 json 标签，与 chart.Meta 的缺省编码对齐，
// PlanID 内容寻址与既有 plan.json 不受影响）。
//
// 独立镜像而非直接引用 chart.Meta，是 plan 作为「冻结契约包」的前提：
// plan 的依赖面收敛到 model 一个内部包（见 deps_test.go 的白名单断言），
// agent 侧只靠本包即可解析计划，chart/playbook 的演进不改变契约面。
// 编译侧（internal/planbuild）负责 chart.Meta → Meta 的字段级转换。
type Meta struct {
	Name        string   `yaml:"name"`
	Version     string   `yaml:"version"`
	Description string   `yaml:"description"`
	Required    []string `yaml:"required"`   // 必须由调用方提供的 values 点路径（缺失即报错）
	MarkerDir   string   `yaml:"marker_dir"` // 目标机 release marker 目录（缺省 /var/lib/wdp）
	NoMarker    bool     `yaml:"no_marker"`  // 不写 release marker
	CheckMode   bool     `yaml:"check_mode"` // 脚本模块支持 check 预演（chart.yaml 显式声明；YAML 的 supported/true 归一在编译侧完成）
	// InventoryOverride 是 values 键白名单：允许被 inventory 组/主机同名
	// 变量直接覆盖（显式 opt-in）。
	InventoryOverride []string `yaml:"inventory_override"`
	// SensitiveValues 是敏感 values 点路径白名单：marker 落盘脱敏，且不
	// 参与 values 摘要（真实敏感值变化不构成 drift 信号）。
	SensitiveValues []string `yaml:"sensitive_values"`
	// Phases 为生命周期相位补充属性声明（键 = 相位名）。
	Phases map[string]PhaseSpec `yaml:"phases"`
}

// PhaseSpec 是相位属性快照（chart.PhaseSpec 的镜像；仅数据，缺省属性
// 合成规则在 chart.MergePhaseSpec——单一实现不复制）。
type PhaseSpec struct {
	Release      bool       `yaml:"release"`       // 部署语义：required 校验、可逆性确认、成功后写 marker、记部署记录
	Record       bool       `yaml:"record"`        // 记部署记录（Release 隐含；uninstall 内置）
	ClearsMarker bool       `yaml:"clears_marker"` // 成功后清除 release marker（uninstall 内置）
	ValuesFrom   ValuesFrom `yaml:"values_from"`   // values 来源（空 = 按相位推导）
}

// ValuesFrom 决定相位读取 values 的来源（chart.ValuesFrom 的镜像）。
type ValuesFrom string

// Records 报告该相位是否写部署记录（与 chart.PhaseSpec 同语义）。
func (s PhaseSpec) Records() bool { return s.Release || s.Record }

// Destructive 报告该相位是否可能破坏现场（与 chart.PhaseSpec 同语义）。
func (s PhaseSpec) Destructive() bool { return s.Release || s.ClearsMarker }
