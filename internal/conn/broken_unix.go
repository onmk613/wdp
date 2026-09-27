//go:build !windows

package conn

// brokenSyscall 按 errno 类判定连接失效（unix）。windows 的 syscall errno
// 集不同，见 broken_windows.go。

import (
	"errors"
	"syscall"
)

func brokenSyscall(err error) bool {
	return errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, syscall.ECONNABORTED) ||
		errors.Is(err, syscall.ETIMEDOUT)
}
