//go:build pushembed

package pushbin

import "embed"

// assets 由 build.sh push 生成（<平台>.bin.gz + manifest.json，目录已
// gitignore）。目录缺失时带 tag 的编译直接失败——宁可构建报错，也不静默
// 产出没有载荷的"内嵌版"。
//
//go:embed assets
var embedded embed.FS

func init() { assetsFS = embedded }
