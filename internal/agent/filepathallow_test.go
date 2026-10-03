package agent

// /file、/archive 领地白名单（--allow-file-path）的回归：这两个端点对持
// 客户端证书的一方是"任意路径读写"原语，/shutdown 的 files 早已收紧到
// agent 领地（clean.go），本开关把文件面收窄到配置的目录树——启用后
// 越界读写 403、界内正常；未启用（缺省）保持既有行为。

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// postArchive 调用 POST /archive 并返回状态码。
func postArchive(base, src, dest string) int {
	resp, err := http.Post(base+"/archive?src="+url.QueryEscape(src)+"&dest="+url.QueryEscape(dest), "", nil)
	if err != nil {
		return -1
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// TestFilePathRootsValidation setter 拒绝相对路径与文件系统根。
func TestFilePathRootsValidation(t *testing.T) {
	s := New(":0")
	if err := s.SetFilePathRoots(nil); err != nil {
		t.Fatalf("空清单应合法（= 不启用）: %v", err)
	}
	if len(s.fileRoots) != 0 {
		t.Fatal("空清单不应留下根目录")
	}
	if err := s.SetFilePathRoots([]string{"relative/path"}); err == nil {
		t.Fatal("相对路径应拒绝")
	}
	if err := s.SetFilePathRoots([]string{"/"}); err == nil {
		t.Fatal("文件系统根应拒绝")
	}
	if err := s.SetFilePathRoots([]string{"/opt/wdp", "  "}); err != nil {
		t.Fatalf("空白项应跳过: %v", err)
	}
	if len(s.fileRoots) != 1 || s.fileRoots[0] != "/opt/wdp" {
		t.Fatalf("应清洗保留有效根: %v", s.fileRoots)
	}
}

// TestFilePathAllowedLexical 前缀判定是词法口径（含尾部斜杠、.. 归一、
// 假前缀 /opt/wdpx 不误放行）。
func TestFilePathAllowedLexical(t *testing.T) {
	s := New(":0")
	if err := s.SetFilePathRoots([]string{"/opt/wdp/"}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/opt/wdp", "/opt/wdp/a.bin", "/opt/wdp///deep//x", "/opt/wdp/../wdp/y"} {
		if !s.filePathAllowed(p) {
			t.Fatalf("%s 应在领地内", p)
		}
	}
	for _, p := range []string{"/opt/wdpx/a.bin", "/etc/shadow", "/opt", "relative"} {
		if s.filePathAllowed(p) {
			t.Fatalf("%s 应在领地外（假前缀不得放行）", p)
		}
	}
	// 未启用时恒允许（既有行为）
	s2 := New(":0")
	if !s2.filePathAllowed("/etc/shadow") {
		t.Fatal("未启用领地时应保持既有行为（允许）")
	}
}

// TestFileEndpointsTerritory 端点行为：领地内上传/下载/解压正常，越界
// 403 且不产生任何文件系统副作用。
func TestFileEndpointsTerritory(t *testing.T) {
	terr := t.TempDir()    // 领地内
	outside := t.TempDir() // 领地外
	s := New(":0")
	if err := s.SetFilePathRoots([]string{terr}); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	put := func(dst string) int {
		t.Helper()
		req, err := http.NewRequest(http.MethodPut, ts.URL+"/file?path="+url.QueryEscape(dst), strings.NewReader("x"))
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.StatusCode
	}
	get := func(src string) int {
		t.Helper()
		resp, err := http.Get(ts.URL + "/file?path=" + url.QueryEscape(src))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.StatusCode
	}

	// 上传：界内 200，界外 403 且不建目录不落盘
	inside := filepath.Join(terr, "sub", "a.bin")
	if code := put(inside); code != http.StatusOK {
		t.Fatalf("领地内上传应 200: %d", code)
	}
	if _, err := os.Stat(inside); err != nil {
		t.Fatalf("领地内上传应落盘: %v", err)
	}
	esc := filepath.Join(outside, "esc", "a.bin")
	if code := put(esc); code != http.StatusForbidden {
		t.Fatalf("领地外上传应 403: %d", code)
	}
	if _, err := os.Stat(filepath.Dir(esc)); !os.IsNotExist(err) {
		t.Fatalf("被拒上传不应建目录: %v", err)
	}

	// 下载：界内 200，界外（如 agent 私钥）403
	if code := get(inside); code != http.StatusOK {
		t.Fatalf("领地内下载应 200: %d", code)
	}
	if code := get("/etc/shadow"); code != http.StatusForbidden {
		t.Fatalf("领地外下载应 403（任意读收紧面）: %d", code)
	}

	// 解压：src 与 dest 任一越界即 403（src 越界 = 借解压读领地外归档）
	zipInside := filepath.Join(terr, "pkg.zip")
	if code := put(zipInside); code != http.StatusOK {
		t.Fatalf("预置归档失败: %d", code)
	}
	if code := postArchive(ts.URL, zipInside, filepath.Join(terr, "x")); code != http.StatusOK && code != http.StatusInternalServerError {
		t.Fatalf("领地内解压不应被领地拒绝: %d", code)
	}
	if code := postArchive(ts.URL, zipInside, filepath.Join(outside, "d")); code != http.StatusForbidden {
		t.Fatalf("dest 越界应 403: %d", code)
	}
	if code := postArchive(ts.URL, "/etc/secret.zip", filepath.Join(terr, "y")); code != http.StatusForbidden {
		t.Fatalf("src 越界应 403: %d", code)
	}
}
