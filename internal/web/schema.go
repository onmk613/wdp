package web

// 编辑器元数据端点：任务/play/chart 字段文档表（来自各解析包的单一事实
// 来源，与 `wdp schema` CLI 同源）+ 模板补全候选（内置变量、sprig 白名单
// 函数）。IDE 的 YAML 补全/悬停文档/结构校验全部由此驱动——新增控制键或
// 模块后编辑器自动跟上，无需前端改动。

import (
	"net/http"

	"wdp/internal/chart"
	"wdp/internal/executor"
	"wdp/internal/model"
	"wdp/internal/playbook"
	"wdp/internal/render"
)

// schemaResp 是 GET /api/schema 的返回体（FieldSection/FieldDoc 无 json
// tag，按导出名序列化：Title/Fields/Example、Name/Type/Default/Desc）。
type schemaResp struct {
	Task          []model.FieldSection `json:"task"`           // 任务控制键字段分组表
	Play          []model.FieldSection `json:"play"`           // play 级字段分组表
	Chart         []model.FieldSection `json:"chart"`          // chart.yaml 字段分组表
	BuiltinVars   []string             `json:"builtin_vars"`   // 模板内置变量（inventory_hostname 等）
	TemplateFuncs []string             `json:"template_funcs"` // 模板函数白名单（sprig 子集 + 自有）
}

// handleSchema 返回编辑器所需的全部结构元数据。
func (s *Server) handleSchema(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, schemaResp{
		Task:          playbook.TaskFieldSections(),
		Play:          playbook.PlayFieldSections(),
		Chart:         chart.ChartFieldSections(),
		BuiltinVars:   executor.BuiltinVarNames(),
		TemplateFuncs: render.AllowlistFuncs(),
	})
}
