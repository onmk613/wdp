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
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"wdp/internal/conn"
	"wdp/internal/conn/agentc"
	"wdp/internal/conn/sshc"
	"wdp/internal/inventory"
	"wdp/internal/model"
	"wdp/internal/pushcerts"
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
