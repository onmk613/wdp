package ca

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/x509"
	"fmt"
	"net"
	"strings"
	"time"
)

// 证书信息解析（ca show 的域逻辑）：解析为结构化信息，展示由命令层渲染。

// CertInfo 是证书的可读信息。
type CertInfo struct {
	Path        string
	Subject     string // 主题（RFC2253 概要：CN=...,O=...）
	Issuer      string // 签发者
	SelfSigned  bool   // 自签
	Serial      string // 序列号（hex）
	NotBefore   time.Time
	NotAfter    time.Time
	IsCA        bool
	PathLen     int  // 可签发的下级 CA 深度（-1 = 不限）
	PathLenZero bool // PathLen=0：只能签叶子，不能签中间 CA
	KeyUsage    []string
	ExtKeyUsage []string
	DNSNames    []string // 服务端证书的 DNS SAN
	IPs         []string // 服务端证书的 IP SAN
	PublicKey   string   // 公钥算法与参数（如 ECDSA P-256、RSA 2048 bit）
	Fingerprint string   // SHA256 指纹（sha256:hex，供 --pin-client-fp）
}

// Inspect 解析证书文件为可读信息。
func Inspect(path string) (*CertInfo, error) {
	cert, err := parseCertificate(path)
	if err != nil {
		return nil, err
	}
	return &CertInfo{
		Path:        path,
		Subject:     cert.Subject.String(),
		Issuer:      cert.Issuer.String(),
		SelfSigned:  cert.CheckSignatureFrom(cert) == nil,
		Serial:      fmt.Sprintf("%x", cert.SerialNumber),
		NotBefore:   cert.NotBefore,
		NotAfter:    cert.NotAfter,
		IsCA:        cert.IsCA,
		PathLen:     cert.MaxPathLen,
		PathLenZero: cert.MaxPathLenZero,
		KeyUsage:    keyUsageNames(cert.KeyUsage),
		ExtKeyUsage: extKeyUsageNames(cert.ExtKeyUsage),
		DNSNames:    cert.DNSNames,
		IPs:         ipStrings(cert.IPAddresses),
		PublicKey:   pubKeyName(cert.PublicKey),
		Fingerprint: FingerprintDER(cert.Raw),
	}, nil
}

// keyUsageNames 按固定顺序输出置位的密钥用途名。
func keyUsageNames(ku x509.KeyUsage) []string {
	bits := []struct {
		bit  x509.KeyUsage
		name string
	}{
		{x509.KeyUsageDigitalSignature, "DigitalSignature"},
		{x509.KeyUsageContentCommitment, "ContentCommitment"},
		{x509.KeyUsageKeyEncipherment, "KeyEncipherment"},
		{x509.KeyUsageDataEncipherment, "DataEncipherment"},
		{x509.KeyUsageKeyAgreement, "KeyAgreement"},
		{x509.KeyUsageCertSign, "CertSign"},
		{x509.KeyUsageCRLSign, "CRLSign"},
		{x509.KeyUsageEncipherOnly, "EncipherOnly"},
		{x509.KeyUsageDecipherOnly, "DecipherOnly"},
	}
	var out []string
	for _, b := range bits {
		if ku&b.bit != 0 {
			out = append(out, b.name)
		}
	}
	return out
}

var extKeyUsageName = map[x509.ExtKeyUsage]string{
	x509.ExtKeyUsageAny:             "Any",
	x509.ExtKeyUsageServerAuth:      "ServerAuth",
	x509.ExtKeyUsageClientAuth:      "ClientAuth",
	x509.ExtKeyUsageCodeSigning:     "CodeSigning",
	x509.ExtKeyUsageEmailProtection: "EmailProtection",
	x509.ExtKeyUsageTimeStamping:    "TimeStamping",
	x509.ExtKeyUsageOCSPSigning:     "OCSPSigning",
}

func extKeyUsageNames(ekus []x509.ExtKeyUsage) []string {
	out := make([]string, 0, len(ekus))
	for _, e := range ekus {
		if n, ok := extKeyUsageName[e]; ok {
			out = append(out, n)
		} else {
			out = append(out, fmt.Sprintf("ExtKeyUsage(%d)", e))
		}
	}
	return out
}

func ipStrings(ips []net.IP) []string {
	out := make([]string, 0, len(ips))
	for _, ip := range ips {
		out = append(out, ip.String())
	}
	return out
}

// pubKeyName 公钥算法与参数。
func pubKeyName(pub any) string {
	switch p := pub.(type) {
	case *ecdsa.PublicKey:
		return "ECDSA " + p.Curve.Params().Name
	case *rsa.PublicKey:
		return fmt.Sprintf("RSA %d bit", p.N.BitLen())
	case ed25519.PublicKey:
		return "Ed25519"
	default:
		return strings.TrimSpace(fmt.Sprintf("%T", pub))
	}
}
