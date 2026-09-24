package ca

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"os"
	"strings"
)

// FingerprintDER 返回证书 DER 的 SHA256 指纹（sha256:hex，供 --pin-client-fp）。
func FingerprintDER(der []byte) string {
	h := sha256.Sum256(der)
	return "sha256:" + hex.EncodeToString(h[:])
}

// FingerprintFile 返回证书文件的 SHA256 指纹。
func FingerprintFile(path string) (string, error) {
	pemBytes, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	block, _ := pem.Decode(pemBytes)
	if block == nil || block.Type != "CERTIFICATE" {
		return "", fmt.Errorf("%s is not a certificate PEM", path)
	}
	return FingerprintDER(block.Bytes), nil
}

// FingerprintPublicKey 返回证书公钥（SPKI）的 SHA256 指纹。
//
// 与证书 DER 指纹的区别：证书续期（保留密钥对）后 DER 变了、SPKI 不变。
// agent 的客户端证书准许名单因此可以同时收录两种形式——用 SPKI 写的
// pin 能跨续期存活，不会因为控制端例行续证把整片 agent 打成不可达。
func FingerprintPublicKey(der []byte) (string, error) {
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return "", fmt.Errorf("parse certificate: %w", err)
	}
	h := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return "sha256:" + hex.EncodeToString(h[:]), nil
}

// FingerprintPublicKeyFile 返回证书文件公钥（SPKI）的 SHA256 指纹。
func FingerprintPublicKeyFile(path string) (string, error) {
	pemBytes, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	block, _ := pem.Decode(pemBytes)
	if block == nil || block.Type != "CERTIFICATE" {
		return "", fmt.Errorf("%s is not a certificate PEM", path)
	}
	return FingerprintPublicKey(block.Bytes)
}

// ParsePin 解析指纹串（sha256:hex / hex / 含冒号 hex）为小写无分隔 hex。
func ParsePin(s string) (string, error) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "sha256:")
	s = strings.ReplaceAll(s, ":", "")
	s = strings.ToLower(s)
	raw, err := hex.DecodeString(s)
	if err != nil || len(raw) != 32 {
		return "", fmt.Errorf("invalid fingerprint %q (SHA256 required)", s)
	}
	return s, nil
}
