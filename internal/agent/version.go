package agent

// BuildVersion 返回二进制发布版本（buildinfo 注入；开发构建为 0.0.1-none）。
// /health 上报给控制端，远程升级据此比较新旧。

import "wdp/internal/buildinfo"

func BuildVersion() string { return buildinfo.BuildVersion() }
