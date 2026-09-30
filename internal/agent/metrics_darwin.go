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

// darwinLoadavg 解 vm.loadavg 的二进制 struct loadavg：3×fixpt_t
// （uint32 LE，1/5/15 分钟）+ 对齐填充 + long fscale。实测 arm64 返回
// 23 字节（int64 fscale 的末位零被截去）：fscale 从偏移 16 起零填充读
// （小端缺的是高位零，补零无损）。fscale 非正/长度不足按未支持处理，
// 采集器输出空集不报错。
func darwinLoadavg() (l1, l5, l15 float64, ok bool) {
	s, err := unix.Sysctl("vm.loadavg")
	if err != nil || len(s) < 20 {
		return 0, 0, 0, false
	}
	b := []byte(s)
	var fsb [8]byte
	copy(fsb[:], b[16:])
	fscale := float64(binary.LittleEndian.Uint64(fsb[:]))
	if fscale <= 0 {
		return 0, 0, 0, false
	}
	return float64(binary.LittleEndian.Uint32(b[0:4])) / fscale,
		float64(binary.LittleEndian.Uint32(b[4:8])) / fscale,
		float64(binary.LittleEndian.Uint32(b[8:12])) / fscale, true
}
