//go:build darwin || linux

package agent

// statfs 平台实现（unix 家族共用；符号在 windows 缺失，见 metrics_fs_other.go）。

import "golang.org/x/sys/unix"

// fsStatsOut 是一个挂载点的容量快照（字节/inode）。
type fsStatsOut struct {
	sizeBytes  float64
	availBytes float64
	files      float64
	filesFree  float64
}

// fsStats 查询挂载点的文件系统统计；不可得返回 ok=false（采集器跳过该项）。
func fsStats(mount string) (fsStatsOut, bool) {
	var st unix.Statfs_t
	if err := unix.Statfs(mount, &st); err != nil {
		return fsStatsOut{}, false
	}
	bsz := float64(st.Bsize)
	return fsStatsOut{
		sizeBytes:  bsz * float64(st.Blocks),
		availBytes: bsz * float64(st.Bavail),
		files:      float64(st.Files),
		filesFree:  float64(st.Ffree),
	}, true
}
