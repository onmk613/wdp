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

// MTLSConfig 返回接线到当前材料快照的 TLS 配置
func (s *Server) MTLSConfig() *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			m := s.material.Load()
			if m == nil {
				return nil, errors.New("mTLS material not configured")
			}
			return &m.cert, nil
		},
		GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) {
			m := s.material.Load()
			if m == nil {
				return nil, errors.New("mTLS material not configured")
			}
			cfg := &tls.Config{
				MinVersion: tls.VersionTLS12,
				ClientAuth: tls.RequireAndVerifyClientCert,
				ClientCAs:  m.clientCAs,
			}
			cert := m.cert
			cfg.GetCertificate = func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
				return &cert, nil
			}
			return cfg, nil
		},
	}
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
