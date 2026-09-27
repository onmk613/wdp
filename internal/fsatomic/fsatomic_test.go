package fsatomic

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// errReader 固定返回错误（读失败路径注入用）。
type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

// assertNoTemp 断言目录内无 fsatomic 临时文件残留（失败清理契约）。
func assertNoTemp(t *testing.T, dir string) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, ".wdp-fsatomic-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("目录残留临时文件: %v", matches)
	}
}

// TestWriteFileBasic 基本契约：内容、权限、覆盖旧件、无临时文件残留。
func TestWriteFileBasic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(path, strings.NewReader("new-content"), 0o600); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "new-content" {
		t.Fatalf("内容 = %q, 期望覆盖后的新内容", string(data))
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != fs.FileMode(0o600) {
		t.Fatalf("权限 = %o, 期望 0600（宽松旧件应被收紧）", got)
	}
	assertNoTemp(t, dir)
}

// TestWriteFileReaderError 读中途失败：错误上抛、目标保持旧内容、
// 临时文件被清理。
func TestWriteFileReaderError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("boom")
	r := io.MultiReader(strings.NewReader("partial"), errReader{boom})
	if err := WriteFile(path, r, 0o644); !errors.Is(err, boom) {
		t.Fatalf("应返回读错误, 实际 %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "old" {
		t.Fatalf("失败后目标被改动: %q", string(data))
	}
	assertNoTemp(t, dir)
}

// TestWriteFileMissingDir 父目录缺失直接报错（建目录是调用方职责）。
func TestWriteFileMissingDir(t *testing.T) {
	path := filepath.Join(t.TempDir(), "no-such-dir", "a.txt")
	if err := WriteFile(path, strings.NewReader("x"), 0o644); err == nil {
		t.Fatal("父目录缺失应报错")
	}
}
