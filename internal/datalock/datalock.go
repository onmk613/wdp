// Package datadirlock 提供数据目录的进程级排他锁：server 的全部可变
// 状态（wdp.db、CA、版本制品）都在单实例假设下运作——SQLite 多写、
// 内存会话表、上传互斥锁、执行闸门都是进程内的。此前第二个实例对同
// 一目录静默双开，两份 SQLite 写同一文件（偶发 database is locked /
// 静默丢失更新），内存态各自为政。启动时在目录上持 flock，重复启动
// 立即失败并指明持有者。
//
// 文件锁的生存期 = 进程生存期（不主动释放，进程退出内核回收）：与
// "实例活着才允许写数据目录" 的语义完全一致，崩溃也不会留死锁。
package datalock

import (
	"fmt"
	"os"
	"path/filepath"
)

// Lock 在 dir 上创建排他锁文件并锁定；成功返回保持到进程退出的句柄。
// 失败（目录不存在不可创建 / 已被锁定）返回带操作指引的错误。
func Lock(dir string) (*os.File, error) {
	fi, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("data dir: %w", err)
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("data dir %s is not a directory", dir)
	}
	f, err := os.OpenFile(filepath.Join(dir, ".wdp.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	if err := lockExclusive(f); err != nil {
		f.Close()
		return nil, fmt.Errorf(
			"data directory %s is already in use by another wdp server (lock held); "+
				"stop the other instance first or use a different --data dir: %w", dir, err)
	}
	// 记录持有者（排障用；锁本身以 flock 为准，写失败不影响互斥）
	_, _ = f.WriteAt([]byte(fmt.Sprintf("pid=%d\n", os.Getpid())), 0)
	return f, nil
}
