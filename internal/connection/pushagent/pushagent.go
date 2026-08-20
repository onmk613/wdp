// Package pushagent 实现临时 agent 自举连接（conn: push）：
//
//  1. 经 SSH 上传自身二进制与随机 token 文件（token 不出现在 ps 参数中）
//  2. 远端启动真端口 agent（默认 7602，可由 wdp.cfg [agent].port 修改，host 键
//     agent_port 可指定；未显式指定且被占用时自动换随机端口重试）——控制端直连
//     HTTP，不经 SSH 隧道
//  3. 自举成功后释放 SSH 连接，执行期全部走 agent（与常驻 agent 同等吞吐）
//  4. Close 时 POST /shutdown，agent 以 --cleanup-on-shutdown 自删二进制
//     （keep_agent: true 可保留调试）
//
// 自举失败自动回退纯 SSH 执行并告警。
package pushagent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
	"sync"
	"time"

	"wdp/internal/config"
	"wdp/internal/connection"
	"wdp/internal/connection/agentconn"
	"wdp/internal/connection/sshconn"
	"wdp/internal/model"
	"wdp/internal/shellquote"
)

// warnOnce 保证明文直连告警整个进程只打印一次。
var warnOnce sync.Once

func init() {
	connection.RegisterFactory("push", func(h *model.Host) (connection.Connection, error) {
		return New(h), nil
	})
}

// Conn 是 push 临时 agent 连接。
type Conn struct {
	host      *model.Host
	ssh       *sshconn.Conn
	agent     *agentconn.Conn
	remoteBin string
	started   bool
}

// New 创建 push 连接（未自举）。
func New(h *model.Host) *Conn {
	return &Conn{host: h, ssh: sshconn.New(h)}
}

// Connect 自举临时 agent；失败回退纯 SSH。
func (c *Conn) Connect(ctx context.Context) error {
	if err := c.ssh.Connect(ctx); err != nil {
		return err
	}
	if err := c.bootstrap(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "[push] 主机 %s 临时 agent 自举失败，回退纯 SSH: %v\n", c.host.Name, err)
		return nil // 降级：保留 SSH 连接继续执行
	}
	c.started = true
	// 安全告警：临时 agent 以明文 HTTP 对外监听，token 与 become 密码将明文过网。
	// 仅在可信内网使用；跨不可信网络的场景请用 conn: ssh 或常驻 agent + mTLS。
	warnOnce.Do(func() {
		fmt.Fprintln(os.Stderr, "[push] 警告: 临时 agent 经明文 HTTP 直连（token 明文传输），请仅在可信内网使用；跨不可信网络请改用 conn: ssh 或常驻 agent+mTLS")
	})
	// 自举完成，SSH 连接使命结束（清理由 agent 自删 + shutdown 完成）
	_ = c.ssh.Close()
	return nil
}

// bootstrap 执行自举流程。
func (c *Conn) bootstrap(ctx context.Context) error {
	bin := c.host.BinaryPath
	if bin == "" {
		exe, err := os.Executable()
		if err != nil {
			return fmt.Errorf("定位自身二进制失败: %w", err)
		}
		bin = exe
	}
	f, err := os.Open(bin)
	if err != nil {
		return fmt.Errorf("读取二进制失败: %w", err)
	}
	defer f.Close()

	suffix, _ := randToken(8)
	c.remoteBin = fmt.Sprintf("/tmp/.wdp-agent-%s", suffix)
	if err := c.ssh.UploadFile(ctx, c.remoteBin, f, 0o755); err != nil {
		return fmt.Errorf("上传二进制失败: %w", err)
	}

	// token 文件（agent 读取后自删，不进 ps 参数）
	token, _ := randToken(32)
	tokenFile := c.remoteBin + ".tok"
	if err := c.ssh.UploadFile(ctx, tokenFile, strings.NewReader(token+"\n"), 0o600); err != nil {
		return fmt.Errorf("上传 token 失败: %w", err)
	}

	// 端口序列：显式指定则只用它；默认端口（wdp.cfg [agent].port）+ 随机重试
	ports := []int{c.host.AgentPort}
	if c.host.AgentPort == 0 {
		ports = []int{config.Current().AgentPort(), randomPort(), randomPort()}
	}
	var lastErr error
	for _, port := range ports {
		if err := c.startAgent(ctx, port, tokenFile); err != nil {
			lastErr = err
			c.killAgent(ctx)
			continue
		}
		// 直连健康检查（真端口开放验证）
		ac := agentconn.New(c.agentHost(port, token))
		checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := ac.Connect(checkCtx)
		cancel()
		if err == nil {
			c.agent = ac
			return nil
		}
		lastErr = fmt.Errorf("端口 %d 健康检查失败: %w", port, err)
		c.killAgent(ctx)
	}
	return lastErr
}

// startAgent 远端启动 agent（后台 + pid 文件）。
func (c *Conn) startAgent(ctx context.Context, port int, tokenFile string) error {
	script := fmt.Sprintf(
		`nohup %s agent --listen 0.0.0.0:%d --token-file %s --cleanup-on-shutdown >/dev/null 2>&1 & echo $! > %s.pid`,
		shellquote.Quote(c.remoteBin), port, shellquote.Quote(tokenFile), shellquote.Quote(c.remoteBin))
	out, err := c.ssh.Exec(ctx, connection.ExecRequest{Script: script, TimeoutMs: 10_000})
	if err != nil {
		return err
	}
	if out.Code != 0 {
		return fmt.Errorf("启动失败: %s", firstLine(out.Stderr))
	}
	time.Sleep(300 * time.Millisecond) // 等监听就绪
	return nil
}

// killAgent 结束远端 agent 进程（端口重试/异常清理用）。
func (c *Conn) killAgent(ctx context.Context) {
	if c.remoteBin == "" {
		return
	}
	script := fmt.Sprintf("[ -f %s.pid ] && kill $(cat %s.pid) 2>/dev/null; rm -f %s.pid",
		shellquote.Quote(c.remoteBin), shellquote.Quote(c.remoteBin), shellquote.Quote(c.remoteBin))
	_, _ = c.ssh.Exec(ctx, connection.ExecRequest{Script: script, TimeoutMs: 5_000})
}

// agentHost 构造直连用的主机描述（复用 agentconn，token 认证）。
func (c *Conn) agentHost(port int, token string) *model.Host {
	clone := c.host.Clone()
	clone.AgentURL = fmt.Sprintf("http://%s:%d", c.host.Address, port)
	clone.Token = token
	clone.TokenEnv = ""
	clone.CAFile, clone.CertFile = "", ""
	clone.KeyFile = ""
	clone.Conn = "agent"
	return clone
}

// Close 关闭：shutdown 临时 agent（自删二进制），keep_agent 时保留。
func (c *Conn) Close() error {
	if c.started && c.agent != nil && !c.host.KeepAgent {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = c.agent.Shutdown(ctx)
	}
	c.agent = nil
	if c.ssh != nil {
		_ = c.ssh.Close()
	}
	return nil
}

// Hostname 返回主机名。
func (c *Conn) Hostname() string { return c.host.Name }

// Exec 执行（agent 就绪走 agent，否则回退 SSH）。
func (c *Conn) Exec(ctx context.Context, req connection.ExecRequest) (connection.ExecResult, error) {
	if c.agent != nil {
		return c.agent.Exec(ctx, req)
	}
	return c.ssh.Exec(ctx, req)
}

// UploadFile 上传（同 Exec 的回退策略）。
func (c *Conn) UploadFile(ctx context.Context, dst string, r io.Reader, mode fs.FileMode) error {
	if c.agent != nil {
		return c.agent.UploadFile(ctx, dst, r, mode)
	}
	return c.ssh.UploadFile(ctx, dst, r, mode)
}

// DownloadFile 下载（同 Exec 的回退策略）。
func (c *Conn) DownloadFile(ctx context.Context, src string, w io.Writer) error {
	if c.agent != nil {
		return c.agent.DownloadFile(ctx, src, w)
	}
	return c.ssh.DownloadFile(ctx, src, w)
}

func randToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func randomPort() int {
	// 避开常见服务端口段
	return 20000 + int(time.Now().UnixNano()%40000)
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}
