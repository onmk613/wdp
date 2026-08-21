package agent

import (
	"os"
	"path/filepath"
	"testing"

	"wdp/internal/ca"
)

// TestCleanupFilesRemovesArtifacts 验证 cleanup-on-shutdown 的自删范围：
// 二进制 + CA/证书/私钥三件套全部删除。
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

	fakeBin := filepath.Join(dir, "fake-wdp-agent")
	if err := os.WriteFile(fakeBin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	s := New(":0")
	if err := s.ConfigureAuth(filepath.Join(dir, ca.CAFile), srvCrt, srvKey); err != nil {
		t.Fatal(err)
	}

	oldArgs := os.Args
	os.Args = []string{fakeBin}
	s.cleanupFiles()
	os.Args = oldArgs

	for _, f := range []string{fakeBin, filepath.Join(dir, ca.CAFile), srvCrt, srvKey} {
		if _, err := os.Stat(f); !os.IsNotExist(err) {
			t.Errorf("%s 应被删除", f)
		}
	}
}
