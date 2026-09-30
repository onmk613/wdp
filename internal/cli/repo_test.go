package cli

// wdp repo 客户端与 run --repo 兜底的往返测试：真起 httptest server
//（复用 web 包的 chart 仓库端点），验证索引解析、digest 校验（含篡改
// 拒绝）、解包形态（chart.yaml 落位）与 --repo 自动拉取缓存。

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"wdp/internal/store"
	"wdp/internal/web"
)

// buildMiniTgz CLI 侧最小 chart 包（顶层 <name>/ 目录规范）。
func buildMiniTgz(t *testing.T, name, version string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	write := func(p, body string) {
		if err := tw.WriteHeader(&tar.Header{Name: p, Mode: 0o644, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	write(name+"/chart.yaml", fmt.Sprintf("name: %s\nversion: %s\nrequired: []\n", name, version))
	write(name+"/deploy.yaml", "- name: e2e\n  hosts: all\n  tasks:\n    - name: say\n      shell: 'echo hi'\n")
	write(name+"/values.yaml", "{}\n")
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// testJar 最小 cookie jar（登录会话）。
type testJar struct{ cookies []*http.Cookie }

func (j *testJar) SetCookies(_ *url.URL, cs []*http.Cookie) { j.cookies = append(j.cookies, cs...) }
func (j *testJar) Cookies(_ *url.URL) []*http.Cookie        { return j.cookies }

// startRepoTestServer 起 web server 并上传 <name> 的 1.0.0/1.2.0 两版本；
// 返回服务基址。凭据 admin/passw0rd。
func startRepoTestServer(t *testing.T, name string) string {
	t.Helper()
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
	s, err := web.New(st, web.Options{AdminUser: "admin", CADir: filepath.Join(t.TempDir(), "ca"), DataDir: dir}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	cl := &http.Client{Jar: &testJar{}}
	lr, err := cl.Post(ts.URL+"/api/login", "application/json", strings.NewReader(`{"User":"admin","Password":"passw0rd"}`))
	if err != nil || lr.StatusCode != http.StatusOK {
		t.Fatalf("login: %v %d", err, lr.StatusCode)
	}
	lr.Body.Close()
	for _, v := range []string{"1.0.0", "1.2.0"} {
		body := &bytes.Buffer{}
		mw := multipart.NewWriter(body)
		part, err := mw.CreateFormFile("tgz", name+".tgz")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write(buildMiniTgz(t, name, v)); err != nil {
			t.Fatal(err)
		}
		if err := mw.Close(); err != nil {
			t.Fatal(err)
		}
		resp, err := cl.Post(ts.URL+"/api/apps/upload", mw.FormDataContentType(), body)
		if err != nil {
			t.Fatal(err)
		}
		rb := &bytes.Buffer{}
		rb.ReadFrom(resp.Body)
		resp.Body.Close()
		if resp.StatusCode >= 300 {
			t.Fatalf("上传 %s: HTTP %d %s", v, resp.StatusCode, rb)
		}
	}
	return ts.URL
}

// TestRepoFlagsResolve --repo 归一化：裸基址补 /charts、缺凭据报错。
func TestRepoFlagsResolve(t *testing.T) {
	f := repoFlags{base: "http://s:7603", auth: "u:p"}
	base, _, err := f.resolve()
	if err != nil || base != "http://s:7603/charts" {
		t.Fatalf("裸基址应补 /charts: %q %v", base, err)
	}
	if _, _, err := (&repoFlags{}).resolve(); err == nil {
		t.Fatal("缺 --repo 应报错")
	}
	if _, _, err := (&repoFlags{base: "http://s", auth: "nopass-colon"}).resolve(); err == nil {
		t.Fatal("凭据非 user:pass 应报错")
	}
}

// TestRepoFetchIndexAndEntry 索引拉取 + 版本解析（最新/指定/未知）。
func TestRepoFetchIndexAndEntry(t *testing.T) {
	root := startRepoTestServer(t, "webapp")
	idx, err := fetchRepoIndex(root+"/charts", "admin:passw0rd")
	if err != nil {
		t.Fatal(err)
	}
	latest, err := resolveEntry(idx, "webapp", "")
	if err != nil || latest.Version != "1.2.0" {
		t.Fatalf("缺省应最新: %+v %v", latest, err)
	}
	pinned, err := resolveEntry(idx, "webapp", "1.0.0")
	if err != nil || pinned.Version != "1.0.0" {
		t.Fatalf("指定版本: %+v %v", pinned, err)
	}
	if _, err := resolveEntry(idx, "webapp", "9.9.9"); err == nil || !strings.Contains(err.Error(), "available") {
		t.Fatalf("未知版本应报可用清单: %v", err)
	}
	// 错误凭据被拒
	if _, err := fetchRepoIndex(root+"/charts", "admin:wrong"); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("错误凭据应 401: %v", err)
	}
}

// TestRepoDownloadDigestVerify 下载内容 digest 校验；索引被篡改时拒绝。
func TestRepoDownloadDigestVerify(t *testing.T) {
	root := startRepoTestServer(t, "verifyapp")
	idx, err := fetchRepoIndex(root+"/charts", "admin:passw0rd")
	if err != nil {
		t.Fatal(err)
	}
	e, err := resolveEntry(idx, "verifyapp", "1.2.0")
	if err != nil {
		t.Fatal(err)
	}
	tmp, got, err := downloadChart(root+"/charts", "admin:passw0rd", e)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmp)
	if len(got) != 64 {
		t.Fatalf("digest 应为 64 hex: %s", got)
	}
	// 篡改 digest → 拒绝
	bad := *e
	bad.Digest = "sha256:" + strings.Repeat("0", 64)
	if _, _, err := downloadChart(root+"/charts", "admin:passw0rd", &bad); err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("篡改 digest 应拒绝: %v", err)
	}
}

// TestRepoFetchInto run --repo 兜底：拉取解包成 charts/<name>/ 目录，
// chart.yaml 落位可被 chart.Load 消费；重复调用命中缓存不再联网。
func TestRepoFetchInto(t *testing.T) {
	root := startRepoTestServer(t, "cacheapp")
	dir := t.TempDir()
	dest := filepath.Join(dir, "charts", "cacheapp")

	rf := &repoFetch{base: root + "/charts", auth: "admin:passw0rd"}
	if err := rf.fetchInto("cacheapp", "", dest); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dest, "chart.yaml")); err != nil {
		t.Fatalf("解包后应有 chart.yaml: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "deploy.yaml")); err != nil {
		t.Fatalf("解包后应有 deploy.yaml: %v", err)
	}
	// 指定 @版本（本地已有缓存时 run 不再走网络——这里直接验证 atVer
	// 参数解析路径：清掉缓存重新拉指定版本）
	if err := os.RemoveAll(dest); err != nil {
		t.Fatal(err)
	}
	if err := rf.fetchInto("cacheapp", "1.0.0", dest); err != nil {
		t.Fatal(err)
	}
	cy, err := os.ReadFile(filepath.Join(dest, "chart.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(cy), "version: 1.0.0") {
		t.Fatalf("@1.0.0 应拉到指定版本:\n%s", cy)
	}
}
