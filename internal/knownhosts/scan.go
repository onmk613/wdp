package knownhosts

import (
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"wdp/internal/model"
)

// ScanHostKey 与目标机完成 SSH 握手并采集主机公钥（认证前阶段，凭据错误不影响采集）。
func ScanHostKey(h *model.Host) (ssh.PublicKey, error) {
	timeout := 10 * time.Second
	if h.ConnectTimeoutSec > 0 {
		timeout = time.Duration(h.ConnectTimeoutSec) * time.Second
	}
	addr := net.JoinHostPort(h.Address, fmt.Sprint(h.Port))
	var captured ssh.PublicKey
	cfg := &ssh.ClientConfig{
		User: h.User,
		Auth: []ssh.AuthMethod{ssh.Password("")}, // 占位：指纹在认证前交换
		HostKeyCallback: func(hostname string, remote net.Addr, key ssh.PublicKey) error {
			captured = key
			return nil
		},
		Timeout: timeout,
	}
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return nil, fmt.Errorf("tcp connection failed: %w", err)
	}
	defer conn.Close()
	sshc, _, _, err := ssh.NewClientConn(conn, addr, cfg)
	if sshc != nil {
		sshc.Close()
	}
	if captured != nil {
		return captured, nil // 认证失败无所谓，指纹已到手
	}
	if err != nil {
		return nil, fmt.Errorf("SSH handshake failed: %w", err)
	}
	return nil, errors.New("no host public key captured")
}

// KnownHostsMarker 返回主机在 known_hosts 中的主机段：非 22 端口或地址含
// 冒号（IPv6 字面量）一律 [host]:port——对齐 OpenSSH put_host_port 行为，
// 否则 IPv6@22 写成裸地址，与校验侧 JoinHostPort 产物 [addr]:22 失配。
func KnownHostsMarker(h *model.Host) string {
	if h.Port != 22 || strings.Contains(h.Address, ":") {
		return fmt.Sprintf("[%s]:%d", h.Address, h.Port)
	}
	return h.Address
}

// KnownHostsLine 生成 known_hosts 行（非 22 端口用 [host]:port 格式）。
func KnownHostsLine(h *model.Host, key ssh.PublicKey) string {
	return knownhosts.Line([]string{KnownHostsMarker(h)}, key)
}
