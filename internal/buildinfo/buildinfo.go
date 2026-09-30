// Package buildinfo 承载构建期注入的版本信息（build.sh 经 -X ldflags
// 写入）。独立成叶子包：agent（/health 上报）与 cli（版本命令）都要用，
// 放在任何一个里都会形成 import 环。
package buildinfo

import "strings"

// Version / Commit / BuildDate / GoVersion / Tier 由 ldflags 注入；开发
// 构建用缺省值。Tier 标识功能档位（full / cli / agent）。
var (
	Version   = "0.0.1"
	Commit    = "none"
	BuildDate = "unknown"
	GoVersion = "unknown"
	Tier      = "full"
)

// BuildVersion 返回发布版本标识（Version-commit 短哈希）。远程 agent
// 升级用它比较新旧；server 与 agent 同源构建，值一致即同版本。
// commit 带点号后缀（build.sh 对脏树注入 .d<差异哈希>）时只截短前段
// git 短哈希、保留后缀——此前无差别 [:7] 会把脏树后缀截掉，脏树重建与
// 旧二进制报同一个串，升级门控误判「已最新」。
func BuildVersion() string {
	c := Commit
	if i := strings.IndexByte(c, '.'); i >= 0 {
		if len(c[:i]) > 7 {
			c = c[:7] + c[i:]
		}
	} else if len(c) > 7 {
		c = c[:7]
	}
	return Version + "-" + c
}

// Unversioned 报告本次构建未注入构建信息（Commit 仍是缺省 "none"：
// 裸 go build / go run，未经 build.sh）。这类构建的版本串不反映二进制
// 内容——同串不能证明同版本，升级门控与幂等短路都应按「可升级」处理，
// 由升级流程重推二进制兜底。
func Unversioned() bool { return Commit == "none" }

// Tier 的刻意的反模式警告：不要把 Tier 并进 BuildVersion()。升级门控以
// 「server 与 agent 同源构建，BuildVersion 值一致即同版本」判新旧；
// tier 进版本串会让 full 与 agent 档的同源构建串不相等，agent 被永久
// 误判「待升级」形成升级循环。档位差异只影响功能面，不影响 agent 侧
// 行为，同 commit 即同 agent 逻辑，版本串必须保持一致。
