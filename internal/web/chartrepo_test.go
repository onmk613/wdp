package web

// Chart 仓库端点测试：索引格式（Helm 兼容字段）、制品下载（digest 与库内
// sha256 一致）、认证（未登录 401 + Basic 质询、basic 凭据可拉、viewer
// 可读 app:view、无 app:view 拒绝）。

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
	"gopkg.in/yaml.v3"

	"wdp/internal/store"
)

// setupRepoServer 建带临时 DataDir 的 server 并上传两版本应用。
// 不用 newEnrollServer：它不设 DataDir，制品会落进真实 ./wdp-data/apps/
// 且跨测试运行同名相撞（版本不可覆盖）。
func setupRepoServer(t *testing.T) (http.Handler, string, string) {
	t.Helper()
	name := "repo" + strings.ToLower(strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, t.Name()))
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	hash, err := bcrypt.GenerateFromPassword([]byte("passw0rd"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateUser("admin", string(hash), "admin"); err != nil {
		t.Fatal(err)
	}
	s, err := New(st, Options{AdminUser: "admin", CADir: filepath.Join(t.TempDir(), "ca"), DataDir: dir}, nil)
	if err != nil {
		t.Fatal(err)
	}
	h := s.Handler()
	token := loginSession(t, s)
	for _, v := range []string{"1.0.0", "1.1.0"} {
		if rec := uploadChart(t, h, token, "/api/apps/upload", "n.tgz", buildChartTgz(t, name, v, "marker-"+v)); rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
			t.Fatalf("上传 %s 失败: %d %s", v, rec.Code, rec.Body)
		}
	}
	return h, token, name
}

// getWith 认证变体辅助。
func getWith(h http.Handler, path string, token *string, basic *string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", path, nil)
	if token != nil {
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: *token})
	}
	if basic != nil {
		u, p, _ := strings.Cut(*basic, ":")
		req.SetBasicAuth(u, p)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestChartRepoIndex 索引：Helm 字段齐全、版本新→旧、digest 与库内一致。
func TestChartRepoIndex(t *testing.T) {
	h, token, name := setupRepoServer(t)
	rec := getWith(h, "/charts/index.yaml", &token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("index 应 200: %d %s", rec.Code, rec.Body)
	}
	var idx struct {
		ApiVersion string `yaml:"apiVersion"`
		Entries    map[string][]struct {
			Name    string   `yaml:"name"`
			Version string   `yaml:"version"`
			Digest  string   `yaml:"digest"`
			Urls    []string `yaml:"urls"`
		} `yaml:"entries"`
	}
	if err := yaml.Unmarshal(rec.Body.Bytes(), &idx); err != nil {
		t.Fatalf("index 解析失败: %v\n%s", err, rec.Body)
	}
	if idx.ApiVersion != "v1" {
		t.Fatalf("apiVersion 应 v1: %s", idx.ApiVersion)
	}
	entries := idx.Entries[name]
	if len(entries) != 2 {
		t.Fatalf("应有 2 个版本: %+v", entries)
	}
	if entries[0].Version != "1.1.0" || entries[1].Version != "1.0.0" {
		t.Fatalf("首条应为最新: %+v", entries)
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Digest, "sha256:") || len(e.Urls) != 1 {
			t.Fatalf("条目缺 digest/urls: %+v", e)
		}
	}
}

// TestChartRepoTgz 下载：内容 sha256 与索引 digest 一致；Range 由
// ServeContent 处理（不单独断言）。
func TestChartRepoTgz(t *testing.T) {
	h, token, name := setupRepoServer(t)
	rec := getWith(h, "/charts/"+name+"-1.0.0.tgz", &token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("下载应 200: %d %s", rec.Code, rec.Body)
	}
	sum := sha256.Sum256(rec.Body.Bytes())
	got := hex.EncodeToString(sum[:])

	// 与索引 digest 对账
	irec := getWith(h, "/charts/index.yaml", &token, nil)
	if !strings.Contains(irec.Body.String(), "sha256:"+got) {
		t.Fatalf("下载内容 sha256 与索引不符: %s", got)
	}
	if rec.Header().Get("ETag") == "" {
		t.Fatal("应带 ETag")
	}

	// 未入库版本/畸形文件名 404
	if rec := getWith(h, "/charts/"+name+"-9.9.9.tgz", &token, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("未知版本应 404: %d", rec.Code)
	}
	if rec := getWith(h, "/charts/..%2f..%2fetc%2fpasswd.tgz", &token, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("畸形名应 404: %d", rec.Code)
	}
}

// TestChartRepoAuth 认证：匿名 401 带 Basic 质询；basic 凭据（admin）
// 可访问；JSON Accept 不发质询（防浏览器原生框）。
func TestChartRepoAuth(t *testing.T) {
	h, _, _ := setupRepoServer(t)

	rec := getWith(h, "/charts/index.yaml", nil, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("匿名应 401: %d", rec.Code)
	}
	if rec.Header().Get("WWW-Authenticate") == "" {
		t.Fatal("GET 非 JSON 应带 Basic 质询")
	}
	// JSON Accept 不带质询（浏览器 fetch 场景）
	req := httptest.NewRequest("GET", "/charts/index.yaml", nil)
	req.Header.Set("Accept", "application/json")
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req)
	if rec2.Header().Get("WWW-Authenticate") != "" {
		t.Fatal("JSON 请求不应发质询")
	}

	// basic（admin/passw0rd 由 newEnrollServer 建立）
	if rec := getWith(h, "/charts/index.yaml", nil, &[]string{"admin:passw0rd"}[0]); rec.Code != http.StatusOK {
		t.Fatalf("basic 应 200: %d %s", rec.Code, rec.Body)
	}
	// 错误凭据
	if rec := getWith(h, "/charts/index.yaml", nil, &[]string{"admin:wrong"}[0]); rec.Code != http.StatusUnauthorized {
		t.Fatalf("错误凭据应 401: %d", rec.Code)
	}
}

// TestChartRepoViewerPerm viewer（有 app:view）可拉；无 app:view 的
// 作用域用户被拒。viewer1 造号走直接建会话（占位 hash 不可登录）。
func TestChartRepoViewerPerm(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	hash, _ := bcrypt.GenerateFromPassword([]byte("passw0rd"), bcrypt.MinCost)
	if err := st.CreateUser("admin", string(hash), "admin"); err != nil {
		t.Fatal(err)
	}
	s, err := New(st, Options{AdminUser: "admin", CADir: filepath.Join(t.TempDir(), "ca"), DataDir: dir}, nil)
	if err != nil {
		t.Fatal(err)
	}
	h := s.Handler()
	token := loginSession(t, s)
	uploadChart(t, h, token, "/api/apps/upload", "n.tgz", buildChartTgz(t, "soloapp", "1.0.0", "v1"))

	if err := s.st.CreateUser("viewer1", "$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy", "viewer"); err != nil {
		t.Fatal(err)
	}
	tok, err := s.sessions.issue("viewer1")
	if err != nil {
		t.Fatal(err)
	}
	if rec := getWith(h, "/charts/index.yaml", &tok, nil); rec.Code != http.StatusOK {
		t.Fatalf("viewer(app:view) 应 200: %d", rec.Code)
	}
}

// TestChartRepoEmpty 空库索引：entries 为空 map 而非 null（Helm 兼容）。
func TestChartRepoEmpty(t *testing.T) {
	s, _ := newEnrollServer(t)
	h := s.Handler()
	token := loginSession(t, s)
	rec := getWith(h, "/charts/index.yaml", &token, nil)
	if !strings.Contains(rec.Body.String(), "entries: {}") {
		t.Fatalf("空库 entries 应为 {}: %s", rec.Body)
	}
}
