// Package agentbin 提供"目标平台 → 本地 agent 二进制"解析：build.sh 全集
// 交叉编译产出多架构 bin 目录（wdp-<os>-<arch>[.exe]），各平台二进制互为
// 同级文件，控制端按目标机 uname 归一出的平台键在自身所在目录查找同级
// 二进制。push 自举与 agentctl install 共用这套逻辑，跨平台分发不再依赖
// [agent].push_binary 配置表，也不再把载荷内嵌进控制端二进制自身。
package agentbin

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// platformKeyRe 限定平台键字符（键由 FromUname 归一产出，正常形如
// linux_amd64；拼文件名前校验，防异常输入构造路径穿越）。
var platformKeyRe = regexp.MustCompile(`^[a-z0-9]+(_[a-z0-9]+)*$`)

// FromUname 归一 uname -sm 输出为 os_arch 平台键：
// "Linux x86_64"→linux_amd64、"Linux aarch64"→linux_arm64、
// "Darwin arm64"→darwin_arm64；无法识别返回空串。
func FromUname(s string) string {
	fields := strings.Fields(strings.TrimSpace(s))
	if len(fields) != 2 {
		return ""
	}
	var goos string
	switch strings.ToLower(fields[0]) {
	case "linux":
		goos = "linux"
	case "darwin":
		goos = "darwin"
	default:
		return ""
	}
	var arch string
	switch fields[1] {
	case "x86_64", "amd64":
		arch = "amd64"
	case "aarch64", "arm64":
		arch = "arm64"
	case "i386", "i486", "i586", "i686":
		arch = "386"
	default:
		return ""
	}
	return goos + "_" + arch
}

// FileName 返回平台键对应的 bin 目录产物文件名：linux_amd64 →
// wdp-linux-amd64（下划线转连字符，windows 平台带 .exe 后缀）。
func FileName(platform string) string {
	name := "wdp-" + strings.ReplaceAll(platform, "_", "-")
	if strings.HasPrefix(platform, "windows_") {
		name += ".exe"
	}
	return name
}

// SiblingPath 返回与当前运行的可执行文件同级的 wdp-<os>-<arch>[.exe]
// 路径（未找到 ok=false）。
func SiblingPath(platform string) (string, bool) {
	exe, err := os.Executable()
	if err != nil {
		return "", false
	}
	return SiblingPathFor(exe, platform)
}

// SiblingPathFor 在 exePath 所在目录查找 platform 平台键对应的同级二进制。
// exePath 先经符号链接解析取真实路径再取目录（运行入口常是链入 bin/ 的
// 符号链接，如 /usr/local/bin/wdp → /opt/wdp/bin/wdp-darwin-arm64）。
// 文件不存在或不是普通文件返回 ok=false。
func SiblingPathFor(exePath, platform string) (string, bool) {
	if !platformKeyRe.MatchString(platform) {
		return "", false
	}
	real, err := filepath.EvalSymlinks(exePath)
	if err != nil {
		return "", false
	}
	path := filepath.Join(filepath.Dir(real), FileName(platform))
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return "", false
	}
	return path, true
}
