package web

// 模块自描述元数据端点：编辑器动态表单的唯一数据源。模块在 Go 侧实现
// UsageProvider（Params/Example）后即自动出现在此接口与前端表单——新增
// 模块无需任何前端改动，这就是「预留后续新增 module」的机制。
// 未知模块名（chart 本地脚本模块 modules/<名>）前端仍可用：表单退化为
// YAML 参数框，语法与内置模块完全一致。

import (
	"net/http"

	"wdp/internal/module"
	"wdp/internal/playbook"
)

// moduleInfo 是单个模块的元数据投影。
type moduleInfo struct {
	Name     string            `json:"name"`
	Desc     string            `json:"desc"`
	FreeForm bool              `json:"free_form"` // 支持 `shell: uptime` 简写（params 含 "(free-form)"）
	Params   []module.ParamDoc `json:"params"`
	Example  string            `json:"example,omitempty"`
	Rollback int               `json:"rollback"` // 0=无 1=部分 2=全量（自动回滚能力）
	ReadOnly bool              `json:"read_only"`
}

// handleListModules 返回全部内置模块的参数文档。
func (s *Server) handleListModules(w http.ResponseWriter, _ *http.Request) {
	out := make([]moduleInfo, 0, 32)
	for _, name := range module.Names() {
		m, ok := module.Get(name)
		if !ok {
			continue
		}
		mi := moduleInfo{
			Name: name, Desc: m.Desc(),
			Params:   module.Usage(m),
			Example:  module.Example(m),
			Rollback: int(module.RollbackCapabilityOf(name)),
			ReadOnly: module.IsReadOnlyModule(name),
		}
		for _, p := range mi.Params {
			if p.Name == "(free-form)" {
				mi.FreeForm = true
				break
			}
		}
		out = append(out, mi)
	}
	// chart 引用伪模块：不是注册表内置模块（由执行器展开为任务序列），
	// 但编辑器的补全/文档面板与 CLI `wdp module chart` 同一文档来源
	out = append(out, moduleInfo{
		Name:    "chart",
		Desc:    "引用 chart 展开为任务序列（chart 内 = charts/ 子 chart；裸 playbook = 同级 charts/ 目录）",
		Params:  playbook.ChartRefParams(),
		Example: playbook.ChartRefExample(),
	})
	writeJSON(w, http.StatusOK, out)
}
