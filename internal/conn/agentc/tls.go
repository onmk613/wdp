package agentc

import (
	"bytes"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"os"

	"wdp/internal/model"
)

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
			return nil, fmt.Errorf("failed to read CA certificate: %w", err)
		}
		if !pool.AppendCertsFromPEM(pemBytes) {
			return nil, fmt.Errorf("failed to parse CA certificate (%s)", h.CAFile)
		}
		cfg.RootCAs = pool
	} else if len(h.CAData) > 0 {
		pool = x509.NewCertPool()
		if !pool.AppendCertsFromPEM(h.CAData) {
			return nil, errors.New("failed to parse CA certificate data (inline PEM)")
		}
		cfg.RootCAs = pool
	}
	// CA 未配置 = 信任系统证书池（公网 CA 场景）

	// 服务端证书 pin（push 通道）：精确比对叶子证书，优先于其它校验开关
	// ——它是比"跳过主机名"更强的保证，显式声明时必须生效。
	pin, err := parsePinnedCert(h.PeerCertData)
	if err != nil {
		return nil, err
	}
	switch {
	case pin != nil:
		roots := pool
		if roots == nil {
			sys, err := x509.SystemCertPool()
			if err != nil {
				return nil, err
			}
			roots = sys
		}
		cfg.InsecureSkipVerify = true
		cfg.VerifyPeerCertificate = pinnedVerifier(roots, pin)
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
			return nil, fmt.Errorf("failed to load client certificate pair: %w", err)
		}
		cfg.Certificates = []tls.Certificate{cert}
	} else if len(h.CertData) > 0 {
		cert, err := tls.X509KeyPair(h.CertData, h.KeyData)
		if err != nil {
			return nil, fmt.Errorf("failed to load client certificate pair (inline PEM): %w", err)
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
			return errors.New("server did not provide a certificate")
		}
		certs := make([]*x509.Certificate, 0, len(rawCerts))
		for _, raw := range rawCerts {
			c, err := x509.ParseCertificate(raw)
			if err != nil {
				return fmt.Errorf("failed to parse server certificate: %w", err)
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
			return fmt.Errorf("server certificate chain verification failed: %w", err)
		}
		return nil
	}
}

// parsePinnedCert 解析内联的期望服务端证书（pin）；空输入返回 nil。
func parsePinnedCert(pemBytes []byte) (*x509.Certificate, error) {
	if len(pemBytes) == 0 {
		return nil, nil
	}
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("pinned server certificate is not valid PEM")
	}
	c, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("failed to parse pinned server certificate: %w", err)
	}
	return c, nil
}

// pinnedVerifier 在链校验之上要求服务端叶子证书与 pin 逐字节一致
// （push 通道：会话服务端证书由控制端签发并上传，控制端持有精确副本，
// 共享证书无法按主机名区分但可以按证书本身 pin 住）。
func pinnedVerifier(roots *x509.CertPool, pin *x509.Certificate) func([][]byte, [][]*x509.Certificate) error {
	chain := chainVerifier(roots)
	return func(rawCerts [][]byte, verified [][]*x509.Certificate) error {
		if err := chain(rawCerts, verified); err != nil {
			return err
		}
		if !bytes.Equal(rawCerts[0], pin.Raw) {
			return fmt.Errorf("server certificate does not match the pinned certificate (expected sha256 %s, got %s)",
				certFingerprint(pin.Raw), certFingerprint(rawCerts[0]))
		}
		return nil
	}
}

// certFingerprint 返回证书 DER 的 sha256 前 16 位十六进制（错误消息用）。
func certFingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])[:16]
}
