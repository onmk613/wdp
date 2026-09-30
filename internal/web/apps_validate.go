package web

// 编辑器整体校验端点：把当前编辑内容物化到临时目录 → chart.Load 结构
// 自检（applySpec 内）→ chart.Lint 全量静态校验（模块名/chart 引用/模板
// 可渲染/values schema/envs/hook/phases 声明对齐）。只读操作：不碰库、
// 不碰制品；校验发现是正常业务结果，以 200 + issues 返回（物化失败——
// 结构/语法错——也归为 issues 的一条 ERROR，前端统一进问题面板）。

import (
	"errors"
	"fmt"
	"net/http"
	"os"

	"wdp/internal/chart"
	"wdp/internal/console"
)

// validateIssue 是一条校验发现（与 chart.LintIssue 对应的 API 形态）。
type validateIssue struct {
	Level string `json:"level"` // ERROR / WARN
	Path  string `json:"path"`  // chart 内相对路径（"" = 整体性问题）
	Msg   string `json:"msg"`
	Line  int    `json:"line,omitempty"` // 相关行号（1 起，0/缺省 = 未知）
}

// handleValidateSpec POST /api/apps/validate。
// body：{app_id, name, …specReq}；app_id>0 = 编辑模式（校验 app:view，
// 底本 = base_version 或最新），否则新建模式（app:create，从空目录开始）。
// 双模式权限语义不同，路由只挂 requireAuth，这里按模式分别把关。
func (s *Server) handleValidateSpec(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AppID int64  `json:"app_id"`
		Name  string `json:"name"`
		specReq
	}
	if !decodeJSONLarge(w, r, &req) {
		return
	}
	name, baseVersion := req.Name, req.BaseVersion
	latestVersion := "" // 编辑模式：库中最新版本（底本非最新 WARN 用）
	if req.AppID > 0 {
		app, ok := s.checkApp(w, r, verbAppView, req.AppID)
		if !ok {
			return
		}
		latestVersion = app.LatestVersion
		if baseVersion == "" {
			baseVersion = app.LatestVersion
		}
		if baseVersion == "" {
			writeError(w, http.StatusBadRequest, "app has no versions to base on")
			return
		}
		name = app.Name
	} else if !s.permsOf(permUser(r)).canVerb(verbAppCreate) {
		writeError(w, http.StatusForbidden, "forbidden: requires "+verbAppCreate)
		return
	}
	// 校验与保存同口径：保存时版本号会被 patch 进 chart.yaml 再提交，
	// 校验请求里 chart.yaml 还是底本版本（已被占用，前端按"下一个可用
	// 版本"发起）——这里做同样的虚拟改写，校验看到的就是保存将产生的
	// 形态。版本对账在保存路径仍是硬约束（ApplySpec）
	console.PatchSpecChartVersion(&req.specReq, req.Version)
	workDir, err := s.apps.PrepareWorkspace(req.AppID, baseVersion, name, &req.specReq)
	if err != nil {
		var be *console.BizError
		if errors.As(err, &be) {
			writeJSON(w, http.StatusOK, map[string]any{"issues": []validateIssue{
				{Level: chart.ERROR, Msg: be.Error()},
			}})
			return
		}
		s.writeStoreErr(w, err)
		return
	}
	defer os.RemoveAll(workDir)

	ch, err := chart.LoadWithLimits(workDir, chart.Limits{})
	if err != nil {
		// applySpec 已做同一加载自检；走到这里说明环境异常而非内容问题
		s.writeInternal(w, fmt.Errorf("chart load failed: %w", err))
		return
	}
	defer ch.Close()
	issues := []validateIssue{}
	for _, is := range chart.Lint(ch, ch.Values) {
		issues = append(issues, validateIssue{Level: is.Level, Path: is.Path, Msg: is.Msg, Line: is.Line})
	}
	// 编辑模式底本非最新：并行编辑会静默分叉（版本化允许旧底本保存，这是
	// 特性），但保存者应当知情——WARN 提示，与 ERROR 门禁不冲突
	if req.AppID > 0 && latestVersion != "" && baseVersion != latestVersion {
		issues = append(issues, validateIssue{
			Level: chart.WARN,
			Msg: fmt.Sprintf("底本 %s 不是最新版本（当前最新 %s）：他人可能已保存更新，建议核对差异后再保存",
				baseVersion, latestVersion),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"issues": issues})
}
