// Package agentc 通过 HTTP(S) 调用远端常驻 agent 的连接实现。
// 认证为 mTLS 双向证书（inventory 的 ca_file/cert_file/key_file 或内联 PEM）。
package agentc

import (
	"context"
	"fmt"
	"io"
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
	shared bool           // client 由 NewWithClient 注入：Close 不关连接（所有权归注入方）
}

// New 创建 agent 连接。TLS 启用条件（任一）：
// 显式 tls: true / agent_url 为 https / 配置了 CA 或客户端证书（文件或内联
// PEM，CA/Cert/Key 三者任一形态都算——漏 KeyData 会让"仅内联私钥"的主机
// 静默走明文）/ 降级或改名开关。CA 未配置时信任系统证书池（公网 CA 场景）；
// 证书文件/数据加载失败显式报错。
// NewWithClient 用调用方提供的 http.Client 创建连接（批量执行的热路径
// 优化：共享 Transport 让同主机的连续请求复用 TCP/TLS 连接，省掉每次
// 完整握手）。连接生命周期归调用方——本形态下 Close 不关空闲连接
// （Transport 的所有权在注入方）。client 为 nil 时等价 New。
func NewWithClient(h *model.Host, dc *conn.Defaults, client *http.Client) *Conn {
	c := New(h, dc)
	if client != nil {
		c.client = client
		c.shared = true
	}
	return c
}

func New(h *model.Host, dc *conn.Defaults) *Conn {
	useTLS := h.TLS || h.CAFile != "" || h.KeyFile != "" || h.CertFile != "" ||
		len(h.CAData) > 0 || len(h.CertData) > 0 || len(h.KeyData) > 0 ||
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
	// agent 直连：显式禁用环境代理（HTTP_PROXY 会把私网地址请求带向
	// 无关服务，其响应会冒充 agent 结论——如代理返回的 400）。
	// 不设 ResponseHeaderTimeout/整体 Timeout：/exec 长任务（脚本跑数分钟
	// 才回包）与 /file 大传输会被掐断，单请求超时仍由调用方 ctx 控制；
	// 握手与空闲连接必须收紧：半死 agent（TCP 通不回包）下 ctx 无 deadline
	// 时调用方 goroutine 会无限挂起。
	tr := &http.Transport{
		Proxy:               nil,
		TLSHandshakeTimeout: 10 * time.Second,
		IdleConnTimeout:     90 * time.Second,
	}
	c := &Conn{
		host:   h,
		base:   strings.TrimRight(base, "/"),
		client: &http.Client{Timeout: 0, Transport: tr}, // 单请求超时由 ctx 控制
		dc:     dc,
	}
	if useTLS && strings.HasPrefix(base, "http://") {
		// fail-loud：agent_url 显式 http:// 却同时配置了 TLS 材料（tls: true /
		// ca_file / cert_file / key_file），二者矛盾——静默忽略 TLS 意味着脚本、
		// become 密码、文件内容全部明文传输。错误信息指出两处配置，由使用方
		// 修正。判定先于材料加载：加载失败不能抢先把矛盾吞掉继续明文。
		c.tlsErr = fmt.Errorf(
			"agent_url %q is http:// while TLS material is configured (tls/ca_file/cert_file/key_file): refusing to send scripts and credentials in plaintext; switch agent_url to https:// or remove the TLS settings", base)
		// 仅 Connect 报错不够（agentctl status 等路径不经 Connect）：让该连接上
		// 的一切请求都立即失败
		c.client.Transport = failingTransport{err: c.tlsErr}
		return c
	}
	if useTLS {
		tlsCfg, err := buildTLSConfig(h)
		if err != nil {
			c.tlsErr = err
			return c
		}
		tr.TLSClientConfig = tlsCfg
	}
	return c
}

// failingTransport 让矛盾配置（http:// + TLS 材料）下的一切请求失败，
// 保证不经 Connect 的调用路径（Health/Logs 等）同样 fail-loud。
type failingTransport struct{ err error }

func (f failingTransport) RoundTrip(*http.Request) (*http.Response, error) { return nil, f.err }

// Connect 校验 agent 可达（10s 超时）。
func (c *Conn) Connect(ctx context.Context) error {
	if c.tlsErr != nil {
		return c.tlsErr // fail-loud：证书配置错误不静默降级
	}
	ctx2, cancel := context.WithTimeout(ctx, 10*time.Second)
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
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("agent health check %s: HTTP %d %s", c.base+"/health", resp.StatusCode,
			strings.TrimSpace(string(body)))
	}
	return nil
}

// Close 释放 HTTP 连接池的空闲连接（幂等；进行中的请求不受影响）。
// transport 未特化时委托给默认共享实例，仅回收空闲连接，无副作用。
func (c *Conn) Close() error {
	if !c.shared {
		c.client.CloseIdleConnections()
	}
	return nil
}

// Hostname 返回主机名。
func (c *Conn) Hostname() string { return c.host.Name }

// BaseURL 返回连接地址（push 自举复用）。
func (c *Conn) BaseURL() string { return c.base }
