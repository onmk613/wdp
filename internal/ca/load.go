package ca

import (
	"crypto"
	"crypto/x509"
	"fmt"
	"time"
)

// LoadCAAt 按显式路径读取根 CA（签发者）证书与私钥：即 issue/renew 的
// --ca-cert/--ca-key，用它签出新证书；私钥明文 SEC1 EC / PKCS8
// （EC/RSA/Ed25519 等实现 crypto.Signer 的类型，兼容 openssl 等自制根 CA）。
func LoadCAAt(certPath, keyPath string) (*x509.Certificate, crypto.Signer, error) {
	cert, err := parseCertificate(certPath)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to read CA certificate: %w", err)
	}
	if !cert.IsCA {
		return nil, nil, fmt.Errorf("%s is a leaf certificate, not a root CA (--ca-cert must point to the signing CA)", certPath)
	}
	if time.Now().After(cert.NotAfter) {
		return nil, nil, fmt.Errorf("root CA certificate has expired (%s), cannot sign", cert.NotAfter.Format(time.RFC3339))
	}
	der, err := readKey(keyPath)
	if err != nil {
		return nil, nil, err
	}
	key, err := parsePrivateKey(der)
	if err != nil {
		return nil, nil, err
	}
	// 证书/私钥错配校验：签发用 key、颁发者用 cert，标准库不会报错——
	// 产出链不上去的废证书，问题拖到 mTLS 握手才暴露
	if !pubEqual(cert.PublicKey, key.Public()) {
		return nil, nil, fmt.Errorf("CA certificate and private key do not match (%s / %s)", certPath, keyPath)
	}
	return cert, key, nil
}
