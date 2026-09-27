//go:build windows

package conn

// brokenSyscall 按 errno 类判定连接失效（windows）。windows syscall 包的
// WSA errno 经 errors.Is 判定；unix 专属常量此处不可用。

import (
	"errors"
	"syscall"
)

func brokenSyscall(err error) bool {
	return errors.Is(err, syscall.WSAECONNRESET) ||
		errors.Is(err, syscall.WSAECONNABORTED) ||
		errors.Is(err, syscall.ETIMEDOUT)
}
