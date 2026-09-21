//go:build !unix

package datalock

import (
	"errors"
	"os"
)

// lockExclusive 非 Unix 平台退化为「锁文件已存在即视为占用」：无 flock
// 语义，但能挡住最常见的同机双开（启动崩溃残留的锁文件需要手动删——
// 错误信息里说明）。windows 目标以服务管理器的单实例约束为主。
func lockExclusive(f *os.File) error {
	if _, err := os.Stat(f.Name() + ".held"); err == nil {
		return errors.New("lock marker exists")
	}
	if err := os.WriteFile(f.Name()+".held", []byte("1"), 0o600); err != nil {
		return err
	}
	return nil
}
