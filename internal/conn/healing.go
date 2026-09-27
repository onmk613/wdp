package conn

// 连接自愈：Manager 按主机复用连接（play 生命周期内免重复握手），但网络
// 断开/对端重启后缓存连接永久不可用——此前无失效机制，同 run 内该主机
// 后续全部操作（含 auto_rollback 的恢复动作，恰好是连接最可能已断的
// 时刻）都死在缓存的死连接上。Get 返回 healingConn 包装：操作返回传输级
// 错误时作废底层连接，下一次操作前自动重建。

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"net"
	"strings"
	"sync"

	"wdp/internal/model"
)

// healingConn 是 Manager.Get 返回的自愈包装。失效判定见 isBrokenConn；
// 已失效的操作**绝不自动重试**——Exec 的脚本可能已部分执行，重试策略
// 属于调用方（executor 的回滚路径对幂等动作做一次重试）。
// 可选能力（NativeExtractor/PrivilegedUploader）必须在这里委托：模块以
// 类型断言探测连接能力，漏一个能力就静默降级到 shell 路径。
type healingConn struct {
	mgr  *Manager
	host *model.Host

	mu    sync.Mutex
	inner Conn
}

// current 返回可用的底层连接；无则经 manager 的建连限流重建（握手期间
// 持锁串行化：并发的第二个调用方等重建完成直接复用，不重复握手）。
func (h *healingConn) current(ctx context.Context) (Conn, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.inner == nil {
		c, err := h.mgr.dial(ctx, h.host)
		if err != nil {
			return nil, err
		}
		h.inner = c
	}
	return h.inner, nil
}

// invalidate 作废底层连接（仅当仍是当初取出的那个实例——并发病程里
// 别人可能已重建）。
func (h *healingConn) invalidate(c Conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.inner == c {
		_ = c.Close()
		h.inner = nil
	}
}

func (h *healingConn) Connect(ctx context.Context) error {
	_, err := h.current(ctx)
	return err
}

func (h *healingConn) Close() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.inner == nil {
		return nil
	}
	err := h.inner.Close()
	h.inner = nil
	return err
}

func (h *healingConn) Hostname() string { return h.host.Name }

func (h *healingConn) Exec(ctx context.Context, req ExecRequest) (ExecResult, error) {
	c, err := h.current(ctx)
	if err != nil {
		return ExecResult{}, err
	}
	res, err := c.Exec(ctx, req)
	if isBrokenConn(err) {
		h.invalidate(c)
	}
	return res, err
}

func (h *healingConn) UploadFile(ctx context.Context, dst string, r io.Reader, mode fs.FileMode) error {
	c, err := h.current(ctx)
	if err != nil {
		return err
	}
	err = c.UploadFile(ctx, dst, r, mode)
	if isBrokenConn(err) {
		h.invalidate(c)
	}
	return err
}

func (h *healingConn) DownloadFile(ctx context.Context, src string, w io.Writer) error {
	c, err := h.current(ctx)
	if err != nil {
		return err
	}
	err = c.DownloadFile(ctx, src, w)
	if isBrokenConn(err) {
		h.invalidate(c)
	}
	return err
}

// NativeExtract 委托可选能力（归档原生解压，unarchive/artifact 以类型
// 断言探测；底层不支持时返回 ErrNativeUnsupported 由模块回退 shell）。
func (h *healingConn) NativeExtract(ctx context.Context, src, dest string) error {
	c, err := h.current(ctx)
	if err != nil {
		return err
	}
	nx, ok := c.(NativeExtractor)
	if !ok {
		return ErrNativeUnsupported
	}
	err = nx.NativeExtract(ctx, src, dest)
	if isBrokenConn(err) {
		h.invalidate(c)
	}
	return err
}

// UploadFileAs 委托可选能力（提权写，fileops 以类型断言探测）。
func (h *healingConn) UploadFileAs(ctx context.Context, dst string, r io.Reader, mode fs.FileMode, becomeUser string) error {
	c, err := h.current(ctx)
	if err != nil {
		return err
	}
	pu, ok := c.(PrivilegedUploader)
	if !ok {
		return ErrNativeUnsupported
	}
	err = pu.UploadFileAs(ctx, dst, r, mode, becomeUser)
	if isBrokenConn(err) {
		h.invalidate(c)
	}
	return err
}

// isBrokenConn 报告错误是否属于"底层连接已失效"（可作废重建）。传输层
// 各实现（ssh / agent http）未统一导出哨兵错误，按错误类判定：网络错误、
// EOF/连接关闭、syscall 复位类；ctx 取消/超时是调用方语义，不构成失效。
// ssh 会话层固定文案兜底（x/crypto 未导出哨兵）。
func isBrokenConn(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return true
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, net.ErrClosed) {
		return true
	}
	if brokenSyscall(err) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "ssh: connection") ||
		strings.Contains(msg, "exited without exit status")
}
