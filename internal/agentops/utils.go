package agentops

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"wdp/internal/conn"
	"wdp/internal/conn/agentc"
	"wdp/internal/conn/sshc"
	"wdp/internal/model"
)

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

// unitNameRe systemd 单元名的合法字符集。单元名会拼进 shell 命令
// （systemctl enable/restart）与单元文件内容，换行等元字符可注入任意
// systemd 指令（ExecStartPre= 等），在入口拒绝。
var unitNameRe = regexp.MustCompile(`^[A-Za-z0-9_.@\\-]+$`)

// validateOptions 校验 install/renew-cert 的安全敏感参数。
func validateOptions(o Options) error {
	if !unitNameRe.MatchString(o.UnitName) {
		return fmt.Errorf("invalid unit name %q (allowed: letters, digits, '.', '_', '-', '@')", o.UnitName)
	}
	for _, p := range []struct{ name, val string }{
		{"bin_path", o.BinPath}, {"remote_dir", o.RemoteDir},
		{"cert_dir", o.CertDir}, {"ca_cert", o.CACertPath}, {"ca_key", o.CAKeyPath},
	} {
		if strings.ContainsAny(p.val, "\n\r") {
			return fmt.Errorf("%s must not contain newlines (injected into systemd unit content)", p.name)
		}
	}
	return nil
}

// certIdentity 校验证书身份名（主机名/地址）并返回：身份被用作证书
// 文件名，路径分隔符会写出 CertDir 之外。
func certIdentity(h *model.Host) (string, error) {
	id := certNameFor(h)
	if strings.Contains(id, "/") || strings.Contains(id, "\\") || id == ".." || id == "." || strings.Contains(id, "..") {
		return "", fmt.Errorf("host %s identity %q is invalid as a certificate name (path separators rejected)", h.Name, id)
	}
	return id, nil
}
