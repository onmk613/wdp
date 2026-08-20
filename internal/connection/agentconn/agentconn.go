// Package agentconn 通过 HTTP(S) 调用远端常驻 agent 的连接实现。
// 支持 token 认证与 mTLS 双向认证（inventory 的 token/ca_file/cert_file/key_file）。
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
	"net/http"
	"os"
	"strings"
	"time"

	"wdp/internal/config"
	"wdp/internal/connection"
	"wdp/internal/model"
)

// tokenHeader 与 agent 端的 X-WDP-Token 约定一致（避免包循环依赖，此处自定义常量）。
const tokenHeader = "X-WDP-Token"

func init() {
	connection.RegisterFactory("agent", func(h *model.Host) (connection.Connection, error) {
		return New(h), nil
	})
}

// Conn 是 agent HTTP(S) 连接。
type Conn struct {
	host   *model.Host
	base   string
	token  string
	client *http.Client
	tlsErr error // 构造期 TLS 配置错误（Connect 时显式报出）
}

// New 创建 agent 连接。TLS 启用条件（任一）：
// 显式 tls: true / agent_url 为 https / 配置了 CA / 内联证书数据 / insecure_skip_verify。
// CA 未配置时信任系统证书池（公网 CA 场景）；证书文件/数据加载失败显式报错。
func New(h *model.Host) *Conn {
	useTLS := h.TLS || h.CAFile != "" || h.InsecureSkipVerify
	base := h.AgentURL
	if base == "" {
		scheme := "http"
		if useTLS {
			scheme = "https"
		}
		addr := h.Address
		if !strings.Contains(addr, ":") {
			port := h.AgentPort
			if port == 0 {
				port = config.Current().AgentPort() // wdp.cfg [agent].port，缺省 7602
			}
			addr = fmt.Sprintf("%s:%d", addr, port)
		}
		base = scheme + "://" + addr
	}
	if strings.HasPrefix(base, "https://") {
		useTLS = true
	}
	c := &Conn{
		host:   h,
		base:   strings.TrimRight(base, "/"),
		token:  model.Secret(h.Token, h.TokenEnv),
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

// buildTLSConfig 构造客户端 TLS 配置：信任根（ca_file > 系统证书池）、
// 客户端证书（cert_file/key_file）、可选跳过校验。加载失败显式报错（不静默降级）。
func buildTLSConfig(h *model.Host) (*tls.Config, error) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if h.InsecureSkipVerify {
		cfg.InsecureSkipVerify = true // 明确声明的降级
	}
	if h.CAFile != "" {
		pool := x509.NewCertPool()
		pemBytes, err := os.ReadFile(h.CAFile)
		if err != nil {
			return nil, fmt.Errorf("读取 CA 证书失败: %w", err)
		}
		if !pool.AppendCertsFromPEM(pemBytes) {
			return nil, fmt.Errorf("解析 CA 证书失败（%s）", h.CAFile)
		}
		cfg.RootCAs = pool
	}
	// CA 未配置 = 信任系统证书池（公网 CA 场景）

	if h.CertFile != "" {
		cert, err := tls.LoadX509KeyPair(h.CertFile, h.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("加载客户端证书对失败: %w", err)
		}
		cfg.Certificates = []tls.Certificate{cert}
	}
	return cfg, nil
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
	c.auth(req)
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

// auth 注入认证头。
func (c *Conn) auth(req *http.Request) {
	if c.token != "" {
		req.Header.Set(tokenHeader, c.token)
	}
}

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
	c.auth(hreq)
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
	c.auth(req)
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
	c.auth(req)
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

// Token 返回连接 token（push 自举透传用）。
func (c *Conn) Token() string { return c.token }

// DownloadFile 调用 agent 的 /file 下载。
func (c *Conn) DownloadFile(ctx context.Context, src string, w io.Writer) error {
	url := fmt.Sprintf("%s/file?path=%s", c.base, queryEscape(src))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	c.auth(req)
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
