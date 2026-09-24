package agent

// 解压内核的测试：正常往返 + 三类越界（路径穿越 / 符号链接逃逸 / 资源上限）。
// ExtractArchive 是 agent 侧唯一处理不可信输入的解析代码（此前零覆盖）。

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// buildTar 构造 tar 字节流（entries 按给定顺序写入）。
func buildTar(t *testing.T, entries []tar.Header, bodies map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, hdr := range entries {
		h := hdr
		if h.Size == 0 && bodies[h.Name] != "" {
			h.Size = int64(len(bodies[h.Name]))
		}
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		if body := bodies[h.Name]; body != "" {
			if _, err := tw.Write([]byte(body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// writeTarGz 把 tar 字节流 gzip 压缩后落盘，返回路径。
func writeTarGz(t *testing.T, raw []byte) string {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "a.tar.gz")
	if err := os.WriteFile(p, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestExtractArchiveRoundTrip(t *testing.T) {
	raw := buildTar(t,
		[]tar.Header{
			{Name: "bin/", Typeflag: tar.TypeDir, Mode: 0o755},
			{Name: "bin/app", Typeflag: tar.TypeReg, Mode: 0o755},
		},
		map[string]string{"bin/app": "BINARY"})
	dest := t.TempDir()
	n, err := ExtractArchive(writeTarGz(t, raw), dest)
	if err != nil {
		t.Fatalf("正常解压失败: %v", err)
	}
	if n != 2 {
		t.Fatalf("条目数 = %d, want 2", n)
	}
	data, err := os.ReadFile(filepath.Join(dest, "bin/app"))
	if err != nil || string(data) != "BINARY" {
		t.Fatalf("内容不符: %q %v", data, err)
	}
}

// TestExtractArchiveRejectsTraversal 条目名含 ".." 必须拒绝，且不写出 dest。
func TestExtractArchiveRejectsTraversal(t *testing.T) {
	raw := buildTar(t,
		[]tar.Header{{Name: "../evil.txt", Typeflag: tar.TypeReg, Mode: 0o644}},
		map[string]string{"../evil.txt": "pwned"})
	parent := t.TempDir()
	dest := filepath.Join(parent, "dest")
	if err := os.Mkdir(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := ExtractArchive(writeTarGz(t, raw), dest); err == nil {
		t.Fatal("路径穿越条目应被拒绝")
	}
	if _, err := os.Stat(filepath.Join(parent, "evil.txt")); !os.IsNotExist(err) {
		t.Fatal("越界文件不应落盘")
	}
}

// TestExtractArchiveRejectsEscapingSymlink 符号链接目标越界必须拒绝。
func TestExtractArchiveRejectsEscapingSymlink(t *testing.T) {
	raw := buildTar(t,
		[]tar.Header{{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "../../etc/passwd"}},
		nil)
	if _, err := ExtractArchive(writeTarGz(t, raw), t.TempDir()); err == nil ||
		!strings.Contains(err.Error(), "out of bounds") {
		t.Fatalf("越界符号链接应被拒绝: %v", err)
	}
}

// TestExtractArchiveRejectsHardlinkThroughSymlink 硬链接源路径经由指向
// dest 外的符号链接解析时必须拒绝：os.Link 跟随符号链接，否则硬链接
// 落在外部 inode 上，随后同名文件条目经它写穿 dest 之外。
func TestExtractArchiveRejectsHardlinkThroughSymlink(t *testing.T) {
	parent := t.TempDir()
	dest := filepath.Join(parent, "dest")
	outside := filepath.Join(parent, "outside")
	for _, d := range []string{dest, outside} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("do-not-touch"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 预置符号链接 dest/d -> outside（模拟历史残留），归档内硬链接
	// target 写成 d/secret.txt
	if err := os.Symlink(outside, filepath.Join(dest, "d")); err != nil {
		t.Fatal(err)
	}
	raw := buildTar(t,
		[]tar.Header{{Name: "x", Typeflag: tar.TypeLink, Linkname: "d/secret.txt", Mode: 0o644}},
		nil)
	if _, err := ExtractArchive(writeTarGz(t, raw), dest); err == nil {
		t.Fatal("经符号链接解析的硬链接应被拒绝")
	}
	// dest 内不得出现指向 secret inode 的硬链接条目
	if _, err := os.Lstat(filepath.Join(dest, "x")); !os.IsNotExist(err) {
		t.Fatal("硬链接条目不应落盘")
	}
	if b, err := os.ReadFile(secret); err != nil || string(b) != "do-not-touch" {
		t.Fatalf("外部文件不应被触碰: %q %v", b, err)
	}
}

// TestExtractArchiveByteLimit 解压总量超限必须失败（解压炸弹防护）。
func TestExtractArchiveByteLimit(t *testing.T) {
	oldBytes, oldEntries := maxExtractBytes, maxExtractEntries
	defer func() { maxExtractBytes, maxExtractEntries = oldBytes, oldEntries }()
	maxExtractBytes = 16

	raw := buildTar(t,
		[]tar.Header{{Name: "big.bin", Typeflag: tar.TypeReg, Mode: 0o644}},
		map[string]string{"big.bin": strings.Repeat("x", 64)})
	if _, err := ExtractArchive(writeTarGz(t, raw), t.TempDir()); err == nil ||
		!strings.Contains(err.Error(), "extraction limit") {
		t.Fatalf("超总量应报错: %v", err)
	}
}

// TestExtractArchiveEntryLimit 条目数超限必须失败（inode 耗尽防护）。
func TestExtractArchiveEntryLimit(t *testing.T) {
	oldBytes, oldEntries := maxExtractBytes, maxExtractEntries
	defer func() { maxExtractBytes, maxExtractEntries = oldBytes, oldEntries }()
	maxExtractEntries = 1

	raw := buildTar(t,
		[]tar.Header{
			{Name: "a.txt", Typeflag: tar.TypeReg, Mode: 0o644},
			{Name: "b.txt", Typeflag: tar.TypeReg, Mode: 0o644},
		},
		map[string]string{"a.txt": "a", "b.txt": "b"})
	if _, err := ExtractArchive(writeTarGz(t, raw), t.TempDir()); err == nil ||
		!strings.Contains(err.Error(), "entry limit") {
		t.Fatalf("超条目数应报错: %v", err)
	}
}

// TestExtractZipLyingSizeRejected：zip 中央目录声明的未压大小可伪造
// （声明 1 字节、deflate 流实际展开 256 字节）。Go 标准库（go1.22.7+
// 的 zip 加固）会在读取超过声明量时返回 ErrFormat——本测试锁定整条
// 防线：伪造声明大小的 zip 绝不能解压成功（无论由哪一层拦截）；
// 预算按实际写入字节计（writeFileLimited）是同方向的纵深防御。
func TestExtractZipLyingSizeRejected(t *testing.T) {
	// 正常构造 zip，再改写中央目录记录的未压大小字段（偏移 24）为 1
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.CreateHeader(&zip.FileHeader{Name: "lie.bin", Method: zip.Deflate})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(strings.Repeat("x", 256))); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	data := buf.Bytes()
	sig := []byte{0x50, 0x4b, 0x01, 0x02} // 中央目录记录签名（单条目，唯一）
	i := bytes.LastIndex(data, sig)
	if i < 0 {
		t.Fatal("central directory record not found")
	}
	binary.LittleEndian.PutUint32(data[i+24:i+28], 1)

	src := filepath.Join(t.TempDir(), "lie.zip")
	if err := os.WriteFile(src, data, 0o644); err != nil {
		t.Fatal(err)
	}
	dest := t.TempDir()
	if _, err := ExtractArchive(src, dest); err == nil {
		t.Fatal("伪造声明大小的 zip 必须解压失败")
	}
	if _, err := os.Stat(filepath.Join(dest, "lie.bin")); !os.IsNotExist(err) {
		t.Fatal("失败时不应残留半截文件")
	}
}

// TestExtractZipByteLimitActual：zip 分支的解压预算按实际写入字节累计
// （诚实声明大小的多文件场景，writeFileLimited 的记账路径）。
func TestExtractZipByteLimitActual(t *testing.T) {
	oldBytes := maxExtractBytes
	defer func() { maxExtractBytes = oldBytes }()
	maxExtractBytes = 32

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.CreateHeader(&zip.FileHeader{Name: "big.bin", Method: zip.Deflate})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(strings.Repeat("x", 256))); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(t.TempDir(), "big.zip")
	if err := os.WriteFile(src, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ExtractArchive(src, t.TempDir()); err == nil ||
		!strings.Contains(err.Error(), "extraction limit") {
		t.Fatalf("实际写入超预算应报错: %v", err)
	}
}
