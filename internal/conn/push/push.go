// Package push 实现临时 agent 自举连接（conn: push）：
//
//  1. 经 SSH 上传自身二进制与会话级临时 mTLS 材料（会话 CA 的生成、
//     落盘与轮换在 internal/pushcerts，全部主机共享）
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
// 残留兜底与自愈：
//   - 自举注入 --idle-timeout（默认 60 分钟，wdp.cfg [agent].idle_timeout_min
//     调整，<0 禁用）：控制端崩溃/断网时 Close 不会被调用，远端 agent 超过
//     空闲周期且无已认证请求即自删退出，不必等 cleanupOnShutdown 信号
//     （/health 免认证探测不计入活动，同网段无法给残留 agent 续命）
//   - agent 传输层失败（不可达/断连）时自动重走自举修复后续任务；本次操作
//     不重放（脚本可能已执行，重试会重复非幂等任务）
//
// 证书轮换代数（gen）由 pushcerts 管理：仓库到期换新代信任链，本连接
// 落后于当前代时在任务前惰性迁移（重传证书 + 重启 agent + 重建直连）。
//
// 自举失败自动回退纯 SSH 执行并告警。
package push

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/url"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"wdp/internal/conn"
	"wdp/internal/conn/agentc"
	"wdp/internal/conn/sshc"
	"wdp/internal/inventory"
	"wdp/internal/model"
	"wdp/internal/pushcerts"
	"wdp/internal/shellquote"
)

func init() {
	// 本连接类型的主机条目专属键（inventory 白名单经 blank-import 注册）
	inventory.RegisterHostKeys("binary_path", "keep_agent")
	conn.RegisterFactory("push", func(h *model.Host, dc *conn.Defaults) (conn.Conn, error) {
		return New(h, dc), nil
	})
}

// Conn 是 push 临时 agent 连接。
type Conn struct {
	host      *model.Host
	dc        *conn.Defaults // 组合根注入的默认值（nil = 内置默认）
	ssh       *sshc.Conn
	agent     *agentc.Conn
	remoteBin string
	started   bool

	mu   sync.Mutex // 串行化任务与证书迁移（防同连接并发操作撞上轮换换新）
	port int        // 当前 agent 端口（轮换重启沿用）
	gen  uint64     // 本连接使用的证书代数（落后当前代则任务前迁移）
}

// New 创建 push 连接（未自举）。
func New(h *model.Host, dc *conn.Defaults) *Conn {
	return &Conn{host: h, dc: dc, ssh: sshc.New(h)}
}

// Connect 自举临时 agent；失败回退纯 SSH。幂等：自举成功后重复调用
// 无副作用（接口契约要求；二次自举会在远端泄漏首个 agent 进程）。
func (c *Conn) Connect(ctx context.Context) error {
	if c.started {
		return nil
	}
	if err := c.ssh.Connect(ctx); err != nil {
		return err
	}
	if err := c.bootstrap(ctx); err != nil {
		c.cleanupArtifacts()
		fmt.Fprintf(os.Stderr, "[push] host %s temporary agent bootstrap failed, falling back to plain SSH: %v\n", c.host.Name, err)
		return nil // 降级：保留 SSH 连接继续执行
	}
	c.started = true
	// 自举完成，SSH 连接使命结束（清理由 agent 自删 + shutdown 完成；
	// Close 阶段的兜底清理会按需重连）
	_ = c.ssh.Close()
	return nil
}

// bootstrap 执行自举流程。
func (c *Conn) bootstrap(ctx context.Context) error {
	bin, err := c.resolveBinary(ctx)
	if err != nil {
		return err
	}
	f, err := os.Open(bin)
	if err != nil {
		return fmt.Errorf("failed to read binary: %w", err)
	}
	defer f.Close()

	certs, gen, err := pushcerts.Material(c.dc)
	if err != nil {
		return fmt.Errorf("failed to generate ephemeral mTLS material: %w", err)
	}

	suffix, err := randToken(8)
	if err != nil {
		// 熵源失败时随机后缀为空串，全部材料落到可预测路径（/tmp/.wdp-agent-），
		// 违背不可预测命名目标，直接失败
		return fmt.Errorf("failed to generate random suffix: %w", err)
	}
	c.remoteBin = fmt.Sprintf("/tmp/.wdp-agent-%s", suffix)
	if err := c.ssh.UploadFile(ctx, c.remoteBin, f, 0o755); err != nil {
		return fmt.Errorf("failed to upload binary: %w", err)
	}
	if err := c.uploadCerts(ctx, certs); err != nil {
		return err
	}

	// 端口序列：显式指定则只用它；默认端口（组合根注入的 [agent].port）+ 随机重试
	ports := []int{c.host.AgentPort}
	if c.host.AgentPort == 0 {
		ports = []int{c.dc.AgentPortOrDefault(), randomPort(), randomPort()}
	}
	var lastErr error
	for _, port := range ports {
		if err := c.startAgent(ctx, port); err != nil {
			lastErr = err
			c.killAgent(ctx)
			continue
		}
		// 直连健康检查（真端口开放验证）
		ac := agentc.New(c.agentHost(port, certs), c.dc)
		checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := ac.Connect(checkCtx)
		cancel()
		if err == nil {
			c.agent = ac
			c.port, c.gen = port, gen
			return nil
		}
		lastErr = withAgentLog(ctx, c, fmt.Errorf("health check on port %d failed: %w", port, err))
		c.killAgent(ctx)
	}
	return lastErr
}

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
// 客户端证书 + 只验证书链不验主机名（证书 SAN 为会话级固定值，与主机地址无关）。
func (c *Conn) agentHost(port int, certs *pushcerts.Session) *model.Host {
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
	if c.dc.AgentCertRotateMinOrDefault() <= 0 {
		return nil
	}
	certs, gen, err := pushcerts.Material(c.dc)
	if err != nil {
		return err
	}
	if c.gen == gen {
		return nil
	}
	return c.migrateCerts(ctx, certs, gen)
}

// migrateCerts 将本主机迁移到新一代材料：SSH 重连 → 覆盖上传证书三件套 →
// 重启 agent（原端口优先，沿用自举的随机端口重试）→ 新配置健康检查通过后
// 原子换入，随后照常释放 SSH。
func (c *Conn) migrateCerts(ctx context.Context, certs *pushcerts.Session, gen uint64) error {
	if err := c.ssh.Connect(ctx); err != nil {
		return fmt.Errorf("cert rotation SSH reconnect failed: %w", err)
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
		ac := agentc.New(c.agentHost(port, certs), c.dc)
		checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := ac.Connect(checkCtx)
		cancel()
		if err == nil {
			_ = c.agent.Close()
			c.agent, c.port, c.gen = ac, port, gen
			_ = c.ssh.Close() // 迁移完毕释放 SSH（与自举后行为一致）
			return nil
		}
		lastErr = withAgentLog(ctx, c, fmt.Errorf("health check on port %d failed: %w", port, err))
	}
	_ = c.ssh.Close()
	return fmt.Errorf("certificate rotation failed: %w", lastErr)
}

// Close 关闭：shutdown 临时 agent（自删二进制与证书），keep_agent 时保留。
// shutdown 失败（agent 假死/网络闪断）时重连 SSH 兜底杀进程并清理产物，
// 否则目标机会残留运行中的 agent、二进制与私钥。
func (c *Conn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock() // 与 Exec/迁移路径串行：防止关闭与在途任务竞态
	if c.started && c.agent != nil && !c.host.KeepAgent {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := c.agent.Shutdown(ctx)
		cancel()
		if err != nil {
			c.shutdownFallback()
		}
	}
	c.agent = nil
	if c.ssh != nil {
		_ = c.ssh.Close()
	}
	return nil
}

// shutdownFallback shutdown 失败后的 SSH 兜底清理（独立限时 ctx；
// 重连 → 杀进程 → 删产物，全部 best-effort，失败时告警残留风险）。
func (c *Conn) shutdownFallback() {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := c.ssh.Connect(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "[push] host %s agent shutdown failed and SSH reconnect failed; agent process/binary/certs may remain on the target\n", c.host.Name)
		return
	}
	c.killAgent(ctx)
	c.cleanupArtifacts()
	_ = c.ssh.Close()
}

// Hostname 返回主机名。
func (c *Conn) Hostname() string { return c.host.Name }

// isTransportError 判定 agent 调用错误是否传输层失败（不可达/断连/握手
// 失败/响应体中途截断）——http.Client 的网络错误为 *url.Error；HTTP 状态码
// 错误是普通 error（agent 活着，业务层拒绝），不触发自愈。
func isTransportError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return true // 响应体传输中途断开（Decode 报 unexpected EOF，非 url.Error）
	}
	var ue *url.Error
	return errors.As(err, &ue)
}

// healIfUnreachable agent 传输层失败后的自愈：重置自举状态重走全流程，
// 让本连接的后续任务恢复可用。本次操作不重放——脚本可能已在远端执行，
// 盲目重试会重复执行非幂等任务；错误原样上抛，由调用方决定是否重跑。
// 用户主动取消/整体超时（ctx 已结束）不触发：那是运行生命周期在收尾，
// 重连没有意义且会拖慢整批主机的失败上报。
func (c *Conn) healIfUnreachable(ctx context.Context, err error) {
	if !isTransportError(err) || ctx.Err() != nil {
		return
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return
	}
	if c.agent != nil {
		_ = c.agent.Close()
		c.agent = nil
	}
	c.started = false
	// 旧 agent 若只是断连仍存活，会残留至空闲超时自清理（自举注入的
	// --idle-timeout 兜底）；新自举换新随机后缀与端口，互不冲突。
	// 上限 60s：自举含整个二进制重传，慢链路下 30s 不够
	hctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if cerr := c.Connect(hctx); cerr != nil {
		fmt.Fprintf(os.Stderr, "[push] host %s agent unreachable and re-bootstrap failed: %v\n", c.host.Name, cerr)
	}
}

// Exec 执行（agent 就绪走 agent 并先做轮换检查，否则回退 SSH）。
func (c *Conn) Exec(ctx context.Context, req conn.ExecRequest) (conn.ExecResult, error) {
	if c.agent != nil {
		c.mu.Lock()
		defer c.mu.Unlock()
		if err := c.rotateIfDue(ctx); err != nil {
			return conn.ExecResult{}, err
		}
		res, err := c.agent.Exec(ctx, req)
		c.healIfUnreachable(ctx, err)
		return res, err
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
		err := c.agent.UploadFile(ctx, dst, r, mode)
		c.healIfUnreachable(ctx, err)
		return err
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
		err := c.agent.DownloadFile(ctx, src, w)
		c.healIfUnreachable(ctx, err)
		return err
	}
	return c.ssh.DownloadFile(ctx, src, w)
}

// NativeExtract 转发给临时 agent（自举二进制与控制端同版本，端点必然存在）；
// SSH 回退态（自举失败）返回 ErrNativeUnsupported，模块回退 shell 路径。
func (c *Conn) NativeExtract(ctx context.Context, src, dest string) error {
	if c.agent == nil {
		return conn.ErrNativeUnsupported
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.rotateIfDue(ctx); err != nil {
		return err
	}
	err := c.agent.NativeExtract(ctx, src, dest)
	c.healIfUnreachable(ctx, err)
	return err
}

func randToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func randomPort() int {
	// crypto/rand 取端口，不可预测（时间戳端口可被同网段探测者提前抢占/扫描）；
	// 避开常见服务端口段
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 20000 + int(time.Now().UnixNano()%40000) // 极端退化：熵源失败回退时间戳
	}
	return 20000 + int(binary.LittleEndian.Uint32(b[:]))%40000
}

// firstLines 取输出前 n 字符（多行错误诊断保留足够上下文）。
func firstLines(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
