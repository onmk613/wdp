package web

// 应用与版本的增删改查（列表/详情/更新/删除/默认版本/作用域/批量操作）。

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	"wdp/internal/store"
)

// ---- 应用 CRUD ----

func (s *Server) handleListApps(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Has("page") || r.URL.Query().Has("page_size") {
		pp := parsePage(r)
		items, total, err := s.st.ListAppsPage(r.URL.Query().Get("q"), pp.Page, pp.PageSize)
		if err != nil {
			s.writeInternal(w, err)
			return
		}
		p := s.permsOf(permUser(r))
		if !p.global[verbAppView] {
			out := items[:0]
			for _, a := range items {
				if p.canApp(verbAppView, a.Pools, a.Groups, a.Labels) {
					out = append(out, a)
				}
			}
			items = out
		}
		writeJSON(w, http.StatusOK, pagedResp[*store.App]{Items: items, Total: total, Page: pp.Page, PageSize: pp.PageSize})
		return
	}
	apps, err := s.st.ListApps()
	if err != nil {
		s.writeInternal(w, err)
		return
	}
	p := s.permsOf(permUser(r))
	if !p.global[verbAppView] {
		out := make([]*store.App, 0, len(apps))
		for _, a := range apps {
			if p.canApp(verbAppView, a.Pools, a.Groups, a.Labels) {
				out = append(out, a)
			}
		}
		apps = out
	}
	writeJSON(w, http.StatusOK, apps)
}

func (s *Server) handleGetApp(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	app, ok := s.checkApp(w, r, verbAppView, id)
	if !ok {
		return
	}
	versions, err := s.st.ListVersions(id)
	if err != nil {
		s.writeInternal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"app": app, "versions": versions})
}

func (s *Server) handleUpdateApp(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	app, ok := s.checkApp(w, r, verbAppEdit, id)
	if !ok {
		return
	}
	var req struct {
		Note   string   `json:"note"`
		Pools  []string `json:"pools"`
		Groups []string `json:"groups"`
		Labels string   `json:"labels"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	// 约束：应用 scope 目标值须全部落在调用者 app:edit 授权内（同编辑器
	// 保存；改 scope 超出自身授权是 app:scope 的权限语义）
	if bad := s.scopeWriteCheck(r, verbAppEdit, req.Pools, req.Groups, labelKeySlice(req.Labels)); bad != "" {
		writeError(w, http.StatusForbidden, "forbidden: "+bad+" outside your app:edit scope (changing scopes beyond your own requires app:scope)")
		return
	}
	if err := s.st.UpdateAppScopes(id, req.Note, req.Pools, req.Groups, req.Labels); err != nil {
		s.writeStoreErr(w, err)
		return
	}
	updated, err := s.st.GetApp(id)
	if err != nil {
		s.writeStoreErr(w, err)
		return
	}
	s.audit(r, "update", "app", app.Name, "描述/池/组/标签")
	writeJSON(w, http.StatusOK, updated)
}

// handleUpdateVersionScope 就地变更版本作用域（不升版本——后期的权限/
// 归属调整）。version 空 = 默认版本；目标恰为默认版本时同步应用级 scope
// （应用级 = 最近保存口径），改旧版本只动该版本行。
func (s *Server) handleUpdateVersionScope(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	app, err := s.st.GetApp(id)
	if err != nil {
		s.writeStoreErr(w, err)
		return
	}
	var req struct {
		Version string   `json:"version"`
		Pools   []string `json:"pools"`
		Groups  []string `json:"groups"`
		Labels  string   `json:"labels"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	version := req.Version
	if version == "" {
		version = app.LatestVersion
	}
	exists, herr := s.st.HasVersion(id, version)
	if herr != nil {
		// 约束：预检出错不得混入 404 判定
		s.writeInternal(w, herr)
		return
	}
	if !exists {
		writeError(w, http.StatusNotFound, fmt.Sprintf("version %s not found", version))
		return
	}
	isLatest, err := s.st.UpdateVersionScopes(id, version, req.Pools, req.Groups, req.Labels)
	if err != nil {
		s.writeStoreErr(w, err)
		return
	}
	if isLatest {
		// 与编辑器保存同口径：动默认版本时应用级一并同步（后续上传继承）
		if err := s.st.UpdateAppScopes(id, app.Note, req.Pools, req.Groups, req.Labels); err != nil {
			s.logger.Warn("sync app scopes failed", "app", app.Name, "err", err)
		}
	}
	s.audit(r, "update", "app", fmt.Sprintf("%s@%s", app.Name, version), "作用域变更（不升版本）")
	s.replyApp(w, id, http.StatusOK)
}

func (s *Server) handleDeleteApp(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	// 约束：应用名必须删前取——DeleteApp 已删行，之后 GetApp 必然
	// ErrNotFound，审计会恒丢应用名
	app, err := s.st.GetApp(id)
	if err != nil {
		s.writeStoreErr(w, err)
		return
	}
	paths, err := s.st.DeleteApp(id)
	if err != nil {
		s.writeStoreErr(w, err)
		return
	}
	for _, p := range paths {
		_ = os.Remove(p)
	}
	s.audit(r, "delete", "app", app.Name, fmt.Sprintf("连同 %d 个版本与制品", len(paths)))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleSetLatestVersion 把某版本设为默认版本（latest）。
func (s *Server) handleSetLatestVersion(w http.ResponseWriter, r *http.Request) {
	id, vid, ok := path2ID(w, r, "id", "vid")
	if !ok {
		return
	}
	app, ok := s.checkApp(w, r, verbAppEdit, id)
	if !ok {
		return
	}
	versions, err := s.st.ListVersions(id)
	if err != nil {
		s.writeInternal(w, err)
		return
	}
	version := ""
	for _, v := range versions {
		if v.ID == vid {
			version = v.Version
			break
		}
	}
	if version == "" {
		writeError(w, http.StatusNotFound, "version not found")
		return
	}
	if err := s.st.SetLatestVersion(id, version); err != nil {
		s.writeStoreErr(w, err)
		return
	}
	updated, err := s.st.GetApp(id)
	if err != nil {
		s.writeStoreErr(w, err)
		return
	}
	s.audit(r, "set_latest", "version", app.Name+"@"+version, "设为默认版本")
	writeJSON(w, http.StatusOK, updated)
}

// handleBatchApps 批量操作应用（当前仅 delete：连同全部版本与制品删除）。
func (s *Server) handleBatchApps(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IDs    []int64 `json:"ids"`
		Action string  `json:"action"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if len(req.IDs) == 0 {
		writeError(w, http.StatusBadRequest, "ids is empty")
		return
	}
	if req.Action != "delete" {
		writeError(w, http.StatusBadRequest, "unknown action "+req.Action)
		return
	}
	type result struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
		OK   bool   `json:"ok"`
		Err  string `json:"error,omitempty"`
	}
	results := make([]result, 0, len(req.IDs))
	okN := 0
	for _, id := range req.IDs {
		app, err := s.st.GetApp(id)
		if err != nil {
			results = append(results, result{ID: id, Err: "app not found"})
			continue
		}
		paths, err := s.st.DeleteApp(id)
		if err != nil {
			results = append(results, result{ID: id, Name: app.Name, Err: err.Error()})
			continue
		}
		for _, p := range paths {
			_ = os.Remove(p)
		}
		_ = os.Remove(filepath.Join(s.appsDir(), app.Name)) // 空目录顺手清理
		okN++
		results = append(results, result{ID: id, Name: app.Name, OK: true})
		s.logger.Info("app deleted", "name", app.Name, "versions", len(paths))
	}
	s.audit(r, "batch_delete", "app", fmt.Sprintf("%d 个", len(req.IDs)), fmt.Sprintf("成功 %d", okN))
	writeJSON(w, http.StatusOK, map[string]any{"results": results, "ok": okN, "failed": len(results) - okN})
}

func (s *Server) handleDeleteVersion(w http.ResponseWriter, r *http.Request) {
	id, vid, ok := path2ID(w, r, "id", "vid")
	if !ok {
		return
	}
	tgz, err := s.st.DeleteVersion(id, vid)
	if err != nil {
		// 约束：DeleteVersion 传播错误（吞错会留下没人重算的旧 latest），
		// 真实失败不得伪装成 404（writeStoreErr 分流）
		s.writeStoreErr(w, err)
		return
	}
	if tgz != "" {
		_ = os.Remove(tgz)
	}
	s.audit(r, "delete", "version", fmt.Sprintf("app#%d 版本#%d（制品已删）", id, vid), "")
	versions, _ := s.st.ListVersions(id)
	writeJSON(w, http.StatusOK, versions)
}
