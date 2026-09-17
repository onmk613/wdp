// Package agentops 实现控制端对常驻 agent 的运维内核：安装上线
// （install）、证书延期（renew-cert）、巡检（status）、退役自清理
// （retire）与日志拉取（logs）。命令行装配（主机来源、flag、确认交互）
// 在 internal/cli，本包只承载可测的操作内核。
//
// install / renew-cert 与 retire 呼应，构成生命周期闭环。全程经 SSH 操作
// 目标机；换证不做热换（/cert 端点已随 renew-cert 旧实现移除，待 CSR 方案
// 重设计），走"本地延期 + 推送 + 重启"的显式路径，停机窗口 = systemd
// 重启的秒级。
package agentops

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"wdp/internal/agentbin"
	"wdp/internal/ca"
	"wdp/internal/conn"
	"wdp/internal/conn/sshc"
	"wdp/internal/model"
)

// Options 是 install / renew-cert 共用的参数。
type Options struct {
	CACertPath  string // 根 CA 证书（签发/延期用）
	CAKeyPath   string // 根 CA 私钥
	CertDir     string // 控制端本地证书目录（<主机身份>.crt/.key 所在/产出）
	BinPath     string // 目标机二进制路径（install）
	UnitName    string // systemd 单元名
	RemoteDir   string // 目标机证书目录
	ClientCert  string // 控制端客户端证书（装完/换完验证用；空跳过验证）
	ClientKey   string
	NewKey      bool // renew-cert：换新私钥
	Days        int  // renew-cert：增量天数
	IdleTimeout time.Duration
}

// agentUnitFile 生成 systemd 单元内容（install 用）。
func agentUnitFile(binPath, dir string, port int, unit, logFile string) string {
	return fmt.Sprintf(`[Unit]
Description=wdp agent (%s)
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=%s agent --listen 0.0.0.0:%d --ca %s --cert %s --key %s --log-file %s
Restart=always
RestartSec=3

[Install]
WantedBy=multi-user.target
`, unit, binPath, port, filepath.Join(dir, "ca.crt"), filepath.Join(dir, "agent.crt"),
		filepath.Join(dir, "agent.key"), logFile)
}

// Install 逐主机安装：SSH 推与目标机平台匹配的二进制 + 逐主机签发服务端
// 证书 + 安装 systemd 单元并启动 + 可选 mTLS 验证。与 retire 对称：retire
// 负责退役清理，install 负责上线。
func Install(ctx context.Context, hosts []*model.Host, o Options, forks int, dc *conn.Defaults, out io.Writer) error {
	if err := validateOptions(o); err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	failed := forEachHost(ctx, hosts, forks, func(ctx context.Context, h *model.Host) error {
		identity, err := certIdentity(h)
		if err != nil {
			return err
		}
		port := h.AgentPort
		if port == 0 {
			port = dc.AgentPortOrDefault() // h.AgentPort > wdp.cfg [agent].port > 7602
		}
		ssh := sshc.New(h)
		if err := ssh.Connect(ctx); err != nil {
			return err
		}
		defer ssh.Close()

		// 1. 二进制：按目标机平台选本地文件（install 是显式操作，选不出
		// 该主机直接报错，不降级推送注定跑不起来的二进制）
		local, err := resolveHostBinary(ctx, ssh, exe, dc)
		if err != nil {
			return fmt.Errorf("select binary: %w", err)
		}
		if err := sshRun(ctx, ssh, fmt.Sprintf(
			"mkdir -p %s", quoteSh(filepath.Dir(o.BinPath)))); err != nil {
			return err
		}
		bin, err := os.Open(local)
		if err != nil {
			return err
		}
		if err := ssh.UploadFile(ctx, o.BinPath, bin, 0o755); err != nil {
			return fmt.Errorf("upload binary: %w", err)
		}
		bin.Close()

		// 2. 逐主机签发服务端证书（SAN = 主机身份）并上传
		crt, key, _, err := ca.Issue(ca.IssueOptions{
			Dir: o.CertDir, CACertPath: o.CACertPath, CAKeyPath: o.CAKeyPath,
			SANs: []string{identity},
		}, identity)
		if err != nil {
			return fmt.Errorf("issue cert: %w", err)
		}
		if err := uploadTriple(ctx, ssh, o.RemoteDir, crt, key, o.CACertPath); err != nil {
			return err
		}

		// 3. systemd 单元 + 启动
		unit := agentUnitFile(o.BinPath, o.RemoteDir, port, o.UnitName, "/var/log/wdp-agent.log")
		script := fmt.Sprintf("mkdir -p %s && cat > %s <<'WDP_UNIT_EOF'\n%s\nWDP_UNIT_EOF\nsystemctl daemon-reload && systemctl enable --now %s\n",
			quoteSh("/etc/systemd/system"), quoteSh("/etc/systemd/system/"+o.UnitName+".service"), unit, quoteSh(o.UnitName))
		if err := sshRun(ctx, ssh, script); err != nil {
			return fmt.Errorf("install unit: %w", err)
		}

		// 4. 可选验证：控制端 mTLS 直连 health
		if o.ClientCert == "" {
			fmt.Fprintf(out, "%s: installed (verification skipped)\n", h.Name)
			return nil
		}
		if err := verifyAgent(ctx, h, port, o, dc); err != nil {
			return fmt.Errorf("installed but verification failed: %w", err)
		}
		fmt.Fprintf(out, "%s: installed and verified\n", h.Name)
		return nil
	})
	return summarize(failed, len(hosts))
}

// resolveHostBinary 探测目标机平台并选择本地 agent 二进制（优先级见
// pickHostBinary）。探测失败或平台无法识别返回错误。
func resolveHostBinary(ctx context.Context, ssh *sshc.Conn, selfExe string, dc *conn.Defaults) (string, error) {
	self := runtime.GOOS + "_" + runtime.GOARCH
	out, err := ssh.Exec(ctx, conn.ExecRequest{Script: "uname -sm 2>/dev/null", TimeoutMs: 5_000})
	if err != nil {
		return "", fmt.Errorf("detect platform: %w", err)
	}
	platform := ""
	if out.Code == 0 {
		platform = agentbin.FromUname(out.Stdout)
	}
	if platform == "" {
		return "", fmt.Errorf("cannot detect target platform (uname -sm: %q); binary selection requires a Linux/Darwin target", strings.TrimSpace(out.Stdout))
	}
	var cfg map[string]string
	if dc != nil {
		cfg = dc.PushBinary
	}
	if b, ok := pickHostBinary(platform, self, selfExe, cfg, agentbin.SiblingPath); ok {
		return b, nil
	}
	return "", fmt.Errorf("no local binary for target platform %s: run wdp from a build.sh bin directory (expected sibling %s next to the executable), or set [agent].push_binary.%s in wdp.cfg",
		platform, agentbin.FileName(platform), platform)
}

// pickHostBinary 纯选择逻辑：[agent].push_binary 平台表 > 同平台用控制端
// 自身 > 控制端可执行文件同级的 wdp-<os>-<arch>（build.sh 全集构建产物）。
func pickHostBinary(platform, selfPlatform, selfExe string, cfg map[string]string, sibling func(string) (string, bool)) (string, bool) {
	if b := cfg[platform]; b != "" {
		return b, true
	}
	if platform == selfPlatform {
		return selfExe, true
	}
	if sibling != nil {
		if b, ok := sibling(platform); ok {
			return b, true
		}
	}
	return "", false
}
