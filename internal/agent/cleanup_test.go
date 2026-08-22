package agent

import (
	"os"
	"path/filepath"
	"testing"

	"wdp/internal/ca"
)

// copyFile 复制文件到目标路径（测试搭建 push 自举产物的临时命名）。
func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestCleanupFilesRemovesArtifacts 验证 cleanup-on-shutdown 的自删范围：
// push 自举产物（二进制 + 同后缀证书三件套，/tmp 下 .wdp-agent-* 命名）全部删除。
// os.Args[0] 临时指向假二进制，避免测试误删真实测试二进制。
func TestCleanupFilesRemovesArtifacts(t *testing.T) {
	dir := t.TempDir()
	if _, _, _, err := ca.Init(dir, ""); err != nil {
		t.Fatal(err)
	}
	srvCrt, srvKey, _, err := ca.Issue(ca.IssueOptions{Dir: dir}, "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}

	// push 自举产物命名：与二进制同目录、同 .wdp-agent-<suffix> 前缀
	bin := filepath.Join(dir, ".wdp-agent-ab12cd34")
	crt := bin + ".crt"
	key := bin + ".key"
	caF := bin + ".ca"
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	copyFile(t, srvCrt, crt)
	copyFile(t, srvKey, key)
	copyFile(t, filepath.Join(dir, ca.CAFile), caF)

	s := New(":0")
	if err := s.ConfigureAuth(caF, crt, key); err != nil {
		t.Fatal(err)
	}

	oldArgs := os.Args
	os.Args = []string{bin}
	s.cleanupFiles()
	os.Args = oldArgs

	for _, f := range []string{bin, crt, key, caF} {
		if _, err := os.Stat(f); !os.IsNotExist(err) {
			t.Errorf("%s 应被删除", f)
		}
	}
}

// TestCleanupFilesSparesShared 验证非 push 产物路径不被误删：
// 常驻 agent 误传 --cleanup-on-shutdown 时，共享 CA（如 /etc/wdp/ca.crt）
// 与安装目录下的二进制必须原样保留。
func TestCleanupFilesSparesShared(t *testing.T) {
	dir := t.TempDir()
	if _, _, _, err := ca.Init(dir, ""); err != nil {
		t.Fatal(err)
	}
	srvCrt, srvKey, _, err := ca.Issue(ca.IssueOptions{Dir: dir}, "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "wdp") // 安装目录下的常驻二进制
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	s := New(":0")
	if err := s.ConfigureAuth(filepath.Join(dir, ca.CAFile), srvCrt, srvKey); err != nil {
		t.Fatal(err)
	}

	oldArgs := os.Args
	os.Args = []string{bin}
	s.cleanupFiles()
	os.Args = oldArgs

	for _, f := range []string{bin, filepath.Join(dir, ca.CAFile), srvCrt, srvKey} {
		if _, err := os.Stat(f); err != nil {
			t.Errorf("%s 不应被删除（非 push 自举产物）: %v", f, err)
		}
	}
}
