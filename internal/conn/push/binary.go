package push

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"time"

	"wdp/internal/agentbin"
	"wdp/internal/conn"
)

// resolveBinary 选择自举二进制（探测目标平台后按优先级取用）：
// 主机 binary_path > wdp.cfg [agent].push_binary.<平台> > 控制端自身
// （目标机与控制端同平台时）> 控制端可执行文件同级的
// wdp-<os>-<arch>（./build.sh 全集构建的 bin 目录）。
// 跨平台且四者皆无返回错误——调用方回退纯 SSH，不做注定失败的二进制上传。
func (c *Conn) resolveBinary(ctx context.Context) (string, error) {
	remote, err := c.detectPlatform(ctx)
	if err != nil {
		return "", err
	}
	var cfgMap map[string]string
	if c.dc != nil {
		cfgMap = c.dc.PushBinary
	}
	bin, useSelf, ok := selectPushBinary(c.host.BinaryPath, remote, runtime.GOOS+"_"+runtime.GOARCH, cfgMap, agentbin.SiblingPath)
	if !ok {
		return "", fmt.Errorf("no local binary for remote platform %s (control %s): run wdp from a build.sh bin directory (expected sibling %s next to the executable), or set [agent].push_binary.%s in wdp.cfg or host binary_path",
			remote, runtime.GOOS+"_"+runtime.GOARCH, agentbin.FileName(remote), remote)
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

// selectPushBinary 纯选择逻辑（hostBinary > 平台表 > 同平台自身 > 同级
// bin 目录）。remote 为空（平台未知）时退回自身尝试（旧行为：探测不可用
// 不阻断自举，由远端启动探测兜底报错）。ok=false 表示跨平台且无可用二进制。
func selectPushBinary(hostBinary, remote, self string, cfg map[string]string, sibling func(string) (string, bool)) (bin string, useSelf, ok bool) {
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
	if sibling != nil {
		if b, hit := sibling(remote); hit {
			return b, false, true
		}
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
	return agentbin.FromUname(out.Stdout), nil
}
