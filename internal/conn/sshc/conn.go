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
	sftp   *sftp.Client
}

// New 创建 SSH 连接（未建连）。
func New(h *model.Host) *Conn { return &Conn{host: h} }

// Connect 建立 SSH 连接并初始化 SFTP（可用时）。
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
		c.sftp = sc
	}
	return nil
}

// Close 关闭连接。
func (c *Conn) Close() error {
	if c.sftp != nil {
		c.sftp.Close()
		c.sftp = nil
	}
	if c.client != nil {
		err := c.client.Close()
		c.client = nil
		return err
	}
	return nil
}

// Hostname 返回主机名。
func (c *Conn) Hostname() string { return c.host.Name }

func (c *Conn) ensureClient() error {
	if c.client == nil {
		return errors.New("connection not established")
	}
	return nil
}
