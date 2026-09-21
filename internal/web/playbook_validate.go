package web

// 裸 playbook 编辑器的校验端点：内容级静态检查（playbook.LintContent——
// 模块名/block 结构/模板 parse-only），不落盘、不进库、不做文件系统检查。
// 与 /api/apps/validate（chart 整体校验）互补：这里只有单文件内容。
// 只读计算，登录即可用（编辑器入口本身由 app:create 权限的路由控制）。

import (
	"net/http"

	"wdp/internal/playbook"
)

// handleValidatePlaybook POST /api/playbook/validate {content}。
// 发现是正常业务结果：200 + issues（语法错也是一条 ERROR，前端进问题面板）。
func (s *Server) handleValidatePlaybook(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Content string `json:"content"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	src := []byte(req.Content)
	issues := []validateIssue{}
	for _, is := range playbook.LintContent(src) {
		issues = append(issues, validateIssue{Level: is.Level, Path: is.Path, Msg: is.Msg})
	}
	writeJSON(w, http.StatusOK, map[string]any{"issues": issues})
}
