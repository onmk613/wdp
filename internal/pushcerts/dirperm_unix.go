//go:build unix

package pushcerts

import (
	"io/fs"
	"syscall"
)

// dirOwnerUID 返回目录属主的 uid（非 Unix 平台见 dirperm_other.go）。
func dirOwnerUID(fi fs.FileInfo) (uint32, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return st.Uid, true
}
