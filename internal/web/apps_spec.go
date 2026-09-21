package web

// 图形化编辑器 spec 端点的传输层：解码、权限、调用 console.AppService、
// 审计与响应（业务实现见 internal/console/spec.go）。

import (
	"errors"
	"fmt"
	"net/http"
	"os"

	"wdp/internal/chart"
	"wdp/internal/console"
	"wdp/internal/store"
)

// 类型别名：console 迁移后保持既有引用面（含测试）不变。
type (
	SpecFile          = console.SpecFile
	AppSpec           = console.AppSpec
	ChartMetaReq      = console.ChartMetaReq
	ChartPhaseSpecReq = console.ChartPhaseSpecReq
	specReq           = console.SpecReq
)

// chartYAMLVersion 读 chart.yaml 顶层 version（console 实现）。
var chartYAMLVersion = console.ChartYAMLVersion

func (s *Server) handleGetSpec(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	app, ok := s.checkApp(w, r, verbAppView, id)
	if !ok {
		return
	}
	version := r.URL.Query().Get("version")
	if version == "" {
		version = app.LatestVersion
	}
	if version == "" {
		writeError(w, http.StatusBadRequest, "app has no versions")
		return
	}
	tgz, err := s.st.VersionTgz(id, version)
	if err != nil {
		writeError(w, http.StatusNotFound, fmt.Sprintf("version %s not found", version))
		return
	}
	ch, err := chart.LoadWithLimits(tgz, chart.Limits{})
	if err != nil {
		s.writeInternal(w, fmt.Errorf("chart load failed: %w", err))
		return
	}
	defer ch.Close()
	spec, err := console.ReadSpecFromDir(ch.Dir, app.Name, ch.Meta.Version, ch.Meta.Description)
	if err != nil {
		s.writeInternal(w, fmt.Errorf("read spec: %w", err))
		return
	}
	// scope 是版本级的：按所请求版本（= 编辑底本）返回，编辑保存即继承；
	// 迁移前的旧行列值为空（ok=false）→ 回退应用级（旧行为）。
	if vp, vg, vl, ok, verr := s.st.VersionScopes(id, version); verr == nil && ok {
		spec.Pools, spec.Groups, spec.Labels = vp, vg, vl
	} else if verr != nil && !errors.Is(verr, store.ErrNotFound) {
		s.writeInternal(w, fmt.Errorf("read version scopes: %w", verr))
		return
	} else {
		spec.Pools, spec.Groups, spec.Labels = app.Pools, app.Groups, app.Labels
	}
	writeJSON(w, http.StatusOK, spec)
}

func (s *Server) writeSpecErr(w http.ResponseWriter, err error, baseVersion string) {
	var be *console.BizError
	if errors.As(err, &be) {
		writeError(w, http.StatusBadRequest, be.Error())
		return
	}
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, fmt.Sprintf("base version %s not found", baseVersion))
		return
	}
	s.writeInternal(w, err)
}

// handleCreateSpec 新建应用（图形化创建：名称/版本/描述/步骤/文件）。
func (s *Server) handleCreateSpec(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
		specReq
	}
	if !decodeJSONLarge(w, r, &req) {
		return
	}
	if !appNameRe.MatchString(req.Name) {
		writeError(w, http.StatusBadRequest, "invalid app name (letters, digits, . _ -, must start alphanumeric)")
		return
	}
	workDir, err := s.apps.PrepareWorkspace(0, "", req.Name, &req.specReq)
	if err != nil {
		s.writeSpecErr(w, err, "")
		return
	}
	defer os.RemoveAll(workDir)
	// 重名判定与建应用整体进临界区（与 chart 上传建应用互斥）：否则两条
	// 并发创建路径可同时通过重名预检、互相覆盖制品
	s.uploadMu.Lock()
	defer s.uploadMu.Unlock()
	if _, err := s.st.AppByName(req.Name); err == nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("app %q already exists (open it and save a new version instead)", req.Name))
		return
	} else if !errors.Is(err, store.ErrNotFound) {
		s.writeInternal(w, err)
		return
	}
	final, sha, size, err := s.apps.PackAndStore(workDir, req.Name, req.Version)
	if err != nil {
		s.writeInternal(w, err)
		return
	}
	phases, perr := chartPhasesOf(final)
	if perr != nil {
		os.Remove(final)
		s.writeInternal(w, fmt.Errorf("load packed chart: %w", perr))
		return
	}
	id, err := s.st.CreateApp(req.Name, req.Description, req.Labels, req.Pools, req.Groups, req.Version, final, sha, size, phases)
	if err != nil {
		// 约束：失败先确认制品未被并发先到者入库引用（重名即同路径），
		// 被引用则不删
		if !s.artifactReferenced(req.Name, req.Version) {
			os.Remove(final)
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.audit(r, "create", "app", req.Name, "图形化创建 "+req.Version)
	s.logger.Info("app created from spec", "name", req.Name, "version", req.Version)
	app, _ := s.st.GetApp(id)
	writeJSON(w, http.StatusCreated, app)
}

// bizError spec 物化过程的业务错误（结构/语法/一致性校验不过）——

func (s *Server) handleSaveSpec(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	app, ok := s.checkApp(w, r, verbAppEdit, id)
	if !ok {
		return
	}
	var req specReq
	if !decodeJSONLarge(w, r, &req) {
		return
	}
	baseVersion := req.BaseVersion
	if baseVersion == "" {
		baseVersion = app.LatestVersion
	}
	if baseVersion == "" {
		writeError(w, http.StatusBadRequest, "app has no versions to base on")
		return
	}
	// 约束：新写版本/应用 scope 的目标值须全部落在调用者 app:edit 授权内
	//（app:edit@poolA 借编辑器把 scope 扩到任意池 = 越权，扩 scope 是
	// app:scope 的权限语义）
	if bad := s.scopeWriteCheck(r, verbAppEdit, req.Pools, req.Groups, labelKeySlice(req.Labels)); bad != "" {
		writeError(w, http.StatusForbidden, "forbidden: "+bad+" outside your app:edit scope (changing scopes beyond your own requires app:scope)")
		return
	}
	workDir, err := s.apps.PrepareWorkspace(id, baseVersion, app.Name, &req)
	if err != nil {
		s.writeSpecErr(w, err, baseVersion)
		return
	}
	defer os.RemoveAll(workDir)

	// 预检、打包与入库整体进临界区（理由同 handleUploadChart）：
	// packAndStore 直接写目标路径，重复版本会覆盖存档
	s.uploadMu.Lock()
	defer s.uploadMu.Unlock()
	exists, herr := s.st.HasVersion(id, req.Version)
	if herr != nil {
		// 约束：预检出错不得当"不存在"放行
		s.writeInternal(w, herr)
		return
	}
	if exists {
		writeError(w, http.StatusBadRequest, versionExistsMsg(app.Name, req.Version, store.ErrVersionExists))
		return
	}
	// DB 行丢失但制品文件残留时同样按版本已存在拒绝：rename 会覆盖它
	if _, serr := os.Stat(s.appTgzPath(app.Name, req.Version)); serr == nil {
		writeError(w, http.StatusBadRequest, versionExistsMsg(app.Name, req.Version, store.ErrVersionExists))
		return
	}
	final, sha, size, err := s.apps.PackAndStore(workDir, app.Name, req.Version)
	if err != nil {
		s.writeInternal(w, err)
		return
	}
	phases, perr := chartPhasesOf(final)
	if perr != nil {
		os.Remove(final)
		s.writeInternal(w, fmt.Errorf("load packed chart: %w", perr))
		return
	}
	note := fmt.Sprintf("edited from %s", baseVersion)
	if err := s.st.AddVersion(id, req.Version, final, sha, size, note, req.Pools, req.Groups, req.Labels, phases); err != nil {
		// 约束：ErrVersionExists = 该路径制品已被先到的入库版本引用，删除
		// 会毁掉其制品；其余错误才清理本次归位
		if !errors.Is(err, store.ErrVersionExists) {
			os.Remove(final)
		}
		writeError(w, http.StatusBadRequest, versionExistsMsg(app.Name, req.Version, err))
		return
	}
	if err := s.st.UpdateAppScopes(id, req.Description, req.Pools, req.Groups, req.Labels); err != nil {
		s.logger.Warn("update app scopes failed", "app", app.Name, "err", err)
	}
	s.audit(r, "update", "app", app.Name, "编辑器保存版本 "+req.Version+"（底本 "+baseVersion+"）")
	s.logger.Info("app spec saved", "name", app.Name, "version", req.Version, "base", baseVersion)
	updated, _ := s.st.GetApp(id)
	writeJSON(w, http.StatusOK, updated)
}
