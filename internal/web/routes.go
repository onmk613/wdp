package web

// 路由注册表：全部业务端点的 method+path → handler 绑定，按资源分组。
// 新端点在这里加一行；权限点紧贴注册处（requirePerm）一目了然。

func (s *Server) routes() {
	s.mux.HandleFunc("POST /api/login", s.handleLogin)
	s.mux.HandleFunc("POST /api/logout", s.requireAuth(s.handleLogout))
	s.mux.HandleFunc("GET /api/me", s.requireAuth(s.handleMe))
	// ---- 用户与会话管理（user:manage）----
	s.mux.HandleFunc("GET /api/users", s.requirePerm(verbUserManage, s.handleListUsers))
	s.mux.HandleFunc("POST /api/users", s.requirePerm(verbUserManage, s.handleCreateUser))
	s.mux.HandleFunc("PUT /api/users/{id}", s.requirePerm(verbUserManage, s.handleUpdateUser))
	s.mux.HandleFunc("PUT /api/users/{id}/password", s.requirePerm(verbUserManage, s.handleResetPassword))
	s.mux.HandleFunc("DELETE /api/users/{id}", s.requirePerm(verbUserManage, s.handleDeleteUser))
	s.mux.HandleFunc("PUT /api/users/{id}/scopes", s.requirePerm(verbUserManage, s.handleSetUserScopes))
	s.mux.HandleFunc("GET /api/sessions", s.requirePerm(verbUserManage, s.handleListSessions))
	s.mux.HandleFunc("DELETE /api/sessions/{token}", s.requirePerm(verbUserManage, s.handleKillSession))
	// ---- 运行时设置（admin 专属：单文档，设置页读写）----
	s.mux.HandleFunc("GET /api/settings", s.requireAuth(s.requireAdmin(s.handleGetSettings)))
	s.mux.HandleFunc("PUT /api/settings", s.requireAuth(s.requireAdmin(s.handlePutSettings)))
	// ---- chart 仓库（Helm 兼容；app:view，session/basic 双认证）----
	s.routesChartRepo()
	// ---- 主机 ----
	s.mux.HandleFunc("GET /api/hosts", s.requirePerm(verbHostView, s.handleListHosts))
	s.mux.HandleFunc("POST /api/hosts", s.requirePerm(verbHostEnroll, s.handleCreateHost))
	s.mux.HandleFunc("GET /api/hosts/{id}", s.requirePerm(verbHostView, s.handleGetHost))
	s.mux.HandleFunc("PUT /api/hosts/{id}", s.requirePerm(verbHostEdit, s.handleUpdateHost))
	s.mux.HandleFunc("DELETE /api/hosts/{id}", s.requirePerm(verbHostDelete, s.handleDeleteHost))
	s.mux.HandleFunc("POST /api/hosts/{id}/probe", s.requirePerm(verbHostView, s.handleProbe))
	s.mux.HandleFunc("GET /api/hosts/{id}/facts", s.requirePerm(verbHostView, s.handleHostFacts))
	s.mux.HandleFunc("POST /api/agents/install", s.requirePerm(verbHostEnroll, s.handleSSHInstall))
	// 证书远程换证（重签 + 推送 agent 热更换；与推装 agent 同权限口径）
	s.mux.HandleFunc("POST /api/hosts/{id}/renew-cert", s.requirePerm(verbHostEnroll, s.handleRenewHostCert))
	// 注册表：列表全员可见（各页搜索/下拉要用），增删需 registry:manage
	s.mux.HandleFunc("GET /api/pools", s.requireAuth(s.handleListPools))
	s.mux.HandleFunc("POST /api/pools", s.requirePerm(verbRegistry, s.handleCreatePool))
	s.mux.HandleFunc("DELETE /api/pools/{id}", s.requirePerm(verbRegistry, s.handleDeletePool))
	s.mux.HandleFunc("GET /api/groups", s.requireAuth(s.handleListGroups))
	s.mux.HandleFunc("POST /api/groups", s.requirePerm(verbRegistry, s.handleCreateGroup))
	s.mux.HandleFunc("DELETE /api/groups/{id}", s.requirePerm(verbRegistry, s.handleDeleteGroup))
	s.mux.HandleFunc("GET /api/labels", s.requireAuth(s.handleListLabels))
	s.mux.HandleFunc("POST /api/labels", s.requirePerm(verbRegistry, s.handleCreateLabel))
	s.mux.HandleFunc("DELETE /api/labels/{id}", s.requirePerm(verbRegistry, s.handleDeleteLabel))
	s.mux.HandleFunc("POST /api/hosts/batch", s.requireAuth(s.handleBatchHosts)) // 按 action 分权限（handler 内）
	s.mux.HandleFunc("POST /api/hosts/upgrade", s.requirePerm(verbHostUpgrade, s.handleUpgradeBatch))
	s.mux.HandleFunc("POST /api/hosts/{id}/upgrade", s.requirePerm(verbHostUpgrade, s.handleUpgradeHost))
	s.mux.HandleFunc("POST /api/hosts/import", s.requirePerm(verbHostEnroll, s.handleImportHosts))
	s.mux.HandleFunc("GET /api/modules", s.requireAuth(s.handleListModules))
	s.mux.HandleFunc("GET /api/schema", s.requireAuth(s.handleSchema))
	s.mux.HandleFunc("GET /api/audit", s.requirePerm(verbAuditView, s.handleListAuditLogs))
	s.mux.HandleFunc("GET /api/alerts", s.requirePerm(verbHostView, s.handleListAlerts))
	s.mux.HandleFunc("GET /api/hosts/{id}/metrics", s.requirePerm(verbHostView, s.handleHostMetrics))
	s.mux.HandleFunc("GET /api/hosts/{id}/series", s.requirePerm(verbHostView, s.handleHostSeries))
	s.mux.HandleFunc("GET /api/hosts/{id}/tasks", s.requirePerm(verbHostView, s.handleHostTasks))
	s.mux.HandleFunc("POST /api/exec", s.requirePerm(verbRunExec, s.handleExec))
	s.mux.HandleFunc("GET /api/exec/targets", s.requirePerm(verbRunExec, s.handleExecTargets))
	s.mux.HandleFunc("GET /api/exec/stream", s.requirePerm(verbRunExec, s.handleExecStream))
	s.mux.HandleFunc("GET /api/apps", s.requirePerm(verbAppView, s.handleListApps))
	s.mux.HandleFunc("POST /api/apps/upload", s.requirePerm(verbAppUpload, s.handleUploadChart))
	s.mux.HandleFunc("POST /api/apps/spec", s.requirePerm(verbAppCreate, s.handleCreateSpec))
	s.mux.HandleFunc("GET /api/apps/{id}", s.requirePerm(verbAppView, s.handleGetApp))
	s.mux.HandleFunc("GET /api/apps/{id}/spec", s.requirePerm(verbAppView, s.handleGetSpec))
	s.mux.HandleFunc("GET /api/apps/{id}/download", s.requirePerm(verbAppView, s.handleDownloadChart))
	s.mux.HandleFunc("PUT /api/apps/{id}/spec", s.requirePerm(verbAppEdit, s.handleSaveSpec))
	s.mux.HandleFunc("PUT /api/apps/{id}", s.requirePerm(verbAppEdit, s.handleUpdateApp))
	s.mux.HandleFunc("PUT /api/apps/{id}/scope", s.requirePerm(verbAppScope, s.handleUpdateVersionScope))
	s.mux.HandleFunc("DELETE /api/apps/{id}", s.requirePerm(verbAppDelete, s.handleDeleteApp))
	s.mux.HandleFunc("POST /api/apps/{id}/versions/upload", s.requirePerm(verbAppUpload, s.handleAddVersion))
	s.mux.HandleFunc("DELETE /api/apps/{id}/versions/{vid}", s.requirePerm(verbAppDelete, s.handleDeleteVersion))
	s.mux.HandleFunc("POST /api/apps/{id}/versions/{vid}/latest", s.requirePerm(verbAppEdit, s.handleSetLatestVersion))
	s.mux.HandleFunc("POST /api/apps/batch", s.requirePerm(verbAppDelete, s.handleBatchApps))
	s.mux.HandleFunc("POST /api/apps/validate", s.requireAuth(s.handleValidateSpec))         // 双模式权限在 handler 内按 app_id 分别把关
	s.mux.HandleFunc("POST /api/playbook/validate", s.requireAuth(s.handleValidatePlaybook)) // 裸 playbook 编辑器校验（只读计算，登录即可）
	s.mux.HandleFunc("GET /api/apps/drafts", s.requireAuth(s.handleListDrafts))              // 草稿箱列表（权限在 handler 内：view/create 任一）
	s.mux.HandleFunc("GET /api/apps/draft", s.requireAuth(s.handleGetDraft))                 // 同上：key 决定模式与权限点
	s.mux.HandleFunc("PUT /api/apps/draft", s.requireAuth(s.handlePutDraft))
	s.mux.HandleFunc("DELETE /api/apps/draft", s.requireAuth(s.handleDeleteDraft))
	s.mux.HandleFunc("POST /api/runs", s.requirePerm(verbRunExec, s.handleRunApps))
	s.mux.HandleFunc("GET /api/runs", s.requirePerm(verbRunView, s.handleListRuns))
	s.mux.HandleFunc("GET /api/runs/stream", s.requirePerm(verbRunView, s.handleRunStream))
	s.mux.HandleFunc("GET /api/runs/{id}", s.requirePerm(verbRunView, s.handleGetRun))
	s.mux.HandleFunc("POST /api/runs/{id}/cancel", s.requirePerm(verbRunExec, s.handleCancelRun))
	s.mux.HandleFunc("DELETE /api/runs/{id}", s.requirePerm(verbRunDelete, s.handleDeleteRun))
	s.mux.HandleFunc("POST /api/runs/batch", s.requirePerm(verbRunDelete, s.handleBatchDeleteRuns))
	s.mux.Handle("GET /assets/", s.handleAssets())
	s.mux.HandleFunc("GET /", s.handleIndex)
	s.routesObservability()
}
