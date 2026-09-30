package web

// Chart 仓库端点：把控制台里版本化管理的 chart 制品以 Helm 兼容仓库的
// 形态暴露给 CLI（与第三方工具）——
//
//	GET /charts/index.yaml                仓库索引（Helm index v1 格式）
//	GET /charts/<name>-<version>.tgz      下载版本制品
//
// 认证与控制台同源：浏览器会话 cookie 或 HTTP Basic（GET 自动支持，
// 服务端对非 JSON 请求发 Basic 质询——curl/CLI 直接可用）。权限点
// app:view：能看应用列表就能拉 chart；上传仍走控制台/API（app:upload）。
//
// 索引 digest 即入库时记录的 sha256，CLI（wdp repo pull）与 Helm 都按
// 此校验完整性。

import (
	"fmt"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// routesChartRepo 注册 chart 仓库端点（无 /api 前缀：Helm/CLI 惯例路径）。
func (s *Server) routesChartRepo() {
	s.mux.HandleFunc("GET /charts/index.yaml", s.requireAuth(s.requirePerm(verbAppView, s.handleRepoIndex)))
	s.mux.HandleFunc("GET /charts/{file}", s.requireAuth(s.requirePerm(verbAppView, s.handleRepoTgz)))
}

// repoIndexEntry Helm index 的单版本条目（字段名与 Helm 语义对齐；
// wdp chart 的 version 即应用版本，appVersion 同值——Helm 端只读展示）。
type repoIndexEntry struct {
	ApiVersion  string            `yaml:"apiVersion"` // 单条目也带（Helm 部分工具读条目级）
	Name        string            `yaml:"name"`
	Version     string            `yaml:"version"`
	AppVersion  string            `yaml:"appVersion"`
	Description string            `yaml:"description"`
	Created     string            `yaml:"created"`
	Digest      string            `yaml:"digest"` // sha256:<hex>（入库校验和）
	Urls        []string          `yaml:"urls"`
	Annotations map[string]string `yaml:"annotations,omitempty"` // phases=生命相位清单
}

// repoIndex Helm index.yaml 顶层结构。
type repoIndex struct {
	ApiVersion string                      `yaml:"apiVersion"`
	Generated  string                      `yaml:"generated"`
	Entries    map[string][]repoIndexEntry `yaml:"entries"`
}

// handleRepoIndex 生成全库索引：应用 → 版本（新→旧，Helm 依赖首条最新）。
func (s *Server) handleRepoIndex(w http.ResponseWriter, _ *http.Request) {
	apps, err := s.st.ListApps()
	if err != nil {
		s.writeInternal(w, err)
		return
	}
	idx := repoIndex{
		ApiVersion: "v1",
		Generated:  time.Now().UTC().Format(time.RFC3339),
		Entries:    map[string][]repoIndexEntry{},
	}
	for _, a := range apps {
		vers, err := s.st.ListVersions(a.ID)
		if err != nil {
			s.writeInternal(w, err)
			return
		}
		for _, v := range vers {
			e := repoIndexEntry{
				ApiVersion:  "v1",
				Name:        a.Name,
				Version:     v.Version,
				AppVersion:  v.Version,
				Description: v.Note,
				Created:     v.CreatedAt,
				Digest:      "sha256:" + v.Sha256,
				Urls:        []string{fmt.Sprintf("/charts/%s-%s.tgz", a.Name, v.Version)},
			}
			if len(v.Phases) > 0 {
				e.Annotations = map[string]string{"phases": strings.Join(v.Phases, ",")}
			}
			idx.Entries[a.Name] = append(idx.Entries[a.Name], e)
		}
		// ListVersions 已新→旧；排序是给 Helm 消费方的保险（首条=最新）
		sort.SliceStable(idx.Entries[a.Name], func(i, j int) bool {
			return idx.Entries[a.Name][i].Version > idx.Entries[a.Name][j].Version
		})
	}
	body, err := yaml.Marshal(idx)
	if err != nil {
		s.writeInternal(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/yaml; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache") // 版本随时入库，禁缓存防索引滞后
	_, _ = w.Write(body)
}

// repoFileRe 合法制品文件名：<name>-<version>.tgz（名字允许字母数字与
// . _ -，版本以数字开头）。名单仍以库内 (name, version) 为准——正则
// 只挡明显垃圾，不做路径拼接。
var repoFileRe = regexp.MustCompile(`^[A-Za-z0-9._-]+-[0-9][A-Za-z0-9.+_-]*\.tgz$`)

// handleRepoTgz 下载版本制品。文件名到库内记录的反查：名字可含连字符，
// 按「应用名前缀 + 剩余段为已入库版本」匹配；命中即按库内 TgzPath 供源。
func (s *Server) handleRepoTgz(w http.ResponseWriter, r *http.Request) {
	file := r.PathValue("file")
	if !repoFileRe.MatchString(file) {
		writeError(w, http.StatusNotFound, "unknown chart file")
		return
	}
	apps, err := s.st.ListApps()
	if err != nil {
		s.writeInternal(w, err)
		return
	}
	base := strings.TrimSuffix(file, ".tgz")
	for _, a := range apps {
		if !strings.HasPrefix(base, a.Name+"-") {
			continue
		}
		version := strings.TrimPrefix(base, a.Name+"-")
		vers, err := s.st.ListVersions(a.ID)
		if err != nil {
			s.writeInternal(w, err)
			return
		}
		for _, v := range vers {
			if v.Version != version {
				continue
			}
			path := v.TgzPath
			if path == "" {
				path = s.appTgzPath(a.Name, v.Version)
			}
			f, err := os.Open(path)
			if err != nil {
				s.writeInternal(w, err)
				return
			}
			defer f.Close()
			// ETag 用内容 sha256：客户端条件请求可免重复下载
			w.Header().Set("Content-Type", "application/gzip")
			w.Header().Set("ETag", `"sha256-`+v.Sha256+`"`)
			http.ServeContent(w, r, "", time.Time{}, f)
			return
		}
	}
	writeError(w, http.StatusNotFound, "unknown chart version")
}
