package datalock

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLockExclusive 同目录二次加锁失败且错误指明「已被占用」；
// 不同目录互不影响；进程语义（同进程再锁同文件——flock 按 open file
// description 计，第二次 open 也会冲突）由集成层面保证，这里只验
// 跨句柄互斥。
func TestLockExclusive(t *testing.T) {
	dir := t.TempDir()

	f1, err := Lock(dir)
	if err != nil {
		t.Fatalf("首次加锁应成功: %v", err)
	}
	defer f1.Close()

	_, err = Lock(dir)
	if err == nil {
		t.Fatal("同目录二次加锁应失败")
	}
	if !strings.Contains(err.Error(), "already in use") {
		t.Fatalf("错误应指明占用与出路: %v", err)
	}

	// 不同目录不互斥
	other := t.TempDir()
	f2, err := Lock(other)
	if err != nil {
		t.Fatalf("不同目录应可锁: %v", err)
	}
	defer f2.Close()

	// 锁文件落在目录内且属主可读写
	fi, err := os.Stat(filepath.Join(dir, ".wdp.lock"))
	if err != nil {
		t.Fatalf("锁文件应存在: %v", err)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		t.Fatalf("锁文件不应对他人开放: %v", fi.Mode())
	}
}

// TestLockMissingDir 目录不存在 → 明确报错（不隐式创建：启动路径上
// MkdirAll 在前，走到这里说明配置错了）。
func TestLockMissingDir(t *testing.T) {
	if _, err := Lock(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Fatal("不存在目录应报错")
	}
}
