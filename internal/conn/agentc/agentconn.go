// Package agentc 通过 HTTP(S) 调用远端常驻 agent 的连接实现。
// 认证为 mTLS 双向证书（inventory 的 ca_file/cert_file/key_file 或内联 PEM）。
package agentc

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"wdp/internal/conn"
	"wdp/internal/inventory"
	"wdp/internal/model"
)

func init() {
	// 本连接类型的主机条目专属键（inventory 白名单经 blank-import 注册）
	inventory.RegisterHostKeys("agent_url", "agent_port",
		"ca_file", "cert_file", "key_file",
		"tls", "insecure_skip_verify", "tls_skip_host_verify", "tls_server_name")
	conn.RegisterFactory("agent", func(h *model.Host, dc *conn.Defaults) (conn.Conn, error) {
		return New(h, dc), nil
	})
}

// Conn 是 agent HTTP(S) 连接。
type Conn struct {
	host   *model.Host
	base   string
	client *http.Client
	tlsErr error          // 构造期 TLS 配置错误（Connect 时显式报出）
	dc     *conn.Defaults // 组合根注入的默认值（nil = 内置默认）
}

// New 创建 agent 连接。TLS 启用条件（任一）：
// 显式 tls: true / agent_url 为 https / 配置了 CA 或客户端证书（文件或内联
// PEM）/ 降级或改名开关。CA 未配置时信任系统证书池（公网 CA 场景）；
// 证书文件/数据加载失败显式报错。
func New(h *model.Host, dc *conn.Defaults) *Conn {
	useTLS := h.TLS || h.CAFile != "" || h.KeyFile != "" || h.CertFile != "" ||
		len(h.CAData) > 0 || len(h.CertData) > 0 ||
		h.InsecureSkipVerify || h.TLSSkipHostVerify || h.TLSServerName != ""
	base := h.AgentURL
	if base == "" {
		scheme := "http"
		if useTLS {
			scheme = "https"
		}
		addr := h.Address
		// SplitHostPort 判断是否已含端口：host:port 与 [ipv6]:port 原样保留，
		// 裸域名/裸 IP（含 IPv6 字面量，其 ":" 不代表端口）拼接 agent 端口
		if _, _, err := net.SplitHostPort(addr); err != nil {
			port := h.AgentPort
			if port == 0 {
				port = dc.AgentPortOrDefault() // wdp.cfg [agent].port 经组合根注入，缺省 7602
			}
			addr = net.JoinHostPort(addr, strconv.Itoa(port))
		}
		base = scheme + "://" + addr
	}
	if strings.HasPrefix(base, "https://") {
		useTLS = true
	}
	c := &Conn{
		host:   h,
		base:   strings.TrimRight(base, "/"),
		client: &http.Client{Timeout: 0}, // 单请求超时由 ctx 控制
		dc:     dc,
	}
	if useTLS {
		tlsCfg, err := buildTLSConfig(h)
		if err != nil {
			c.tlsErr = err
			return c
		}
		c.client.Transport = &http.Transport{TLSClientConfig: tlsCfg}
	}
	return c
}

// Connect 校验 agent 可达（10s 或 host 配置）。
func (c *Conn) Connect(ctx context.Context) error {
	if c.tlsErr != nil {
		return c.tlsErr // fail-loud：证书配置错误不静默降级
	}
	timeout := 10 * time.Second
	if c.host.ConnectTimeoutSec > 0 {
		timeout = time.Duration(c.host.ConnectTimeoutSec) * time.Second
	}
	ctx2, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx2, http.MethodGet, c.base+"/health", nil)
	if err != nil {
		return err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("agent unreachable %s: %w", c.base, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("agent health check failed: HTTP %d", resp.StatusCode)
	}
	return nil
}

// Close 释放 HTTP 连接池的空闲连接（幂等；进行中的请求不受影响）。
// transport 未特化时委托给默认共享实例，仅回收空闲连接，无副作用。
func (c *Conn) Close() error {
	c.client.CloseIdleConnections()
	return nil
}

// Hostname 返回主机名。
func (c *Conn) Hostname() string { return c.host.Name }

// BaseURL 返回连接地址（push 自举复用）。
func (c *Conn) BaseURL() string { return c.base }
