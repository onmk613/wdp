// Package sshc 是基于 golang.org/x/crypto/ssh 的连接实现。
// 目标机仅需 POSIX sh（脚本经 base64 传输，规避引号转义问题）；
// 文件传输优先 SFTP，目标机未启用 SFTP 子系统时降级为 exec 流式传输。
package sshc

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"wdp/internal/conn"
	"wdp/internal/model"
)

func init() {
	conn.RegisterFactory("ssh", func(h *model.Host, dc *conn.Defaults) (conn.Conn, error) {
		return New(h), nil
	})
}

// Conn 是 SSH 连接。
type Conn struct {
	host   *model.Host
	client *ssh.Client

	// sftpMu 保护 sftp/sftpDead：delegate_to 会让多个源主机的传输 goroutine
	// 共享同一目标主机的 Conn，取消路径要关闭 SFTP 通道解除阻塞——没有
	// 锁的话该写与其他 goroutine 的裸读构成数据竞争与 TOCTOU nil 解引用。
	sftpMu   sync.Mutex
	sftp     *sftp.Client
	sftpDead bool // 通道曾被取消路径关闭：永久降级 exec 流式，不重建
}

// New 创建 SSH 连接（未建连）。
func New(h *model.Host) *Conn { return &Conn{host: h} }

// Connect 建立 SSH 连接并初始化 SFTP（可用时）。幂等重入依赖调用方
// 串行调用（manager.Get 与 healingConn.current 各自持锁保证）；并发
// 调用会双重拨号并泄漏第一条连接。
func (c *Conn) Connect(_ context.Context) error {
	if c.client != nil {
		return nil
	}
	addr := net.JoinHostPort(c.host.Address, fmt.Sprint(c.host.Port))
	timeout := 10 * time.Second
	if c.host.ConnectTimeoutSec > 0 {
		timeout = time.Duration(c.host.ConnectTimeoutSec) * time.Second
	}
	methods, closeAgent, keyWarns := authMethods(c.host)
	cfg := &ssh.ClientConfig{
		User:            c.host.User,
		Auth:            methods,
		HostKeyCallback: hostKeyCallback(c.host),
		Timeout:         timeout,
	}
	// ssh.Dial 不接受 ctx，TCP+握手超时由 cfg.Timeout 控制
	client, err := ssh.Dial("tcp", addr, cfg)
	closeAgent() // 认证在 Dial 内同步完成，agent 的 unix 连接此后不再使用
	if err != nil {
		// 附带私钥解析阶段的诊断：密钥损坏/口令错误时明确指出，
		// 而不是静默滑落到密码认证后被"密码错误"误导
		if len(keyWarns) > 0 {
			return fmt.Errorf("ssh connection failed: %w (private key issues: %s)", err, strings.Join(keyWarns, "; "))
		}
		return fmt.Errorf("ssh connection failed: %w", err)
	}
	c.client = client
	// SFTP 可选（失败时回退 exec 传输）
	if sc, err := sftp.NewClient(c.client); err == nil {
		c.sftpMu.Lock()
		c.sftp = sc
		c.sftpMu.Unlock()
	}
	return nil
}

// Close 关闭连接。
func (c *Conn) Close() error {
	c.sftpMu.Lock()
	if c.sftp != nil {
		c.sftp.Close()
		c.sftp = nil
	}
	c.sftpMu.Unlock()
	if c.client != nil {
		err := c.client.Close()
		c.client = nil
		return err
	}
	return nil
}

// sftpClient 返回当前可用的 SFTP 客户端快照；通道从未建立或已被取消
// 路径关闭时返回 nil（调用方降级 exec 流式——streamUpload 原生响应 ctx）。
func (c *Conn) sftpClient() *sftp.Client {
	c.sftpMu.Lock()
	defer c.sftpMu.Unlock()
	if c.sftpDead || c.sftp == nil {
		return nil
	}
	return c.sftp
}

// killSftp 关闭 SFTP 通道解除阻塞中的传输（SFTP 协议无 deadline）并标记
// 永久降级。只关闭不置 nil：共享该连接的其他传输 goroutine 持有的指针
// 仍可安全调用（对已关闭通道的操作返回错误而非 panic）；后续新传输经
// sftpClient 看到 sftpDead 后走 exec 流式。
func (c *Conn) killSftp() {
	c.sftpMu.Lock()
	defer c.sftpMu.Unlock()
	if c.sftpDead || c.sftp == nil {
		return
	}
	c.sftpDead = true
	_ = c.sftp.Close()
}

// Hostname 返回主机名。
func (c *Conn) Hostname() string { return c.host.Name }

func (c *Conn) ensureClient() error {
	if c.client == nil {
		return errors.New("connection not established")
	}
	return nil
}
