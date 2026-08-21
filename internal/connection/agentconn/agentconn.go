// Package agentconn 通过 HTTP(S) 调用远端常驻 agent 的连接实现。
// 认证为 mTLS 双向证书（inventory 的 ca_file/cert_file/key_file 或内联 PEM）。
package agentconn

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"wdp/internal/config"
	"wdp/internal/connection"
	"wdp/internal/model"
)

func init() {
	connection.RegisterFactory("agent", func(h *model.Host) (connection.Connection, error) {
		return New(h), nil
	})
}

// Conn 是 agent HTTP(S) 连接。
type Conn struct {
	host   *model.Host
	base   string
	client *http.Client
	tlsErr error // 构造期 TLS 配置错误（Connect 时显式报出）
}

// New 创建 agent 连接。TLS 启用条件（任一）：
// 显式 tls: true / agent_url 为 https / 配置了 CA 或客户端证书（文件或内联
// PEM）/ 降级或改名开关。CA 未配置时信任系统证书池（公网 CA 场景）；
// 证书文件/数据加载失败显式报错。
func New(h *model.Host) *Conn {
	useTLS := h.TLS || h.CAFile != "" || len(h.CAData) > 0 || len(h.CertData) > 0 ||
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
				port = config.Current().AgentPort() // wdp.cfg [agent].port，缺省 7602
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

// buildTLSConfig 构造客户端 TLS 配置。主机名校验目标（tls_server_name 的解析）：
//
//	显式指定 → 按该名称校验
//	未指定 + agent_url + 自建 CA + host 字段 → 默认按 host 字段（inventory 未填 host
//	                                    时即主机名）校验：端口转发/NAT 下入口地址
//	                                    （如 127.0.0.1）不是证书 SAN 中的逻辑地址
//	其余 → 按连接地址（URL 主机，标准 TLS 行为）
//
// 校验强度：insecure_skip_verify 跳过全部；tls_skip_host_verify 仅跳过主机名、
// 保留 CA 链校验；默认链校验 + 主机名校验。
// 信任根（ca_file > 系统证书池）、客户端证书（cert_file/key_file）加载失败显式报错。
func buildTLSConfig(h *model.Host) (*tls.Config, error) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if h.TLSServerName != "" {
		cfg.ServerName = h.TLSServerName
	} else if h.AgentURL != "" && h.CAFile != "" && h.Address != "" {
		// 自定义入口 + 自建 CA：按逻辑身份（host 字段，inventory 缺省即主机名）而非入口地址校验
		cfg.ServerName = h.Address
	}
	var pool *x509.CertPool
	if h.CAFile != "" {
		pool = x509.NewCertPool()
		pemBytes, err := os.ReadFile(h.CAFile)
		if err != nil {
			return nil, fmt.Errorf("读取 CA 证书失败: %w", err)
		}
		if !pool.AppendCertsFromPEM(pemBytes) {
			return nil, fmt.Errorf("解析 CA 证书失败（%s）", h.CAFile)
		}
		cfg.RootCAs = pool
	} else if len(h.CAData) > 0 {
		pool = x509.NewCertPool()
		if !pool.AppendCertsFromPEM(h.CAData) {
			return nil, fmt.Errorf("解析 CA 证书数据失败（内联 PEM）")
		}
		cfg.RootCAs = pool
	}
	// CA 未配置 = 信任系统证书池（公网 CA 场景）

	switch {
	case h.InsecureSkipVerify:
		cfg.InsecureSkipVerify = true // 全量降级：链与主机名校验均跳过
	case h.TLSSkipHostVerify:
		// 仅跳过主机名：Go 无原生"只验链不验名"开关，关闭内置校验后手动执行链校验
		roots := pool
		if roots == nil {
			sys, err := x509.SystemCertPool()
			if err != nil {
				return nil, err
			}
			roots = sys
		}
		cfg.InsecureSkipVerify = true
		cfg.VerifyPeerCertificate = chainVerifier(roots)
	}

	if h.CertFile != "" {
		cert, err := tls.LoadX509KeyPair(h.CertFile, h.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("加载客户端证书对失败: %w", err)
		}
		cfg.Certificates = []tls.Certificate{cert}
	} else if len(h.CertData) > 0 {
		cert, err := tls.X509KeyPair(h.CertData, h.KeyData)
		if err != nil {
			return nil, fmt.Errorf("加载客户端证书对失败（内联 PEM）: %w", err)
		}
		cfg.Certificates = []tls.Certificate{cert}
	}
	return cfg, nil
}

// chainVerifier 构造"仅链校验"的证书回调（tls_skip_host_verify 用）：
// 校验服务端证书由可信 CA 签发（含中间链），不做主机名匹配。
func chainVerifier(roots *x509.CertPool) func([][]byte, [][]*x509.Certificate) error {
	return func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		if len(rawCerts) == 0 {
			return fmt.Errorf("服务端未提供证书")
		}
		certs := make([]*x509.Certificate, 0, len(rawCerts))
		for _, raw := range rawCerts {
			c, err := x509.ParseCertificate(raw)
			if err != nil {
				return fmt.Errorf("解析服务端证书失败: %w", err)
			}
			certs = append(certs, c)
		}
		opts := x509.VerifyOptions{
			Roots:     roots,
			KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		}
		if len(certs) > 1 {
			opts.Intermediates = x509.NewCertPool()
			for _, c := range certs[1:] {
				opts.Intermediates.AddCert(c)
			}
		}
		if _, err := certs[0].Verify(opts); err != nil {
			return fmt.Errorf("服务端证书链校验失败: %w", err)
		}
		return nil
	}
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
		return fmt.Errorf("agent 不可达 %s: %w", c.base, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("agent 健康检查失败: HTTP %d", resp.StatusCode)
	}
	return nil
}

// Close 释放资源（HTTP 无长连接状态）。
func (c *Conn) Close() error { return nil }

// Hostname 返回主机名。
func (c *Conn) Hostname() string { return c.host.Name }

// Exec 调用 agent 的 /exec。
func (c *Conn) Exec(ctx context.Context, req connection.ExecRequest) (connection.ExecResult, error) {
	becomeUser, becomePW := "", ""
	if req.BecomeUser != "" {
		becomeUser = req.BecomeUser
		becomePW = model.Secret(c.host.BecomePassword, c.host.BecomePasswordEnv)
	}
	body, _ := json.Marshal(map[string]any{
		"script":          req.Script,
		"stdin":           req.Stdin,
		"env":             req.Env,
		"timeout_ms":      req.TimeoutMs,
		"become_user":     becomeUser,
		"become_password": becomePW,
	})
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/exec", bytes.NewReader(body))
	if err != nil {
		return connection.ExecResult{}, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	resp, err := c.client.Do(hreq)
	if err != nil {
		return connection.ExecResult{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return connection.ExecResult{}, fmt.Errorf("agent exec HTTP %d: %s", resp.StatusCode, string(msg))
	}
	var out struct {
		Code     int    `json:"code"`
		Stdout   string `json:"stdout"`
		Stderr   string `json:"stderr"`
		TimedOut bool   `json:"timed_out"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return connection.ExecResult{}, fmt.Errorf("解析 agent 响应失败: %w", err)
	}
	if out.TimedOut {
		return connection.ExecResult{Code: out.Code, Stdout: out.Stdout, Stderr: out.Stderr},
			context.DeadlineExceeded
	}
	return connection.ExecResult{Code: out.Code, Stdout: out.Stdout, Stderr: out.Stderr}, nil
}

// UploadFile 调用 agent 的 /file 上传。
func (c *Conn) UploadFile(ctx context.Context, dst string, r io.Reader, mode fs.FileMode) error {
	if mode == 0 {
		mode = 0o644
	}
	url := fmt.Sprintf("%s/file?path=%s&mode=%o", c.base, queryEscape(dst), mode.Perm())
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, r)
	if err != nil {
		return err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("agent 上传 HTTP %d: %s", resp.StatusCode, string(msg))
	}
	return nil
}

// Shutdown 请求 agent 优雅退出（清理用）。
func (c *Conn) Shutdown(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/shutdown", nil)
	if err != nil {
		return err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("shutdown HTTP %d", resp.StatusCode)
	}
	return nil
}

// DownloadFile 调用 agent 的 /file 下载。
func (c *Conn) DownloadFile(ctx context.Context, src string, w io.Writer) error {
	url := fmt.Sprintf("%s/file?path=%s", c.base, queryEscape(src))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("agent 下载 HTTP %d: %s", resp.StatusCode, string(msg))
	}
	_, err = io.Copy(w, resp.Body)
	return err
}

// BaseURL 返回连接地址（push 自举复用）。
func (c *Conn) BaseURL() string { return c.base }

// NativeExtract 调用 agent 的 /archive 原生解压（远端 Go 实现，不依赖
// 目标机工具链）。旧版常驻 agent 无该端点（404/405）时返回
// connection.ErrNativeUnsupported，调用方回退 shell 路径；其余错误真实上抛。
func (c *Conn) NativeExtract(ctx context.Context, src, dest string) error {
	url := fmt.Sprintf("%s/archive?src=%s&dest=%s", c.base, queryEscape(src), queryEscape(dest))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed {
		return connection.ErrNativeUnsupported
	}
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("agent 解压 HTTP %d: %s", resp.StatusCode, string(msg))
	}
	return nil
}

func queryEscape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') ||
			ch == '-' || ch == '_' || ch == '.' || ch == '~' || ch == '/' {
			b.WriteByte(ch)
		} else {
			fmt.Fprintf(&b, "%%%02X", ch)
		}
	}
	return b.String()
}
