// Package buildinfo 承载构建期注入的版本信息（build.sh 经 -X ldflags
// 写入）。独立成叶子包：agent（/health 上报）与 cli（版本命令）都要用，
// 放在任何一个里都会形成 import 环。
package buildinfo

// Version / Commit / BuildDate / GoVersion 由 ldflags 注入；开发构建用缺省值。
var (
	Version   = "0.0.1"
	Commit    = "none"
	BuildDate = "unknown"
	GoVersion = "unknown"
)

// BuildVersion 返回发布版本标识（Version-commit 短哈希）。远程 agent
// 升级用它比较新旧；server 与 agent 同源构建，值一致即同版本。
func BuildVersion() string {
	commit := Commit
	if len(commit) > 7 {
		commit = commit[:7]
	}
	return Version + "-" + commit
}
