package push

import (
	"context"
	"fmt"
	"time"

	"wdp/internal/conn/agentc"
	"wdp/internal/pushcerts"
)

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
		// 与自举流程对齐：本轮拉起的 agent 未通过健康检查时立即杀掉，
		// 最后一轮失败不再泄漏带着新证书的远端进程
		c.killAgent(ctx)
	}
	_ = c.ssh.Close()
	return fmt.Errorf("certificate rotation failed: %w", lastErr)
}
