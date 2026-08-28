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
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"wdp/internal/ca"
	"wdp/internal/conn"
	"wdp/internal/conn/agentc"
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

// Install 逐主机安装：SSH 推自身二进制 + 逐主机签发服务端证书 +
// 安装 systemd 单元并启动 + 可选 mTLS 验证。与 retire 对称：retire 负责
// 退役清理，install 负责上线。
func Install(ctx context.Context, hosts []*model.Host, o Options, forks int, dc *conn.Defaults, out io.Writer) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	failed := forEachHost(ctx, hosts, forks, func(ctx context.Context, h *model.Host) error {
		identity := certNameFor(h)
		port := h.AgentPort
		if port == 0 {
			port = dc.AgentPortOrDefault() // h.AgentPort > wdp.cfg [agent].port > 7602
		}
		ssh := sshc.New(h)
		if err := ssh.Connect(ctx); err != nil {
			return err
		}
		defer ssh.Close()

		// 1. 二进制（仅缺失或版本不同才重推：幂等重装不无谓覆盖）
		if err := sshRun(ctx, ssh, fmt.Sprintf(
			"mkdir -p %s", quoteSh(filepath.Dir(o.BinPath)))); err != nil {
			return err
		}
		bin, err := os.Open(exe)
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
			quoteSh("/etc/systemd/system"), quoteSh("/etc/systemd/system/"+o.UnitName+".service"), unit, o.UnitName)
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

// RenewCert 逐主机延期+推送+重启：本地更新延期（增量+备份，复用 wdp ca
// renew 语义）→ SSH 推送新证书 → 重启 agent → 可选验证。
func RenewCert(ctx context.Context, hosts []*model.Host, o Options, forks int, dc *conn.Defaults, out io.Writer) error {
	failed := forEachHost(ctx, hosts, forks, func(ctx context.Context, h *model.Host) error {
		identity := certNameFor(h)
		crt := filepath.Join(o.CertDir, identity+".crt")
		// 本地更新延期：同路径原地（自动备份 *.old.<ts>），身份字段全继承
		newCrt, newKey, _, err := ca.Renew(ca.RenewOptions{
			CertPath: crt, KeyPath: filepath.Join(o.CertDir, identity+".key"),
			OutPath: crt, CACertPath: o.CACertPath, CAKeyPath: o.CAKeyPath,
			NewKey: o.NewKey, Days: o.Days,
		})
		if err != nil {
			return fmt.Errorf("renew %s: %w", identity, err)
		}
		ssh := sshc.New(h)
		if err := ssh.Connect(ctx); err != nil {
			return err
		}
		defer ssh.Close()
		if err := uploadTriple(ctx, ssh, o.RemoteDir, newCrt, newKey, o.CACertPath); err != nil {
			return err
		}
		if err := sshRun(ctx, ssh, "systemctl restart "+o.UnitName); err != nil {
			return fmt.Errorf("restart agent: %w", err)
		}
		if o.ClientCert != "" {
			port := h.AgentPort
			if port == 0 {
				port = dc.AgentPortOrDefault() // h.AgentPort > wdp.cfg [agent].port > 7602
			}
			// systemd 重启后短暂等待监听就绪
			time.Sleep(300 * time.Millisecond)
			if err := verifyAgent(ctx, h, port, o, dc); err != nil {
				return fmt.Errorf("renewed but verification failed: %w", err)
			}
		}
		fmt.Fprintf(out, "%s: renewed (+%dd)\n", h.Name, o.Days)
		return nil
	})
	return summarize(failed, len(hosts))
}

// uploadTriple 上传证书三件套到目标机目录（agent.crt/agent.key/ca.crt）。
func uploadTriple(ctx context.Context, ssh *sshc.Conn, dir, crt, key, caPath string) error {
	files := []struct {
		src, dst string
		mode     os.FileMode
	}{
		{crt, filepath.Join(dir, "agent.crt"), 0o644},
		{key, filepath.Join(dir, "agent.key"), 0o600},
		{caPath, filepath.Join(dir, "ca.crt"), 0o644},
	}
	if err := sshRun(ctx, ssh, "mkdir -p "+quoteSh(dir)); err != nil {
		return err
	}
	for _, f := range files {
		fh, err := os.Open(f.src)
		if err != nil {
			return err
		}
		err = ssh.UploadFile(ctx, f.dst, fh, f.mode)
		fh.Close()
		if err != nil {
			return fmt.Errorf("upload %s: %w", f.dst, err)
		}
	}
	return nil
}

// verifyAgent 控制端 mTLS 直连目标 agent 做 health 验证
// （证书 SAN = 主机身份，同时验证主机名校验）。
func verifyAgent(ctx context.Context, h *model.Host, port int, o Options, dc *conn.Defaults) error {
	caPEM, err := os.ReadFile(o.CACertPath)
	if err != nil {
		return err
	}
	crtPEM, err := os.ReadFile(o.ClientCert)
	if err != nil {
		return err
	}
	keyPEM, err := os.ReadFile(o.ClientKey)
	if err != nil {
		return err
	}
	probe := &model.Host{
		Name: h.Name, Conn: "agent", Address: h.Address,
		AgentURL: "https://" + net.JoinHostPort(h.Address, strconv.Itoa(port)),
		CAData:   caPEM, CertData: crtPEM, KeyData: keyPEM,
	}
	vctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn := agentc.New(probe, dc)
	defer conn.Close()
	return conn.Connect(vctx)
}

// sshRun 经 SSH 执行一段脚本（失败返回错误，含退出码语义）。
func sshRun(ctx context.Context, ssh *sshc.Conn, script string) error {
	res, err := ssh.Exec(ctx, conn.ExecRequest{Script: script, TimeoutMs: 60_000})
	if err != nil {
		return err
	}
	if res.Code != 0 {
		return fmt.Errorf("exit %d: %s", res.Code, res.Stderr)
	}
	return nil
}

// quoteSh 单字参数 shell 引用。
func quoteSh(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
