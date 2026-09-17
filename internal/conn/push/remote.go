package push

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"net"
	"strconv"
	"strings"
	"time"

	"wdp/internal/conn"
	"wdp/internal/model"
	"wdp/internal/pushcerts"
	"wdp/internal/shellquote"
)

// uploadCerts 上传服务端证书对与 CA 证书（与二进制同目录、同随机后缀）。
func (c *Conn) uploadCerts(ctx context.Context, certs *pushcerts.Session) error {
	files := []struct {
		dst  string
		data []byte
		mode fs.FileMode
	}{
		{c.remoteBin + ".crt", certs.ServerCertPEM, 0o644},
		{c.remoteBin + ".key", certs.ServerKeyPEM, 0o600},
		{c.remoteBin + ".ca", certs.CACertPEM, 0o644},
	}
	for _, f := range files {
		if err := c.ssh.UploadFile(ctx, f.dst, bytes.NewReader(f.data), f.mode); err != nil {
			return fmt.Errorf("failed to upload mTLS material: %w", err)
		}
	}
	return nil
}

// startAgent 远端启动 agent（后台 + pid 文件 + 日志落盘）。mTLS 三件套
// 来自自举上传，无 token——客户端证书即认证凭证。监听 :port 双栈通配
// （IPv4/IPv6 皆可达；Go 在无 IPv6 的内核上自动回退 IPv4 通配，纯 IPv4
// 主机不受影响）。启动后等 1s 验活：进程即死（典型：控制端与目标机平台
// 不匹配，exec format error）时直接带出日志报错，不留到健康检查阶段。
func (c *Conn) startAgent(ctx context.Context, port int) error {
	script := agentStartScript(c.remoteBin, port, c.dc.AgentIdleTimeoutMinOrDefault())
	out, err := c.ssh.Exec(ctx, conn.ExecRequest{Script: script, TimeoutMs: 10_000})
	if err != nil {
		return err
	}
	if out.Code != 0 {
		return fmt.Errorf("failed to start: %s", firstLines(out.Stderr, 500))
	}
	return nil
}

// agentStartScript 生成远端启动命令。idleMin > 0 时附加 --idle-timeout：
// 控制端崩溃/被杀/断网时 Close 不会被调用，远端 agent 依赖空闲超时兜底
// 自清理（默认 60 分钟，wdp.cfg [agent].idle_timeout_min 调整，<0 禁用）。
// 输出重定向到 <bin>.log（诊断：证书问题等启动失败原因可查）；启动前
// --help 探测二进制可执行性——控制端与目标机平台不匹配（如 macOS 二进制
// 推给 Linux）时 exec format error，这里给出明确提示而不是留到端口层面。
func agentStartScript(bin string, port, idleMin int) string {
	q := shellquote.Quote(bin)
	script := fmt.Sprintf(`%s agent --help >/dev/null 2>&1 || { echo 'binary cannot run on this host (control/target platform mismatch?): rc='$? >&2; exit 1; }
nohup %s agent --listen :%d --ca %s --cert %s --key %s --cleanup-on-shutdown`,
		q, q, port,
		shellquote.Quote(bin+".ca"), shellquote.Quote(bin+".crt"), shellquote.Quote(bin+".key"))
	if idleMin > 0 {
		script += fmt.Sprintf(" --idle-timeout %dm", idleMin)
	}
	return script + fmt.Sprintf(` >%s.log 2>&1 &
echo $! > %s.pid
sleep 1
kill -0 $(cat %s.pid) 2>/dev/null || { echo 'agent exited early:' >&2; cat %s.log >&2; exit 1; }`,
		shellquote.Quote(bin), shellquote.Quote(bin), shellquote.Quote(bin), shellquote.Quote(bin))
}

// killAgent 结束远端 agent 进程（端口重试/异常清理用）。只杀进程删 pid，
// 二进制与证书在端口重试间复用，最终清理由 cleanupArtifacts 或 agent
// --cleanup-on-shutdown 完成。
func (c *Conn) killAgent(ctx context.Context) {
	if c.remoteBin == "" {
		return
	}
	script := fmt.Sprintf("[ -f %s.pid ] && kill $(cat %s.pid) 2>/dev/null; rm -f %s.pid",
		shellquote.Quote(c.remoteBin), shellquote.Quote(c.remoteBin), shellquote.Quote(c.remoteBin))
	_, _ = c.ssh.Exec(ctx, conn.ExecRequest{Script: script, TimeoutMs: 5_000})
}

// cleanupArtifacts 删除已上传的自举产物（自举失败回退纯 SSH 前调用；
// 成功路径由 agent --cleanup-on-shutdown 自删，两者不重叠）。
func (c *Conn) cleanupArtifacts() {
	if c.remoteBin == "" {
		return
	}
	script := fmt.Sprintf("rm -f %s %s.pid %s.log %s.ca %s.crt %s.key",
		shellquote.Quote(c.remoteBin), shellquote.Quote(c.remoteBin), shellquote.Quote(c.remoteBin),
		shellquote.Quote(c.remoteBin), shellquote.Quote(c.remoteBin), shellquote.Quote(c.remoteBin))
	ctx2, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = c.ssh.Exec(ctx2, conn.ExecRequest{Script: script, TimeoutMs: 5_000})
}

// agentHost 构造直连用的主机描述：https + 会话级临时 CA（内联 PEM）+
// 客户端证书 + 只验证书链不验主机名（证书 SAN 为会话级固定值，与主机地址
// 无关），并以会话服务端证书精确 pin 住对端——共享证书场景下这是唯一能
// 区分"是不是本会话该主机"的凭据。
func (c *Conn) agentHost(port int, certs *pushcerts.Session) *model.Host {
	clone := c.host.Clone()
	clone.AgentURL = "https://" + net.JoinHostPort(c.host.Address, strconv.Itoa(port))
	clone.CAFile, clone.CertFile, clone.KeyFile = "", "", ""
	clone.CAData = certs.CACertPEM
	clone.CertData = certs.ClientCertPEM
	clone.KeyData = certs.ClientKeyPEM
	clone.PeerCertData = certs.ServerCertPEM
	clone.TLSSkipHostVerify = true
	clone.Conn = "agent"
	return clone
}

// withAgentLog 健康检查失败时附带远端 agent 日志尾部（进程活着但端口
// 未就绪/证书问题等场景的根因诊断）。
func withAgentLog(ctx context.Context, c *Conn, err error) error {
	if log := c.agentLog(ctx); log != "" {
		return fmt.Errorf("%v; agent log: %s", err, log)
	}
	return err
}

// agentLog 读取远端 agent 启动日志尾部（best-effort，失败返回空）。
func (c *Conn) agentLog(ctx context.Context) string {
	if c.remoteBin == "" {
		return ""
	}
	ctx2, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	out, err := c.ssh.Exec(ctx2, conn.ExecRequest{
		Script:    fmt.Sprintf("tail -c 500 %s.log 2>/dev/null", shellquote.Quote(c.remoteBin)),
		TimeoutMs: 3_000,
	})
	if err != nil || out.Code != 0 {
		return ""
	}
	return strings.TrimSpace(out.Stdout)
}
