package ca

// CSR 签发链路：纳管私钥由目标机本地生成（docs/20），server 只见过
// 公钥——叶子私钥不再落 server 磁盘、不再经 URL 交付。

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"

	"wdp/internal/fsatomic"
)

// csrBodyLimit CSR PEM 的大小上限：ed25519/ECDSA P-256 的 DER 不足 1KiB，
// 16KiB 已是数量级冗余（RSA 大模也远够——但 RSA 本就不在白名单）。
const csrBodyLimit = 16 << 10

// ParseCSR 校验 PEM CSR：解码、解析、自签名校验、公钥算法白名单。
// 通过即证明提交方持有对应私钥（防拿别人的公钥材料冒名）。
// 注意：CSR 自带的 SAN/CN 是否采信由调用方决定——纳管签发一律忽略，
// 身份以 server 侧 claim 记录为准。
func ParseCSR(pemBytes []byte) (*x509.CertificateRequest, error) {
	if len(pemBytes) > csrBodyLimit {
		return nil, fmt.Errorf("CSR body exceeds %d bytes", csrBodyLimit)
	}
	blk, _ := pem.Decode(pemBytes)
	if blk == nil || blk.Type != "CERTIFICATE REQUEST" {
		return nil, fmt.Errorf("not a PEM CERTIFICATE REQUEST")
	}
	csr, err := x509.ParseCertificateRequest(blk.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse CSR: %w", err)
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, fmt.Errorf("CSR signature check failed (submitter does not hold the key): %w", err)
	}
	switch pub := csr.PublicKey.(type) {
	case ed25519.PublicKey:
		if len(pub) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("malformed ed25519 public key in CSR")
		}
	case *ecdsa.PublicKey:
		if pub.Curve != elliptic.P256() && pub.Curve != elliptic.P384() {
			return nil, fmt.Errorf("ECDSA curve %v not allowed (want P-256/P-384)", pub.Curve.Params().Name)
		}
	default:
		return nil, fmt.Errorf("CSR public key algorithm %T not allowed (want ed25519 or ECDSA P-256/P-384)", csr.PublicKey)
	}
	return csr, nil
}

// PubkeySHA 返回公钥的 sha256（SPKI DER 形态）。CSR 与证书同用此口径：
// 「既有证书 == 本次 CSR 同钥」的幂等判定靠字节级可比。
func PubkeySHA(pub crypto.PublicKey) (string, error) {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:]), nil
}

// CertPubkeySHA 证书文件公钥的 sha256（与 PubkeySHA 同口径——服务端
// 幂等判定"既有证书 == 本次 CSR 同钥"用）。
func CertPubkeySHA(certPath string) (string, error) {
	cert, err := parseCertificate(certPath)
	if err != nil {
		return "", err
	}
	return PubkeySHA(cert.PublicKey)
}

// SignCSROptions 是 CSR 签发参数（与 IssueOptions 同构，但没有 Key——
// 公钥来自 CSR，签发方无私钥可配）。
type SignCSROptions struct {
	Dir        string   // 证书输出目录
	CACertPath string   // CA 证书路径（空 = <Dir>/ca.crt）
	CAKeyPath  string   // CA 私钥路径（空 = <Dir>/ca.key）
	SANs       []string // 身份完全由调用方给出（claim 记录），CSR 自报一律忽略
	Profile    Profile
	Days       int
	Subject    Subject
}

// SignCSR 用**已验证** CSR（ParseCSR 产物）的公钥签发证书。只写 .crt：
// 签发方不持有也不产生叶子私钥。返回 (证书路径, 指纹)。
func SignCSR(o SignCSROptions, csr *x509.CertificateRequest, name string) (string, string, error) {
	profile, err := NormalizeProfile(string(o.Profile))
	if err != nil {
		return "", "", err
	}
	if name == "" {
		name = string(profile)
	}
	tpl, err := leafTemplate(name, o.SANs, profile, o.Days)
	if err != nil {
		return "", "", err
	}
	cn := name
	if o.Subject.CN != "" {
		cn = o.Subject.CN
	}
	tpl.Subject = o.Subject.fillDefaults().name(cn)
	certPath, _, fp, err := signCert(o.Dir, o.CACertPath, o.CAKeyPath, name, tpl, csr.PublicKey)
	return certPath, fp, err
}

// GenCSR 在**本机**生成（或复用已有）私钥并产出 CSR——目标机侧的
// `wdp agent gencsr`。已存在可解析的私钥时幂等复用（重装不换身份）；
// 存在但解析失败时显式报错（静默覆盖可能毁掉 agent 正在使用的钥匙）。
// CSR 不带 Subject：身份由 server 侧 claim 记录决定，自报无意义。
// 返回是否复用了既有私钥。
func GenCSR(keyPath, csrPath string) (bool, error) {
	var key crypto.Signer
	reused := false
	if _, statErr := os.Stat(keyPath); statErr == nil {
		keyDER, derr := readKey(keyPath)
		if derr != nil {
			return false, fmt.Errorf("existing key %s is unreadable (%v); move it away or choose another --key path", keyPath, derr)
		}
		parsed, perr := parsePrivateKey(keyDER)
		if perr != nil {
			return false, fmt.Errorf("existing key %s is unreadable (%v); move it away or choose another --key path", keyPath, perr)
		}
		key, reused = parsed, true
	} else if !os.IsNotExist(statErr) {
		return false, statErr
	} else {
		var gerr error
		if key, gerr = (KeySpec{}).Generate(); gerr != nil {
			return false, gerr
		}
		der, pemType, merr := marshalKey(key)
		if merr != nil {
			return false, merr
		}
		if err := os.MkdirAll(filepath.Dir(keyPath), 0o755); err != nil {
			return false, err
		}
		if err := fsatomic.WriteFile(keyPath, bytes.NewReader(pem.EncodeToMemory(&pem.Block{Type: pemType, Bytes: der})), 0o600); err != nil {
			return false, err
		}
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{PublicKey: key.Public()}, key)
	if err != nil {
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(csrPath), 0o755); err != nil {
		return false, err
	}
	return reused, fsatomic.WriteFile(csrPath, bytes.NewReader(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER})), 0o644)
}
