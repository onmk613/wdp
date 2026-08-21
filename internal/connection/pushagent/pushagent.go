// Package pushagent 实现临时 agent 自举连接（conn: push）：
//
//  1. 经 SSH 上传自身二进制与会话级临时 mTLS 材料（控制端每进程生成一次
//     一次性 CA 与证书对，全部主机共享，不按主机签发）
//  2. 远端以 --ca/--cert/--key 启动真端口 agent（默认 7602，可由 wdp.cfg
//     [agent].port 修改，host 键 agent_port 可指定；未显式指定且被占用时自动
//     换随机端口重试）——控制端 mTLS 直连 HTTPS，不经 SSH 隧道，无 token
//     （客户端证书即凭证；只验证书链不验主机名，SAN 无需覆盖主机地址）
//  3. 自举成功后释放 SSH 连接，执行期全部走 agent（与常驻 agent 同等吞吐），
//     过网流量全部 TLS 加密
//  4. Close 时 POST /shutdown，agent 以 --cleanup-on-shutdown 自删二进制与
//     证书文件（keep_agent: true 可保留调试；证书为会话级，保留的 agent 在
//     控制端退出后即不可再连）
//
// 证书轮换（wdp.cfg [agent].cert_rotate_min，缺省不轮换）：证书对为全部主机
// 共享，配置轮换周期可压缩其暴露窗口（被攻破主机配合流量劫持冒充他机的
// 有效期缩短为一个周期）。仓库到期由首个使用者换新代信任链，其余存活主机
// 在各自下一次任务前惰性迁移（重传证书 + 重启 agent + 重建直连）；
// 已完成自销毁的主机不再有任务，自然永不轮换。
//
// 自举失败自动回退纯 SSH 执行并告警。
package pushagent

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"wdp/internal/ca"
	"wdp/internal/config"
	"wdp/internal/connection"
	"wdp/internal/connection/agentconn"
	"wdp/internal/connection/sshconn"
	"wdp/internal/model"
	"wdp/internal/shellquote"
)

// certStore 进程级证书仓库：push 全部主机共享当前代材料（gen 单调递增，
// 每代独立信任链）。
var certStore struct {
	sync.Mutex
	certs *ca.EphemeralCerts
	gen   uint64
	at    time.Time
}

// certMaterial 返回当前代材料与代数（首次调用生成；配置轮换且已到期先换新）。
func certMaterial() (*ca.EphemeralCerts, uint64, error) {
	certStore.Lock()
	defer certStore.Unlock()
	if err := refreshDueLocked(); err != nil {
		return nil, 0, err
	}
	return certStore.certs, certStore.gen, nil
}

// refreshDueLocked 材料缺失或超过轮换周期时重新生成（调用方持有锁）。
func refreshDueLocked() error {
	interval := config.Current().AgentCertRotateMin()
	if certStore.certs != nil &&
		(interval <= 0 || time.Since(certStore.at) <= time.Duration(interval)*time.Minute) {
		return nil
	}
	certs, err := ca.IssueEphemeral()
	if err != nil {
		return err
	}
	certStore.certs, certStore.gen, certStore.at = certs, certStore.gen+1, time.Now()
	return nil
}

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

	mu   sync.Mutex // 串行化任务与证书迁移（防同连接并发操作撞上轮换换新）
	port int        // 当前 agent 端口（轮换重启沿用）
	gen  uint64     // 本连接使用的证书代数（落后当前代则任务前迁移）
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
		c.cleanupArtifacts(ctx)
		fmt.Fprintf(os.Stderr, "[push] 主机 %s 临时 agent 自举失败，回退纯 SSH: %v\n", c.host.Name, err)
		return nil // 降级：保留 SSH 连接继续执行
	}
	c.started = true
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

	certs, gen, err := certMaterial()
	if err != nil {
		return fmt.Errorf("生成临时 mTLS 材料失败: %w", err)
	}

	suffix, _ := randToken(8)
	c.remoteBin = fmt.Sprintf("/tmp/.wdp-agent-%s", suffix)
	if err := c.ssh.UploadFile(ctx, c.remoteBin, f, 0o755); err != nil {
		return fmt.Errorf("上传二进制失败: %w", err)
	}
	if err := c.uploadCerts(ctx, certs); err != nil {
		return err
	}

	// 端口序列：显式指定则只用它；默认端口（wdp.cfg [agent].port）+ 随机重试
	ports := []int{c.host.AgentPort}
	if c.host.AgentPort == 0 {
		ports = []int{config.Current().AgentPort(), randomPort(), randomPort()}
	}
	var lastErr error
	for _, port := range ports {
		if err := c.startAgent(ctx, port); err != nil {
			lastErr = err
			c.killAgent(ctx)
			continue
		}
		// 直连健康检查（真端口开放验证）
		ac := agentconn.New(c.agentHost(port, certs))
		checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := ac.Connect(checkCtx)
		cancel()
		if err == nil {
			c.agent = ac
			c.port, c.gen = port, gen
			return nil
		}
		lastErr = fmt.Errorf("端口 %d 健康检查失败: %w", port, err)
		c.killAgent(ctx)
	}
	return lastErr
}

// uploadCerts 上传服务端证书对与 CA 证书（与二进制同目录、同随机后缀）。
func (c *Conn) uploadCerts(ctx context.Context, certs *ca.EphemeralCerts) error {
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
			return fmt.Errorf("上传 mTLS 材料失败: %w", err)
		}
	}
	return nil
}

// startAgent 远端启动 agent（后台 + pid 文件）。mTLS 三件套来自自举上传，
// 无 token——客户端证书即认证凭证。监听 :port 双栈通配（IPv4/IPv6 皆可达；
// Go 在无 IPv6 的内核上自动回退 IPv4 通配，纯 IPv4 主机不受影响）。
func (c *Conn) startAgent(ctx context.Context, port int) error {
	script := fmt.Sprintf(
		`nohup %s agent --listen :%d --ca %s --cert %s --key %s --cleanup-on-shutdown >/dev/null 2>&1 & echo $! > %s.pid`,
		shellquote.Quote(c.remoteBin), port,
		shellquote.Quote(c.remoteBin+".ca"), shellquote.Quote(c.remoteBin+".crt"), shellquote.Quote(c.remoteBin+".key"),
		shellquote.Quote(c.remoteBin))
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

// killAgent 结束远端 agent 进程（端口重试/异常清理用）。只杀进程删 pid，
// 二进制与证书在端口重试间复用，最终清理由 cleanupArtifacts 或 agent
// --cleanup-on-shutdown 完成。
func (c *Conn) killAgent(ctx context.Context) {
	if c.remoteBin == "" {
		return
	}
	script := fmt.Sprintf("[ -f %s.pid ] && kill $(cat %s.pid) 2>/dev/null; rm -f %s.pid",
		shellquote.Quote(c.remoteBin), shellquote.Quote(c.remoteBin), shellquote.Quote(c.remoteBin))
	_, _ = c.ssh.Exec(ctx, connection.ExecRequest{Script: script, TimeoutMs: 5_000})
}

// cleanupArtifacts 删除已上传的自举产物（自举失败回退纯 SSH 前调用；
// 成功路径由 agent --cleanup-on-shutdown 自删，两者不重叠）。
func (c *Conn) cleanupArtifacts(ctx context.Context) {
	if c.remoteBin == "" {
		return
	}
	script := fmt.Sprintf("rm -f %s %s.pid %s.ca %s.crt %s.key",
		shellquote.Quote(c.remoteBin), shellquote.Quote(c.remoteBin), shellquote.Quote(c.remoteBin),
		shellquote.Quote(c.remoteBin), shellquote.Quote(c.remoteBin))
	ctx2, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = c.ssh.Exec(ctx2, connection.ExecRequest{Script: script, TimeoutMs: 5_000})
}

// agentHost 构造直连用的主机描述：https + 会话级临时 CA（内联 PEM）+
// 客户端证书 + 只验证书链不验主机名（证书 SAN 为会话级固定值，与主机地址无关）。
func (c *Conn) agentHost(port int, certs *ca.EphemeralCerts) *model.Host {
	clone := c.host.Clone()
	clone.AgentURL = "https://" + net.JoinHostPort(c.host.Address, strconv.Itoa(port))
	clone.CAFile, clone.CertFile, clone.KeyFile = "", "", ""
	clone.CAData = certs.CACertPEM
	clone.CertData = certs.ClientCertPEM
	clone.KeyData = certs.ClientKeyPEM
	clone.TLSSkipHostVerify = true
	clone.Conn = "agent"
	return clone
}

// rotateIfDue 证书轮换惰性检查（每次任务前调用，仅存活连接生效）：
// 仓库到期则换新代信任链；本连接落后于当前代则迁移。失败时本次任务报错，
// 但连接保留旧代记录，下次任务自动重试迁移（自愈）。
func (c *Conn) rotateIfDue(ctx context.Context) error {
	if config.Current().AgentCertRotateMin() <= 0 {
		return nil
	}
	certStore.Lock()
	if err := refreshDueLocked(); err != nil {
		certStore.Unlock()
		return err
	}
	certs, gen := certStore.certs, certStore.gen
	certStore.Unlock()
	if c.gen == gen {
		return nil
	}
	return c.migrateCerts(ctx, certs, gen)
}

// migrateCerts 将本主机迁移到新一代材料：SSH 重连 → 覆盖上传证书三件套 →
// 重启 agent（原端口优先，沿用自举的随机端口重试）→ 新配置健康检查通过后
// 原子换入，随后照常释放 SSH。
func (c *Conn) migrateCerts(ctx context.Context, certs *ca.EphemeralCerts, gen uint64) error {
	if err := c.ssh.Connect(ctx); err != nil {
		return fmt.Errorf("证书轮换 SSH 重连失败: %w", err)
	}
	if err := c.uploadCerts(ctx, certs); err != nil {
		_ = c.ssh.Close()
		return err
	}
	var lastErr error
	for _, port := range []int{c.port, randomPort(), randomPort()} {
		c.killAgent(ctx)
		if err := c.startAgent(ctx, port); err != nil {
			lastErr = err
			continue
		}
		ac := agentconn.New(c.agentHost(port, certs))
		checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := ac.Connect(checkCtx)
		cancel()
		if err == nil {
			_ = c.agent.Close()
			c.agent, c.port, c.gen = ac, port, gen
			_ = c.ssh.Close() // 迁移完毕释放 SSH（与自举后行为一致）
			return nil
		}
		lastErr = fmt.Errorf("端口 %d 健康检查失败: %w", port, err)
	}
	_ = c.ssh.Close()
	return fmt.Errorf("证书轮换失败: %w", lastErr)
}

// Close 关闭：shutdown 临时 agent（自删二进制与证书），keep_agent 时保留。
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

// Exec 执行（agent 就绪走 agent 并先做轮换检查，否则回退 SSH）。
func (c *Conn) Exec(ctx context.Context, req connection.ExecRequest) (connection.ExecResult, error) {
	if c.agent != nil {
		c.mu.Lock()
		defer c.mu.Unlock()
		if err := c.rotateIfDue(ctx); err != nil {
			return connection.ExecResult{}, err
		}
		return c.agent.Exec(ctx, req)
	}
	return c.ssh.Exec(ctx, req)
}

// UploadFile 上传（同 Exec 的轮换与回退策略；迁移在消费 reader 前完成）。
func (c *Conn) UploadFile(ctx context.Context, dst string, r io.Reader, mode fs.FileMode) error {
	if c.agent != nil {
		c.mu.Lock()
		defer c.mu.Unlock()
		if err := c.rotateIfDue(ctx); err != nil {
			return err
		}
		return c.agent.UploadFile(ctx, dst, r, mode)
	}
	return c.ssh.UploadFile(ctx, dst, r, mode)
}

// DownloadFile 下载（同 Exec 的轮换与回退策略）。
func (c *Conn) DownloadFile(ctx context.Context, src string, w io.Writer) error {
	if c.agent != nil {
		c.mu.Lock()
		defer c.mu.Unlock()
		if err := c.rotateIfDue(ctx); err != nil {
			return err
		}
		return c.agent.DownloadFile(ctx, src, w)
	}
	return c.ssh.DownloadFile(ctx, src, w)
}

// NativeExtract 转发给临时 agent（自举二进制与控制端同版本，端点必然存在）；
// SSH 回退态（自举失败）返回 ErrNativeUnsupported，模块回退 shell 路径。
func (c *Conn) NativeExtract(ctx context.Context, src, dest string) error {
	if c.agent == nil {
		return connection.ErrNativeUnsupported
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.rotateIfDue(ctx); err != nil {
		return err
	}
	return c.agent.NativeExtract(ctx, src, dest)
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
