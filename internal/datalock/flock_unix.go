//go:build unix

package datalock

import (
	"os"
	"syscall"
)

// lockExclusive 对锁文件加排他 flock（非阻塞：占用即失败）。
func lockExclusive(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}
