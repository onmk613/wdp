package agent

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ulikunitz/xz"
)

// tarEntry 描述测试归档中的一个条目。
type tarEntry struct {
	name     string
	typeflag byte // 0 = 自动按有无 body/linkname 推断
	mode     int64
	body     string
	linkname string
}

// buildTar 构造内存 tar（压缩由 wrap 决定）。
func buildTar(t *testing.T, entries []tarEntry, wrap func(io.Writer) io.WriteCloser) []byte {
	t.Helper()
	var buf bytes.Buffer
	var w io.Writer = &buf
	var closer io.Closer
	if wrap != nil {
		c := wrap(&buf)
		w, closer = c, c
	}
	tw := tar.NewWriter(w)
	for _, e := range entries {
		flag := e.typeflag
		if flag == 0 {
			switch {
			case e.linkname != "":
				flag = tar.TypeSymlink
			case strings.HasSuffix(e.name, "/"):
				flag = tar.TypeDir
			default:
				flag = tar.TypeReg
			}
		}
		mode := e.mode
		if mode == 0 {
			mode = 0o644
		}
		hdr := &tar.Header{Name: e.name, Mode: mode, Typeflag: flag, Linkname: e.linkname, Size: int64(len(e.body))}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if e.body != "" {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if closer != nil {
		if err := closer.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return buf.Bytes()
}

func writeTemp(t *testing.T, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "arc")
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestExtractTarGZ(t *testing.T) {
	arc := writeTemp(t, buildTar(t, []tarEntry{
		{name: "sub/", mode: 0o755},
		{name: "sub/app.sh", mode: 0o755, body: "#!/bin/sh\necho hi\n"},
		{name: "README", body: "hello"},
		{name: "latest", linkname: "sub/app.sh"},
	}, func(w io.Writer) io.WriteCloser { return gzip.NewWriter(w) }))

	dest := filepath.Join(t.TempDir(), "out")
	n, err := ExtractArchive(arc, dest)
	if err != nil || n != 4 {
		t.Fatalf("解压失败: n=%d err=%v", n, err)
	}
	got, err := os.ReadFile(filepath.Join(dest, "sub", "app.sh"))
	if err != nil || string(got) != "#!/bin/sh\necho hi\n" {
		t.Fatalf("app.sh 内容: %q err=%v", got, err)
	}
	fi, _ := os.Stat(filepath.Join(dest, "sub", "app.sh"))
	if fi.Mode().Perm() != 0o755 {
		t.Fatalf("权限未保留: %o", fi.Mode().Perm())
	}
	ln, err := os.Readlink(filepath.Join(dest, "latest"))
	if err != nil || ln != "sub/app.sh" {
		t.Fatalf("符号链接: %q err=%v", ln, err)
	}
}

func TestExtractPlainTarAndXZ(t *testing.T) {
	entries := []tarEntry{{name: "a.txt", body: "x"}}
	// 裸 tar（无压缩，走魔数兜底分支）
	arc := writeTemp(t, buildTar(t, entries, nil))
	if _, err := ExtractArchive(arc, filepath.Join(t.TempDir(), "o1")); err != nil {
		t.Fatal(err)
	}
	// tar.xz
	arcXZ := writeTemp(t, buildTar(t, entries, func(w io.Writer) io.WriteCloser {
		xw, err := xz.NewWriter(w)
		if err != nil {
			t.Fatal(err)
		}
		return xw
	}))
	dest := filepath.Join(t.TempDir(), "o2")
	if _, err := ExtractArchive(arcXZ, dest); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(dest, "a.txt")); string(got) != "x" {
		t.Fatalf("xz 内容: %q", got)
	}
}

func TestExtractZip(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	mw, _ := zw.Create("sub/app.conf")
	mw.Write([]byte("k=v\n"))
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	arc := writeTemp(t, buf.Bytes())
	dest := filepath.Join(t.TempDir(), "out")
	if _, err := ExtractArchive(arc, dest); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(dest, "sub", "app.conf")); string(got) != "k=v\n" {
		t.Fatalf("zip 内容: %q", got)
	}
}

// TestExtractRejectsTraversal 含 ".." 的条目必须拒绝。
func TestExtractRejectsTraversal(t *testing.T) {
	arc := writeTemp(t, buildTar(t, []tarEntry{{name: "../evil", body: "x"}}, nil))
	dest := filepath.Join(t.TempDir(), "out")
	if _, err := ExtractArchive(arc, dest); err == nil || !strings.Contains(err.Error(), "..") {
		t.Fatalf("应拒绝 .. 条目: %v", err)
	}
}

// TestExtractStripsLeadingSlash 绝对路径条目按 tar 惯例剥除前导 /，落在 dest 内。
func TestExtractStripsLeadingSlash(t *testing.T) {
	arc := writeTemp(t, buildTar(t, []tarEntry{{name: "/etc/evil", body: "x"}}, nil))
	dest := filepath.Join(t.TempDir(), "out")
	if _, err := ExtractArchive(arc, dest); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dest, "etc", "evil")); err != nil {
		t.Fatalf("应剥除前导 / 落在 dest 内: %v", err)
	}
}

// TestExtractSymlinkHijack 目标位置被预置符号链接指向外部时，解压不得写穿：
// 链接被替换为常规文件，外部文件原样。
func TestExtractSymlinkHijack(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("ORIGINAL"), 0o644); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "out")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dest, "hijack")); err != nil {
		t.Fatal(err)
	}
	arc := writeTemp(t, buildTar(t, []tarEntry{{name: "hijack", body: "PWNED"}}, nil))
	if _, err := ExtractArchive(arc, dest); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(outside); string(got) != "ORIGINAL" {
		t.Fatal("外部文件被写穿篡改")
	}
	if got, _ := os.ReadFile(filepath.Join(dest, "hijack")); string(got) != "PWNED" {
		t.Fatalf("应替换为归档内容: %q", got)
	}
}

// TestExtractRejectsSymlinkEscape 指向 dest 外部的符号链接条目必须拒绝。
func TestExtractRejectsSymlinkEscape(t *testing.T) {
	arc := writeTemp(t, buildTar(t, []tarEntry{{name: "s", linkname: "../../outside"}}, nil))
	if _, err := ExtractArchive(arc, filepath.Join(t.TempDir(), "out")); err == nil {
		t.Fatal("越界符号链接应被拒绝")
	}
}

// TestExtractRejectsAbsoluteSymlinkTarget 回归：绝对路径符号链接目标曾因
// safeJoin 剥除前导 "/" 而通过校验、又以原始 Linkname 创建链接，导致链接
// 指向 dest 之外（后续条目即可写穿任意系统路径）。现在必须拒绝。
func TestExtractRejectsAbsoluteSymlinkTarget(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("ORIGINAL"), 0o644); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "out")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	// 攻击链：① mylink → <外部绝对路径>；② 经 mylink 写外部文件
	arc := writeTemp(t, buildTar(t, []tarEntry{
		{name: "mylink", linkname: outside},
		{name: "mylink/passwd", body: "PWNED"},
	}, nil))
	if _, err := ExtractArchive(arc, dest); err == nil {
		t.Fatal("绝对路径符号链接目标应被拒绝")
	}
	if got, _ := os.ReadFile(outside); string(got) != "ORIGINAL" {
		t.Fatal("外部文件被符号链接逃逸写穿篡改")
	}
}

// TestExtractRejectsAbsoluteZipSymlinkTarget 同上，zip 变体。
func TestExtractRejectsAbsoluteZipSymlinkTarget(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("ORIGINAL"), 0o644); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "out")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	hdr := &zip.FileHeader{Name: "mylink", Method: zip.Store}
	hdr.SetMode(0o777 | fs.ModeSymlink)
	w, err := zw.CreateHeader(hdr)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(outside)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	arc := writeTemp(t, buf.Bytes())
	if _, err := ExtractArchive(arc, dest); err == nil {
		t.Fatal("zip 绝对路径符号链接目标应被拒绝")
	}
	if got, _ := os.ReadFile(outside); string(got) != "ORIGINAL" {
		t.Fatal("外部文件被 zip 符号链接逃逸写穿篡改")
	}
}

// TestHandleArchiveEndpoint 端点级验证：真实解压 200、缺参 400。
func TestHandleArchiveEndpoint(t *testing.T) {
	arc := writeTemp(t, buildTar(t, []tarEntry{{name: "f", body: "v"}}, nil))
	ts := httptest.NewServer(New(":0").Handler())
	t.Cleanup(ts.Close)

	resp, err := http.Post(ts.URL+"/archive?src="+arc+"&dest="+filepath.Join(t.TempDir(), "o"), "", nil)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("解压请求失败: %v status=%v", err, resp)
	}
	resp.Body.Close()

	resp2, err := http.Post(ts.URL+"/archive", "", nil)
	if err != nil || resp2.StatusCode != http.StatusBadRequest {
		t.Fatalf("缺参应 400: %v status=%v", err, resp2)
	}
	resp2.Body.Close()
}
