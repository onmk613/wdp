package ca

import (
	"crypto/sha256"
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
