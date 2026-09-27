package web

// 应用上传：chart tgz 上传入库（建应用/加版本）的 HTTP 处理与版本重复提示。

import (
	"errors"
	"fmt"
	"net/http"
	"os"

	"wdp/internal/store"
)

// chartUpload 一次 chart 上传的解析产物（readChartUpload 的返回形态）。
type chartUpload struct {
	tmpPath     string // 落盘临时文件（调用方负责 os.Remove）
	sha         string
	size        int64
	name        string // chart.yaml 名称
	version     string
	description string
	phases      []string
}

// readChartUpload chart 上传的校验阶段：multipart 体限制（无总量上限的
// 体可被持权限者无限灌盘）→ tgz 落临时文件 → 读 chart.yaml 元信息
// （应用名/版本取自 chart.yaml）。失败时已写好响应，返回 false。
func (s *Server) readChartUpload(w http.ResponseWriter, r *http.Request) (chartUpload, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		writeMultipartErr(w, err)
		return chartUpload{}, false
	}
	tmpPath, sha, size, err := s.saveTgzTemp(r, "tgz")
	if err != nil {
		writeMultipartErr(w, err)
		return chartUpload{}, false
	}
	name, version, description, phases, err := chartMetaOf(tmpPath)
	if err != nil {
		os.Remove(tmpPath)
		s.writeStoreErr(w, err)
		return chartUpload{}, false
	}
	return chartUpload{tmpPath: tmpPath, sha: sha, size: size,
		name: name, version: version, description: description, phases: phases}, true
}

// handleUploadChart 上传 chart 包（multipart，字段 tgz）：应用名与版本
// 取自 chart.yaml；同名应用存在则并入（版本已存在即覆盖），不存在则创建。
// 校验（readChartUpload）与入库（storeVersion）各自拆开，本函数只做编排。
func (s *Server) handleUploadChart(w http.ResponseWriter, r *http.Request) {
	up, ok := s.readChartUpload(w, r)
	if !ok {
		return
	}
	defer os.Remove(up.tmpPath)
	name, version := up.name, up.version

	// 版本入库临界区：预检/归位/入库的并发与清理约束见 storeVersion
	var (
		appID   int64
		created bool
	)
	if !s.storeVersion(w, name, version,
		// 锁内解析目标应用（决定建应用还是加版本）
		func() (*store.App, bool) {
			existing, appErr := s.st.AppByName(name)
			if appErr != nil && !errors.Is(appErr, store.ErrNotFound) {
				s.writeInternal(w, appErr)
				return nil, false
			}
			if appErr != nil {
				return nil, true
			}
			return existing, true
		},
		// 并入既有应用须在该应用的作用域内；新应用要求 app:upload 的**全局**
		// 权限（与 handleCreateSpec 的 verbAppCreate 口径一致）——requirePerm
		// 对作用域用户同样放行，不补这道校验的话仅持 app:upload@poolA 的
		// 用户可以任意名字建空作用域应用，污染全局应用列表
		func(app *store.App) bool {
			if app != nil {
				if !s.permsOf(permUser(r)).canApp(verbAppUpload, app.Pools, app.Groups, app.Labels) {
					permApp403(w)
					return false
				}
				return true
			}
			if !s.permsOf(permUser(r)).global[verbAppUpload] {
				permApp403(w)
				return false
			}
			return true
		},
		func() (string, string, int64, error) {
			final, err := s.moveTgz(up.tmpPath, name, version)
			return final, up.sha, up.size, err
		},
		func(app *store.App, final, sha string, size int64) error {
			if app != nil {
				appID = app.ID
				// chart 上传不带 scope：继承应用当前 scope
				return s.st.AddVersion(app.ID, version, final, sha, size, "uploaded chart",
					app.Pools, app.Groups, app.Labels, up.phases)
			}
			id, cerr := s.st.CreateApp(name, up.description, "{}", nil, nil, version, final, sha, size, up.phases)
			appID, created = id, true
			return cerr
		},
	) {
		return
	}
	if created {
		s.audit(r, "create", "app", name, "chart 上传建应用 "+version)
		s.logger.Info("app created from chart upload", "name", name, "version", version, "size", up.size)
	} else {
		s.audit(r, "upload", "version", name+"@"+version, "chart 上传新版本")
		s.logger.Info("app version uploaded", "name", name, "version", version, "size", up.size)
	}
	s.replyApp(w, appID, http.StatusCreated)
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
	up, ok := s.readChartUpload(w, r)
	if !ok {
		return
	}
	defer os.Remove(up.tmpPath)
	if up.name != app.Name {
		writeError(w, http.StatusBadRequest, fmt.Sprintf(
			"chart.yaml name %q does not match app %q (uploading to the wrong app?)", up.name, app.Name))
		return
	}
	// chart 上传继承应用当前 scope 写入版本行：继承值同样须全部落在调用者
	// 上传授权内（canApp 是"任一匹配即过"，版本行不得带上授权外的池/组）
	if bad := s.scopeWriteCheck(r, verbAppUpload, app.Pools, app.Groups, labelKeySlice(app.Labels)); bad != "" {
		writeError(w, http.StatusForbidden, "forbidden: app scope includes "+bad+" outside your app:upload scope")
		return
	}

	// 版本入库临界区：预检/归位/入库的并发与清理约束见 storeVersion
	if !s.storeVersion(w, app.Name, up.version,
		func() (*store.App, bool) { return app, true },
		nil,
		func() (string, string, int64, error) {
			final, err := s.moveTgz(up.tmpPath, app.Name, up.version)
			return final, up.sha, up.size, err
		},
		func(_ *store.App, final, sha string, size int64) error {
			// chart 上传不带 scope：继承应用当前 scope
			return s.st.AddVersion(app.ID, up.version, final, sha, size, r.FormValue("note"),
				app.Pools, app.Groups, app.Labels, up.phases)
		},
	) {
		return
	}
	s.audit(r, "upload", "version", app.Name+"@"+up.version, "chart 上传新版本")
	versions, _ := s.st.ListVersions(app.ID)
	writeJSON(w, http.StatusCreated, versions)
}

// storeVersion 版本入库临界区（chart 上传并入/建应用、编辑器保存三处
// 入库路径共用）：uploadMu 加锁 → resolve 在锁内解析目标应用 →
// HasVersion 预检 → check 入区复核 → os.Stat 查残留制品 → place 制品
// 归位 → add 版本行入库。任一步失败时已写好响应并返回 false。
//
// 并发与清理约束归一到此（原先在三处 handler 内重复维护）：
//   - 预检与归位之间留窗口的话，并发同版本上传会互相覆盖制品——预检、
//     归位与入库必须整体进临界区（uploadMu）；
//   - 版本存在性预检必须发生在归位之前：归位是覆盖式写入，事后判重复
//     会把已发布版本的制品一并毁掉；预检出错不得当"不存在"放行——
//     归位是覆盖式写入；
//   - DB 行丢失但制品文件残留时同样按版本已存在拒绝：rename 会覆盖它；
//   - 入库失败先确认制品未被并发先到者入库引用（重名即同路径），被引用
//     则不删——ErrVersionExists = 该路径制品已被先到的入库版本引用，
//     删除会毁掉已入库版本；其余错误才清理本次归位。
//
// resolve 返回目标应用（nil = 上传建应用路径，不存在的应用无版本可言，
// HasVersion 恒为空）；check 为预检后的入区复核（上传路径的应用级权限
// 校验，nil = 跳过）；place 归位制品并给出摘要（sha/size）；add 落版本行
// （或建应用）。
func (s *Server) storeVersion(
	w http.ResponseWriter, appName, version string,
	resolve func() (*store.App, bool),
	check func(app *store.App) bool,
	place func() (final, sha string, size int64, err error),
	add func(app *store.App, final, sha string, size int64) error,
) bool {
	s.uploadMu.Lock()
	defer s.uploadMu.Unlock()

	app, ok := resolve()
	if !ok {
		return false
	}
	appID := int64(0)
	if app != nil {
		appID = app.ID
	}
	exists, herr := s.st.HasVersion(appID, version)
	if herr != nil {
		// 约束：预检出错不得当"不存在"放行——归位是覆盖式写入
		s.writeInternal(w, herr)
		return false
	}
	if exists {
		writeError(w, http.StatusBadRequest, versionExistsMsg(appName, version, store.ErrVersionExists))
		return false
	}
	if check != nil && !check(app) {
		return false
	}
	// DB 行丢失但制品文件残留时同样按版本已存在拒绝：rename 会覆盖它
	if _, serr := os.Stat(s.appTgzPath(appName, version)); serr == nil {
		writeError(w, http.StatusBadRequest, versionExistsMsg(appName, version, store.ErrVersionExists))
		return false
	}
	final, sha, size, err := place()
	if err != nil {
		s.writeInternal(w, err)
		return false
	}
	if err := add(app, final, sha, size); err != nil {
		// 约束：先确认制品未被并发先到者入库引用（重名即同路径），被引用
		// 则不删——ErrVersionExists 即此情形，删除会毁掉已入库版本
		if !s.artifactReferenced(appName, version) {
			os.Remove(final)
		}
		if errors.Is(err, store.ErrVersionExists) {
			writeError(w, http.StatusBadRequest, versionExistsMsg(appName, version, err))
			return false
		}
		s.writeStoreErr(w, err)
		return false
	}
	return true
}

// versionExistsMsg 版本重复的可读提示（版本一经发布不可覆盖）。
func versionExistsMsg(appName, version string, err error) string {
	if errors.Is(err, store.ErrVersionExists) {
		return fmt.Sprintf("应用 %s 已存在版本 %s：版本发布后不可覆盖——请改版本号后重试（上传场景改 chart.yaml 的 version；编辑器里直接改「版本号」字段保存为新版本）",
			appName, version)
	}
	return err.Error()
}
