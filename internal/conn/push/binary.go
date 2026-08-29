package push

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"

	"wdp/internal/conn"
)

// resolveBinary 选择自举二进制（探测目标平台后按优先级取用）：
// 主机 binary_path > wdp.cfg [agent].push_binary.<平台> > 控制端自身
// （目标机与控制端同平台时）。跨平台且未配置对应二进制返回错误——
// 调用方回退纯 SSH，不做注定失败的二进制上传。
func (c *Conn) resolveBinary(ctx context.Context) (string, error) {
	remote, err := c.detectPlatform(ctx)
	if err != nil {
		return "", err
	}
	var cfgMap map[string]string
	if c.dc != nil {
		cfgMap = c.dc.PushBinary
	}
	bin, useSelf, ok := selectPushBinary(c.host.BinaryPath, remote, runtime.GOOS+"_"+runtime.GOARCH, cfgMap)
	if !ok {
		return "", fmt.Errorf("remote platform %s has no matching push binary (control %s): set [agent].push_binary.%s in wdp.cfg or host binary_path",
			remote, runtime.GOOS+"_"+runtime.GOARCH, remote)
	}
	if !useSelf {
		return bin, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("failed to locate own binary: %w", err)
	}
	return exe, nil
}

// selectPushBinary 纯选择逻辑（hostBinary > 平台表 > 同平台自身）。
// remote 为空（平台未知）时退回自身尝试（旧行为：探测不可用不阻断自举，
// 由远端启动探测兜底报错）。ok=false 表示跨平台且无可用二进制。
func selectPushBinary(hostBinary, remote, self string, cfg map[string]string) (bin string, useSelf, ok bool) {
	if hostBinary != "" {
		return hostBinary, false, true
	}
	if remote == "" {
		return "", true, true
	}
	if b := cfg[remote]; b != "" {
		return b, false, true
	}
	if remote == self {
		return "", true, true
	}
	return "", false, false
}

// detectPlatform 经 uname -sm 探测目标平台（归一为 os_arch 键，如
// linux_amd64）；探测失败返回错误，无法判定（非 POSIX 环境）返回空串
// ——push 通道本就依赖 POSIX sh，此类主机由上层回退 SSH。
func (c *Conn) detectPlatform(ctx context.Context) (string, error) {
	ctx2, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := c.ssh.Exec(ctx2, conn.ExecRequest{Script: "uname -sm 2>/dev/null", TimeoutMs: 5_000})
	if err != nil {
		return "", err
	}
	if out.Code != 0 {
		return "", nil
	}
	return platformFromUname(out.Stdout), nil
}

// platformFromUname 归一 uname -sm 输出为 os_arch 平台键：
// "Linux x86_64"→linux_amd64、"Linux aarch64"→linux_arm64、
// "Darwin arm64"→darwin_arm64；无法识别返回空串。
func platformFromUname(s string) string {
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
