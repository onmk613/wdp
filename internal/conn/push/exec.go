package push

import (
	"context"
	"io"
	"io/fs"

	"wdp/internal/conn"
)

// Hostname 返回主机名。
func (c *Conn) Hostname() string { return c.host.Name }

// 任务接口实现：agent 判定与迁移/自愈/关闭同锁——多个 fanOut goroutine
// 共享本连接（delegate_to）时，锁外读 c.agent 与持锁写方（rotate/heal/
// Close）构成数据竞态。healIfUnreachable 及其内部 bootstrap 不取 c.mu，
// 锁内调用安全；SSH 回退分支同样在锁内（字段注释的"串行化任务与证书
// 迁移"契约对回退态同样成立）。

// Exec 执行（agent 就绪走 agent 并先做轮换检查，否则回退 SSH）。
func (c *Conn) Exec(ctx context.Context, req conn.ExecRequest) (conn.ExecResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.agent == nil {
		return c.ssh.Exec(ctx, req)
	}
	if err := c.rotateIfDue(ctx); err != nil {
		return conn.ExecResult{}, err
	}
	res, err := c.agent.Exec(ctx, req)
	c.healIfUnreachable(ctx, err)
	return res, err
}

// UploadFile 上传（同 Exec 的轮换与回退策略；迁移在消费 reader 前完成）。
func (c *Conn) UploadFile(ctx context.Context, dst string, r io.Reader, mode fs.FileMode) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.agent == nil {
		return c.ssh.UploadFile(ctx, dst, r, mode)
	}
	if err := c.rotateIfDue(ctx); err != nil {
		return err
	}
	err := c.agent.UploadFile(ctx, dst, r, mode)
	c.healIfUnreachable(ctx, err)
	return err
}

// DownloadFile 下载（同 Exec 的轮换与回退策略）。
func (c *Conn) DownloadFile(ctx context.Context, src string, w io.Writer) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.agent == nil {
		return c.ssh.DownloadFile(ctx, src, w)
	}
	if err := c.rotateIfDue(ctx); err != nil {
		return err
	}
	err := c.agent.DownloadFile(ctx, src, w)
	c.healIfUnreachable(ctx, err)
	return err
}

// NativeExtract 转发给临时 agent（自举二进制与控制端同版本，端点必然存在）；
// SSH 回退态（自举失败）返回 ErrNativeUnsupported，模块回退 shell 路径。
func (c *Conn) NativeExtract(ctx context.Context, src, dest string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.agent == nil {
		return conn.ErrNativeUnsupported
	}
	if err := c.rotateIfDue(ctx); err != nil {
		return err
	}
	err := c.agent.NativeExtract(ctx, src, dest)
	c.healIfUnreachable(ctx, err)
	return err
}
