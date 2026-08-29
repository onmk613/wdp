package push

import (
	"context"
	"fmt"
	"os"
	"time"
)

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
