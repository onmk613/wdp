package agent

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"wdp/internal/ca"
)

// ConfigureAuth 配置 mTLS 认证：ca 校验客户端证书、cert/key 为服务端证书。
// 三者全空 = 无认证模式（直接跳过）；部分提供报错。
func (s *Server) ConfigureAuth(ca, cert, key string) error {
	if ca == "" && cert == "" && key == "" {
		return nil
	}
	if ca == "" || cert == "" || key == "" {
		return errors.New("mTLS requires ca/cert/key all together")
	}

	pool := x509.NewCertPool()
	caPEM, err := os.ReadFile(ca)
	if err != nil {
		return fmt.Errorf("failed to read CA certificate: %w", err)
	}
	if !pool.AppendCertsFromPEM(caPEM) {
		return fmt.Errorf("failed to parse CA certificate: %s", ca)
	}
	pair, leaf, err := loadCertPair(cert, key)
	if err != nil {
		return err
	}

	s.tlsCAFile, s.tlsCertFile, s.tlsKeyFile = ca, cert, key

	old := s.material.Load()
	m := &tlsMaterial{cert: pair, leaf: leaf, clientCAs: pool}
	if old != nil {
		m.pins = old.pins
	}
	s.material.Store(m)

	return nil
}

// MTLSConfig 返回接线到当前材料快照的 TLS 配置。每次握手经
// GetConfigForClient 派生新配置（客户端 CA 池、服务端证书均取当时快照，
// POST /cert 热更换即时生效）。
//
// 两处关键细节：
//   - 派生配置必须复制 NextProtos：crypto/tls 对回调返回的配置不继承
//     外层任何字段，漏复制会让 ALPN 协商不出 h2（客户端提供 h2 时服务端
//     不应答，静默退回 http/1.1，连接仍可用故难以察觉）。NextProtos 在
//     此显式声明而非依赖 net/http 注入：注入改的是 ServeTLS 持有的克隆，
//     本闭包引用的原始对象不会被回填。
//   - 外层不再设 GetCertificate：GetConfigForClient 恒返回自带证书回调
//     的派生配置，外层回调在该路径永不执行（外层证书快照还可能在热
//     更换后滞后一代）。
func (s *Server) MTLSConfig() *tls.Config {
	cfg := &tls.Config{
		MinVersion: tls.VersionTLS12,
		NextProtos: []string{"h2", "http/1.1"},
	}
	cfg.GetConfigForClient = func(*tls.ClientHelloInfo) (*tls.Config, error) {
		m := s.material.Load()
		if m == nil {
			return nil, errors.New("mTLS material not configured")
		}
		inner := &tls.Config{
			MinVersion: tls.VersionTLS12,
			ClientAuth: tls.RequireAndVerifyClientCert,
			ClientCAs:  m.clientCAs,
			NextProtos: cfg.NextProtos,
		}
		cert := m.cert
		inner.GetCertificate = func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			return &cert, nil
		}
		return inner, nil
	}
	return cfg
}

// PinClientFingerprints 设置客户端证书指纹准许名单
func (s *Server) PinClientFingerprints(pins []string) error {
	if len(pins) == 0 {
		if m := s.material.Load(); m != nil {
			nm := *m
			nm.pins = nil
			s.material.Store(&nm)
		}
		return nil
	}
	norm, err := parsePins(pins)
	if err != nil {
		return err
	}
	m := s.material.Load()
	if m == nil {
		return errors.New("pin list requires mTLS (ca/cert/key) first")
	}
	nm := *m
	nm.pins = norm
	s.material.Store(&nm)
	return nil
}

// loadCertPair 加载并解析证书对
func loadCertPair(cert, key string) (tls.Certificate, *x509.Certificate, error) {
	pair, err := tls.LoadX509KeyPair(cert, key)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("failed to load server certificate pair: %w", err)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("failed to parse server certificate: %w", err)
	}
	pair.Leaf = leaf
	return pair, leaf, nil
}

// parsePins 解析指纹串列表为准许名单
func parsePins(pins []string) (map[string]struct{}, error) {
	m := make(map[string]struct{}, len(pins))
	for _, p := range pins {
		norm, err := ca.ParsePin(p)
		if err != nil {
			return nil, err
		}
		m[norm] = struct{}{}
	}
	return m, nil
}
