package agent

// 解压内核的测试：正常往返 + 三类越界（路径穿越 / 符号链接逃逸 / 资源上限）。
// ExtractArchive 是 agent 侧唯一处理不可信输入的解析代码（此前零覆盖）。

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
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
