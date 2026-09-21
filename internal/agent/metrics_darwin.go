//go:build darwin

package agent

// darwin 演练环境的 sysctl 兜底（metrics.go 的 /proc 路径在 darwin 不存在）。
// sysctl 系列是 darwin 专属符号，按 build tag 隔离保证交叉编译。

import (
	"encoding/binary"

	"golang.org/x/sys/unix"
)

// darwinMemTotal 物理内存总量（hw.memsize，字节）。
func darwinMemTotal() (uint64, bool) {
	total, err := unix.SysctlUint64("hw.memsize")
	if err != nil || total == 0 {
		return 0, false
	}
	return total, true
}

// darwinBootSecs 内核启动时刻（kern.boottime 的 timeval.tv_sec，小端 8 字节）。
func darwinBootSecs() (uint64, bool) {
	b, err := unix.Sysctl("kern.boottime")
	if err != nil || len(b) < 8 {
		return 0, false
	}
	return binary.LittleEndian.Uint64([]byte(b[:8])), true
}
