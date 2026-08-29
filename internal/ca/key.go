package ca

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"strings"
)

// KeySpec 是密钥规格：算法 + 参数
type KeySpec struct {
	Algo string // "ecdsa"（默认）| "rsa" | "ed25519"
	Size int    // rsa: 2048/3072/4096（默认 2048）；ecdsa: 256/384/521（默认 256）；ed25519 忽略
}

// Generate 生成私钥。Algo 空 = 默认 ed25519（现代默认：快、密钥小、
// 无参数选择负担；需要传统互操作时显式选 ecdsa/rsa）。
func (k KeySpec) Generate() (crypto.Signer, error) {
	switch strings.ToLower(strings.TrimSpace(k.Algo)) {
	case "", "ed25519":
		_, priv, err := ed25519.GenerateKey(rand.Reader)
		return priv, err
	case "ecdsa":
		var curve elliptic.Curve
		switch k.Size {
		case 0, 256:
			curve = elliptic.P256()
		case 384:
			curve = elliptic.P384()
		case 521:
			curve = elliptic.P521()
		default:
			return nil, fmt.Errorf("unsupported ECDSA size %d (256|384|521)", k.Size)
		}
		return ecdsa.GenerateKey(curve, rand.Reader)
	case "rsa":
		size := k.Size
		if size == 0 {
			size = 2048
		}
		if size < 2048 || size%256 != 0 {
			return nil, fmt.Errorf("unsupported RSA size %d (2048/3072/4096)", size)
		}
		return rsa.GenerateKey(rand.Reader, size)
	default:
		return nil, fmt.Errorf("unknown key algorithm %q (ecdsa|rsa|ed25519)", k.Algo)
	}
}

// keySpecOf 从公钥反推密钥规格（renew --new-key 沿用原算法）。
func keySpecOf(pub crypto.PublicKey) KeySpec {
	switch p := pub.(type) {
	case *ecdsa.PublicKey:
		return KeySpec{Algo: "ecdsa", Size: p.Curve.Params().BitSize}
	case *rsa.PublicKey:
		return KeySpec{Algo: "rsa", Size: p.N.BitLen()}
	case ed25519.PublicKey:
		return KeySpec{Algo: "ed25519"}
	}
	return KeySpec{}
}

// readKey 读取明文私钥：PKCS8（本工具产物）与 SEC1 EC / PKCS1 RSA
// （openssl 等外部产物的输入互操作）。openssl 产物常在私钥块前附带
// EC PARAMETERS 等非私钥块（ecparam -genkey 的输出格式），逐块扫描
// 直到遇到私钥类型；口令加密的 PKCS8 明确报不支持。
func readKey(path string) ([]byte, error) {
	keyPEM, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read private key: %w", err)
	}
	rest := keyPEM
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			return nil, errors.New("failed to parse private key PEM")
		}
		switch block.Type {
		case "ENCRYPTED PRIVATE KEY":
			return nil, errors.New("passphrase-encrypted PKCS8 keys are not supported; re-export in plaintext (openssl pkey -in ca.key)")
		case "EC PRIVATE KEY", "PRIVATE KEY", "RSA PRIVATE KEY":
			return block.Bytes, nil
		}
		// 其它块（EC PARAMETERS 等）跳过继续找私钥
	}
}

// parsePrivateKey 解析私钥 DER（SEC1 EC / PKCS8 / PKCS1 RSA），须实现
// crypto.Signer（自制根 CA 可能是 RSA/Ed25519 等任意算法）。
func parsePrivateKey(der []byte) (crypto.Signer, error) {
	if key, err := x509.ParseECPrivateKey(der); err == nil {
		return key, nil
	}
	if key, err := x509.ParsePKCS1PrivateKey(der); err == nil {
		return key, nil
	}
	key, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return nil, fmt.Errorf("failed to parse private key (SEC1 EC / PKCS8 / PKCS1 RSA supported): %w", err)
	}
	signer, ok := key.(crypto.Signer)
	if !ok {
		return nil, fmt.Errorf("private key type %T does not support signing", key)
	}
	return signer, nil
}

// writeKey 落盘私钥（明文 0600，统一 PKCS8——一种格式覆盖全部算法）。
func writeKey(path string, key crypto.Signer) error {
	der, pemType, err := marshalKey(key)
	if err != nil {
		return err
	}
	return writePEM(path, pemType, der, 0o600)
}

// marshalKey 编码私钥为 PKCS8 DER + PEM。
func marshalKey(key crypto.Signer) ([]byte, string, error) {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	return der, "PRIVATE KEY", err
}

// VerifyKeyPair 校验证书与私钥是否为配对的公私钥对（show --key 用）：
// 读取证书公钥与私钥（PKCS8 / SEC1 EC / PKCS1 RSA 均可），比对是否同钥。
// 配对返回 nil；不配对或读取失败返回错误（命令行非零退出，可脚本化）。
func VerifyKeyPair(certPath, keyPath string) error {
	cert, err := parseCertificate(certPath)
	if err != nil {
		return err
	}
	der, err := readKey(keyPath)
	if err != nil {
		return fmt.Errorf("failed to read private key: %w", err)
	}
	key, err := parsePrivateKey(der)
	if err != nil {
		return err
	}
	if !pubEqual(cert.PublicKey, key.Public()) {
		return fmt.Errorf("certificate and private key do NOT match (%s / %s)", certPath, keyPath)
	}
	return nil
}

// pubEqual 比较两个公钥（标准库密钥类型均实现 Equal）。
func pubEqual(a, b crypto.PublicKey) bool {
	type equaler interface{ Equal(crypto.PublicKey) bool }
	ae, ok := a.(equaler)
	return ok && ae.Equal(b)
}
