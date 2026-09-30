package web

// agent 客户端复用缓存：批量执行（exec/run/升级）在同一主机上的连续
// 请求共享 TCP/TLS 连接——此前每任务新建 Transport + Close 关空闲，
// 几十台 × 每台 N 任务时握手开销线性放大（mTLS 非对称握手尤其贵）。
//
// 缓存键 = TLS 形态（明文/mTLS）+ mTLS 下的证书校验名（hostNameOf(地址)）：
// mTLS 客户端证书与信任链是进程级常量（cam 的 ctl 材料，bootstrapCA 装配后
// 只读——续期发生在下次启动的 bootstrap 阶段，进程内不换材料，故无需失效），
// 逐主机唯一变化的是证书校验必须按台账地址进行的 ServerName。此处曾只按
// 形态分桶：第一台主机的 ServerName 被固化进共享 Transport，批量执行访问
// 其余主机时证书校验全部错位（hostname mismatch，fail-closed）——与
// probeClientFor/caMaterial.tlsClientFor 的"按校验名分桶"口径对齐后修复。
// Transport 参数与 agentc.New 内建的一致，另拉高 MaxIdleConnsPerHost
// （默认 2 会频繁断连）。

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"sync"
	"time"

	"wdp/internal/model"
	"wdp/internal/store"
)

// agentClientCache 主机 → *http.Client。clientFor 被批量执行的每主机
// goroutine 并发调用，全部读写都在 mu 内进行。
type agentClientCache struct {
	mu    sync.Mutex
	plain *http.Client            // 明文形态共享（无逐主机差异）
	mtls  map[string]*http.Client // 校验名 → mTLS 客户端
}

// clientFor 返回该主机适用的共享客户端；mTLS 材料损坏（换证窗口等极端
// 场景）返回 nil，调用方退回每请求新建连接的老路径，错误语义由
// agentc.New 原样给出。
func (c *agentClientCache) clientFor(s *Server, h *store.Host) *http.Client {
	if !s.useTLS(h) {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.plain == nil {
			c.plain = &http.Client{Timeout: 0, Transport: agentTransport(nil)}
		}
		return c.plain
	}
	name := hostNameOf(h.Address)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.mtls == nil {
		c.mtls = map[string]*http.Client{}
	}
	if cl, ok := c.mtls[name]; ok {
		return cl
	}
	m := s.agentHostModel(h)
	tr, err := mtlsTransport(m)
	if err != nil {
		return nil
	}
	cl := &http.Client{Timeout: 0, Transport: tr}
	c.mtls[name] = cl
	return cl
}

// agentTransport 明文/共享 Transport（与 agentc.New 内建参数一致，
// MaxIdleConnsPerHost 拉高支持同主机并发）。
func agentTransport(tlsCfg *tls.Config) *http.Transport {
	return &http.Transport{
		Proxy:               nil,
		TLSClientConfig:     tlsCfg,
		TLSHandshakeTimeout: 10 * time.Second,
		IdleConnTimeout:     90 * time.Second,
		MaxIdleConns:        256,
		MaxIdleConnsPerHost: 16,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
	}
}

// mtlsTransport 按主机模型构造 mTLS Transport（证书来自 server 的 ctl
// 材料，进程级一致；ServerName 取该主机的台账校验名，逐主机唯一差异）。
func mtlsTransport(m *model.Host) (*http.Transport, error) {
	cert, err := tls.X509KeyPair(m.CertData, m.KeyData)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if len(m.CAData) > 0 && !pool.AppendCertsFromPEM(m.CAData) {
		return nil, errors.New("agent CA material is not parseable PEM")
	}
	return agentTransport(&tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      pool,
		ServerName:   m.TLSServerName,
		MinVersion:   tls.VersionTLS12,
	}), nil
}
