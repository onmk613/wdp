// Package module 实现内置模块。模块只依赖 connection 原语，
// 在控制端编排（exec/upload/download），因此 SSH 与 agent 行为完全一致。
//
// 本文件是模块框架的契约与注册表；执行上下文（runctx.go）、自动回滚
// （rollback.go）、参数解析与校验（args.go）与各模块实现分文件存放。
package module

import (
	"fmt"
	"slices"

	"wdp/internal/model"
)

// Result 是模块执行结果。
type Result struct {
	Changed bool
	Failed  bool
	Skipped bool // 模块主动跳过（如 check 模式下未声明的脚本模块）
	Msg     string
	Stdout  string
	Stderr  string
	Rc      int
	Facts   map[string]any // 非空时并入主机变量域与 fact store（setup/set_fact）
	Groups  []string       // 动态组（group_by）：executor 聚合进 inventory，下一批次/play 生效
	// AddHost 运行期新增/更新主机（add_host）：executor 聚合进 inventory，
	// 后续 play 的 hosts 选择与 groups/hosts/hostvars 内置变量即刻可见。
	AddHost *HostAddition
	Diff    string // --diff 模式：内容级差异（unified diff 或属性前后对照）
}

// HostAddition 是 add_host 声明的新主机（executor 聚合进 inventory）。
type HostAddition struct {
	Host   *model.Host
	Groups []string // 同时加入的组（可空）
}

// Fail 快速构造失败结果。
func Fail(format string, a ...any) *Result {
	return &Result{Failed: true, Msg: fmt.Sprintf(format, a...)}
}

// Module 是模块接口。实现必须无状态（全局单例注册）。
type Module interface {
	Name() string
	Desc() string
	Run(rc *RunContext, args map[string]any, free string) *Result
}

// ParamDoc 描述模块参数（模块文档与骨架片段生成的单一事实来源）。
type ParamDoc struct {
	Name    string `json:"name"`
	Type    string `json:"type"` // string / list / bool / int / mode / map
	Default string `json:"default,omitempty"`
	Desc    string `json:"desc"`
}

// UsageProvider 模块可选实现：参数自描述 + 示例任务 YAML。
// 实现后自动进入模块文档与骨架片段生成链路。
type UsageProvider interface {
	Params() []ParamDoc
	Example() string
}

// Usage 返回模块参数文档（未实现 UsageProvider 时返回 nil）。
func Usage(m Module) []ParamDoc {
	if up, ok := m.(UsageProvider); ok {
		return up.Params()
	}
	return nil
}

// Example 返回模块示例任务（未实现时返回空串）。
func Example(m Module) string {
	if up, ok := m.(UsageProvider); ok {
		return up.Example()
	}
	return ""
}

// RollbackCapability 描述模块变更的自动回滚能力。
type RollbackCapability int

const (
	RollbackNone    RollbackCapability = iota // 无自动回滚（shell/package/service 等过程性变更）
	RollbackPartial                           // 部分可回滚（unarchive：仅删除新建目录，覆盖已有文件不恢复）
	RollbackFull                              // 全量可回滚（copy/template/file：快照恢复 + absent 卸载）
)

// RollbackProvider 模块可选实现：声明变更的自动回滚能力。
// 供 chart 可逆性评估使用——取代跨包硬编码模块名单，新增模块声明能力后即被自动归类。
type RollbackProvider interface {
	RollbackCapability() RollbackCapability
}

// ReadOnlyProvider 模块可选实现：声明模块不产生目标机变更（如 setup）。
type ReadOnlyProvider interface {
	ReadOnly() bool
}

// RollbackCapabilityOf 查询模块回滚能力（未实现 RollbackProvider 或未知模块视为 RollbackNone）。
func RollbackCapabilityOf(name string) RollbackCapability {
	m, ok := Get(name)
	if !ok {
		return RollbackNone
	}
	if rp, ok := m.(RollbackProvider); ok {
		return rp.RollbackCapability()
	}
	return RollbackNone
}

// IsReadOnlyModule 查询模块只读性（未实现 ReadOnlyProvider 视为非只读）。
func IsReadOnlyModule(name string) bool {
	m, ok := Get(name)
	if !ok {
		return false
	}
	if rp, ok := m.(ReadOnlyProvider); ok {
		return rp.ReadOnly()
	}
	return false
}

var registry = map[string]Module{}

// Register 注册模块（各实现文件 init 调用）。
func Register(m Module) {
	registry[m.Name()] = m
}

// Get 按名查找模块。
func Get(name string) (Module, bool) {
	m, ok := registry[name]
	return m, ok
}

// Resolve 是模块名解析的唯一规则（executor 与 chart lint 共用）：
// 内置注册表优先；未命中时回退 chart 本地脚本模块（modules/<名> 可执行文件）。
// 返回（内置模块或 nil、脚本模块路径、是否可解析）。
func Resolve(name string, scriptDirs []string) (mod Module, scriptPath string, ok bool) {
	if m, found := Get(name); found {
		return m, "", true
	}
	if p := FindScriptModule(scriptDirs, name); p != "" {
		return nil, p, true
	}
	return nil, "", false
}

// Names 返回全部模块名（有序）。
func Names() []string {
	out := make([]string, 0, len(registry))
	for n := range registry {
		out = append(out, n)
	}
	slices.Sort(out)
	return out
}
