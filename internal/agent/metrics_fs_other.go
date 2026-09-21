//go:build !darwin && !linux

package agent

// 非 unix 平台的文件系统桩（windows 无 statfs 符号；生产环境是 Linux）。

type fsStatsOut struct {
	sizeBytes  float64
	availBytes float64
	files      float64
	filesFree  float64
}

func fsStats(string) (fsStatsOut, bool) { return fsStatsOut{}, false }
