package web

// 应用上传：chart tgz 上传入库（建应用/加版本）的 HTTP 处理与版本重复提示。

import (
	"errors"
	"fmt"
	"net/http"
	"os"

	"wdp/internal/store"
)

// handleUploadChart 上传 chart 包（multipart，字段 tgz）：应用名与版本
// 取自 chart.yaml；同名应用存在则并入（版本已存在即覆盖），不存在则创建。
func (s *Server) handleUploadChart(w http.ResponseWriter, r *http.Request) {
	// 约束：无总量上限的 multipart 体可被持权限者无限灌盘
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		writeMultipartErr(w, err)
		return
	}
	tmpPath, sha, size, err := s.saveTgzTemp(r, "tgz")
	if err != nil {
		writeMultipartErr(w, err)
		return
	}
	defer os.Remove(tmpPath)

	name, version, description, phases, err := chartMetaOf(tmpPath)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// 应用存在性/版本预检、制品归位与入库整体进临界区（uploadMu）：预检与
	// 归位之间留窗口的话，并发同版本上传会互相覆盖制品
	s.uploadMu.Lock()
	defer s.uploadMu.Unlock()

	// 应用是否已存在（决定建应用还是加版本）；版本存在性预检必须发生在
	// moveTgz 之前：moveTgz 直写目标路径，事后判重复会把已发布版本的制品
	// 一并毁掉
	existing, appErr := s.st.AppByName(name)
	switch {
	case appErr == nil:
		exists, herr := s.st.HasVersion(existing.ID, version)
		if herr != nil {
			// 约束：预检出错不得当"不存在"放行——归位是覆盖式写入
			s.writeInternal(w, herr)
			return
		}
		if exists {
			writeError(w, http.StatusBadRequest, versionExistsMsg(name, version, store.ErrVersionExists))
			return
		}
	case !errors.Is(appErr, store.ErrNotFound):
		s.writeInternal(w, appErr)
		return
	}

	// 并入既有应用须在该应用的作用域内（新应用走 app:upload 全局权限）
	if appErr == nil && !s.permsOf(permUser(r)).canApp(verbAppUpload, existing.Pools, existing.Groups, existing.Labels) {
		permApp403(w)
		return
	}
	// DB 行丢失但制品文件残留时同样按版本已存在拒绝：rename 会覆盖它
	if _, serr := os.Stat(s.appTgzPath(name, version)); serr == nil {
		writeError(w, http.StatusBadRequest, versionExistsMsg(name, version, store.ErrVersionExists))
		return
	}

	final, err := s.moveTgz(tmpPath, name, version)
	if err != nil {
		s.writeInternal(w, err)
		return
	}

	var app *store.App
	if appErr == nil {
		// chart 上传不带 scope：继承应用当前 scope
		if err := s.st.AddVersion(existing.ID, version, final, sha, size, "uploaded chart", existing.Pools, existing.Groups, existing.Labels, phases); err != nil {
			// 约束：ErrVersionExists = 该路径制品已被先到的入库版本引用，
			// 删除会毁掉已入库版本；其余错误才清理本次归位
			if !errors.Is(err, store.ErrVersionExists) {
				os.Remove(final)
			}
			writeError(w, http.StatusBadRequest, versionExistsMsg(name, version, err))
			return
		}
		app, _ = s.st.GetApp(existing.ID)
		s.audit(r, "upload", "version", name+"@"+version, "chart 上传新版本")
		s.logger.Info("app version uploaded", "name", name, "version", version, "size", size)
	} else {
		id, cerr := s.st.CreateApp(name, description, "{}", nil, nil, version, final, sha, size, phases)
		if cerr != nil {
			// 约束：失败先确认制品未被并发先到者入库引用（重名即同路径），
			// 被引用则不删
			if !s.artifactReferenced(name, version) {
				os.Remove(final)
			}
			writeError(w, http.StatusBadRequest, cerr.Error())
			return
		}
		app, _ = s.st.GetApp(id)
		s.audit(r, "create", "app", name, "chart 上传建应用 "+version)
		s.logger.Info("app created from chart upload", "name", name, "version", version, "size", size)
	}
	writeJSON(w, http.StatusCreated, app)
}

// handleAddVersion 向既有应用上传新版本（multipart，字段 tgz）：版本取自
// chart.yaml；chart.yaml 名称必须与应用一致（避免传错应用）。
func (s *Server) handleAddVersion(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	app, ok := s.checkApp(w, r, verbAppUpload, id)
	if !ok {
		return
	}
	// 约束：无总量上限的 multipart 体可被持权限者无限灌盘
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		writeMultipartErr(w, err)
		return
	}
	tmpPath, sha, size, err := s.saveTgzTemp(r, "tgz")
	if err != nil {
		writeMultipartErr(w, err)
		return
	}
	defer os.Remove(tmpPath)

	name, version, _, phases, err := chartMetaOf(tmpPath)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if name != app.Name {
		writeError(w, http.StatusBadRequest, fmt.Sprintf(
			"chart.yaml name %q does not match app %q (uploading to the wrong app?)", name, app.Name))
		return
	}
	// chart 上传继承应用当前 scope 写入版本行：继承值同样须全部落在调用者
	// 上传授权内（canApp 是"任一匹配即过"，版本行不得带上授权外的池/组）
	if bad := s.scopeWriteCheck(r, verbAppUpload, app.Pools, app.Groups, labelKeySlice(app.Labels)); bad != "" {
		writeError(w, http.StatusForbidden, "forbidden: app scope includes "+bad+" outside your app:upload scope")
		return
	}

	// 预检、归位与入库整体进临界区（理由同 handleUploadChart）
	s.uploadMu.Lock()
	defer s.uploadMu.Unlock()
	exists, herr := s.st.HasVersion(app.ID, version)
	if herr != nil {
		// 约束：预检出错不得当"不存在"放行
		s.writeInternal(w, herr)
		return
	}
	if exists {
		writeError(w, http.StatusBadRequest, versionExistsMsg(app.Name, version, store.ErrVersionExists))
		return
	}
	// DB 行丢失但制品文件残留时同样按版本已存在拒绝：rename 会覆盖它
	if _, serr := os.Stat(s.appTgzPath(app.Name, version)); serr == nil {
		writeError(w, http.StatusBadRequest, versionExistsMsg(app.Name, version, store.ErrVersionExists))
		return
	}
	final, err := s.moveTgz(tmpPath, app.Name, version)
	if err != nil {
		s.writeInternal(w, err)
		return
	}
	// chart 上传不带 scope：继承应用当前 scope
	if err := s.st.AddVersion(app.ID, version, final, sha, size, r.FormValue("note"), app.Pools, app.Groups, app.Labels, phases); err != nil {
		// 约束：ErrVersionExists = 该路径制品已被先到的入库版本引用，删除
		// 会毁掉其制品；其余错误才清理本次归位
		if !errors.Is(err, store.ErrVersionExists) {
			os.Remove(final)
		}
		writeError(w, http.StatusBadRequest, versionExistsMsg(app.Name, version, err))
		return
	}
	s.audit(r, "upload", "version", app.Name+"@"+version, "chart 上传新版本")
	versions, _ := s.st.ListVersions(app.ID)
	writeJSON(w, http.StatusCreated, versions)
}

// versionExistsMsg 版本重复的可读提示（版本一经发布不可覆盖）。
func versionExistsMsg(appName, version string, err error) string {
	if errors.Is(err, store.ErrVersionExists) {
		return fmt.Sprintf("应用 %s 已存在版本 %s：版本发布后不可覆盖——请改版本号后重试（上传场景改 chart.yaml 的 version；编辑器里直接改「版本号」字段保存为新版本）",
			appName, version)
	}
	return err.Error()
}
