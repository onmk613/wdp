package ca

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"testing"
	"time"
)

func TestIssueEphemeral(t *testing.T) {
	certs, err := IssueEphemeral()
	if err != nil {
		t.Fatal(err)
	}

	// CA 可解析、是 CA、PathLen=0、短有效期
	caCert := parsePEMCert(t, certs.CACertPEM, "CACertPEM")
	if !caCert.IsCA {
		t.Fatal("临时 CA 缺少 BasicConstraints CA=true")
	}
	if !caCert.MaxPathLenZero || caCert.MaxPathLen != 0 {
		t.Fatal("临时 CA 未限制 PathLen=0")
	}
	if caCert.NotAfter.Sub(caCert.NotBefore) > 25*time.Hour {
		t.Fatalf("临时 CA 有效期过长: %v", caCert.NotAfter.Sub(caCert.NotBefore))
	}

	// 服务端/客户端证书链校验通过、EKU 正确
	pool := x509.NewCertPool()
	pool.AddCert(caCert)
	server := parsePEMCert(t, certs.ServerCertPEM, "ServerCertPEM")
	if _, err := server.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
		t.Fatalf("服务端证书链校验失败: %v", err)
	}
	client := parsePEMCert(t, certs.ClientCertPEM, "ClientCertPEM")
	if _, err := client.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		t.Fatalf("客户端证书链校验失败: %v", err)
	}
	if _, err := client.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err == nil {
		t.Fatal("客户端证书不应携带 serverAuth EKU")
	}

	// 证书与私钥可配对（tls.X509KeyPair 同时校验公私钥匹配）
	if _, err := tls.X509KeyPair(certs.ServerCertPEM, certs.ServerKeyPEM); err != nil {
		t.Fatalf("服务端证书对不匹配: %v", err)
	}
	if _, err := tls.X509KeyPair(certs.ClientCertPEM, certs.ClientKeyPEM); err != nil {
		t.Fatalf("客户端证书对不匹配: %v", err)
	}
}

// TestIssueEphemeralUnique 两次签发产物互不相干（每进程独立信任链）。
func TestIssueEphemeralUnique(t *testing.T) {
	a, err := IssueEphemeral()
	if err != nil {
		t.Fatal(err)
	}
	b, err := IssueEphemeral()
	if err != nil {
		t.Fatal(err)
	}
	poolA := x509.NewCertPool()
	poolA.AddCert(parsePEMCert(t, a.CACertPEM, "a"))
	if _, err := parsePEMCert(t, b.ServerCertPEM, "b").Verify(
		x509.VerifyOptions{Roots: poolA, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err == nil {
		t.Fatal("不同会话的证书不应形成信任链")
	}
}

func parsePEMCert(t *testing.T, pemBytes []byte, what string) *x509.Certificate {
	t.Helper()
	block, _ := pem.Decode(pemBytes)
	if block == nil || block.Type != "CERTIFICATE" {
		t.Fatalf("%s 不是证书 PEM", what)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("解析 %s 失败: %v", what, err)
	}
	return cert
}
