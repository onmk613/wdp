package console

// spec 读取的符号链接回归：ReadSpecFromDir 走 filepath.WalkDir，
// 符号链接的 d.IsDir() 为 false，os.ReadFile 却会跟随它——不加过滤
// 就等于把链接目标的内容（解包侧本已挡住的越界链接，或其他渠道放进
// chart 目录的链接）原文回给读 spec 的调用方。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeMinimalChart(t *testing.T, dir string) {
	t.Helper()
	files := map[string]string{
		"chart.yaml":             "name: demo\nversion: 0.1.0\n",
		"values.yaml":            "app:\n  port: 8080\n",
		"deploy.yaml":            "- name: p\n  hosts: all\n  tasks: []\n",
		"templates/app.conf.tpl": "port={{ .app.port }}\n",
	}
	for rel, body := range files {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestReadSpecFromDirSkipsSymlink(t *testing.T) {
	secret := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(secret, []byte("TOP-SECRET-CONTENT"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	writeMinimalChart(t, dir)
	if err := os.Symlink(secret, filepath.Join(dir, "leak.txt")); err != nil {
		t.Skipf("符号链接不可用: %v", err)
	}

	spec, err := ReadSpecFromDir(dir, "demo", "0.1.0", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range spec.Files {
		if f.Content != nil && strings.Contains(*f.Content, "TOP-SECRET-CONTENT") {
			t.Fatalf("符号链接目标内容被读出: %s", f.Path)
		}
		if f.Path == "leak.txt" {
			t.Fatalf("符号链接不应出现在 spec.files 里: %+v", f)
		}
	}
	// 正常文件仍在（避免误杀）
	var found bool
	for _, f := range spec.Files {
		if f.Path == "templates/app.conf.tpl" && f.Content != nil {
			found = true
		}
	}
	if !found {
		t.Fatalf("普通文本文件应照常读出: %+v", spec.Files)
	}
}

// TestCopyDirSkipsSymlink 保存副本时同样不跟随链接：否则链接目标内容
// 会被写进新版本制品（其 Lstat 权限位还是 0777），可再经下载端点外带。
func TestCopyDirSkipsSymlink(t *testing.T) {
	secret := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(secret, []byte("TOP-SECRET-CONTENT"), 0o600); err != nil {
		t.Fatal(err)
	}
	src, dst := t.TempDir(), t.TempDir()
	writeMinimalChart(t, src)
	if err := os.Symlink(secret, filepath.Join(src, "leak.txt")); err != nil {
		t.Skipf("符号链接不可用: %v", err)
	}

	if err := CopyDir(src, dst); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(dst, "leak.txt")); err == nil {
		t.Fatalf("符号链接不应被复制到目标目录")
	}
	if _, err := os.Stat(filepath.Join(dst, "values.yaml")); err != nil {
		t.Fatalf("普通文件应照常复制: %v", err)
	}
}
