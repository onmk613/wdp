package web

// 应用版本制品下载：GET /api/apps/{id}/download?version=<v>（缺省最新）。
// 回原样 tgz（Content-Disposition: attachment，文件名 <name>-<version>.tgz），
// 可直接 `wdp apply` 或再上传导入。下载是只读操作（app:view），进审计。

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
)

func (s *Server) handleDownloadChart(w http.ResponseWriter, r *http.Request) {
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
		s.writeStoreErr(w, err)
		return
	}
	f, err := os.Open(tgz)
	if err != nil {
		if os.IsNotExist(err) {
			// DB 行在、制品文件丢失：数据不一致，如实报告而非 200 空响应
			s.writeInternal(w, fmt.Errorf("artifact missing on disk: %s", filepath.Base(tgz)))
			return
		}
		s.writeInternal(w, err)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		s.writeInternal(w, err)
		return
	}
	s.audit(r, "download", "app", app.Name, "下载版本制品 "+version)
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf(`attachment; filename="%s-%s.tgz"`, app.Name, version))
	w.Header().Set("Content-Length", strconv.FormatInt(fi.Size(), 10))
	if _, err := io.Copy(w, f); err != nil {
		// 响应头已发，无法改状态码；客户端中断/磁盘读错在此只能记日志
		s.logger.Warn("download chart: write body failed", "app", app.Name, "version", version, "err", err)
	}
}
