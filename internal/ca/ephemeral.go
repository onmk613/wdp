// 会话级临时证书（内存态，不落盘）：push 通道自举用。
//
// 控制端每进程调用一次 IssueEphemeral，全部 push 主机共享同一信任链与同一对
// 服务端证书——不按主机生成（控制端按"只验证书链不验主机名"模式连接，
// 服务端证书 SAN 无需覆盖各主机地址）。客户端证书与私钥仅存在于控制端内存；
// CA 私钥在签发完两张叶子证书后即弃置，不随产物返回。
//
// 共享服务端证书对的取舍：被攻破的主机若再配合流量劫持可向控制端冒充其他
// 主机（无主机名校验）——push 临时会话场景的既定取舍，换取零按主机签发开销。

package ca

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"time"
)

// ephemeralValidity 临时证书有效期：push agent 生命周期为分钟级，
// 24h 仅作余量，同时把泄漏窗口压到会话级。
const ephemeralValidity = 24 * time.Hour

// EphemeralCerts 是一次 push 会话的临时 mTLS 材料（PEM，全程内存）。
// ServerCertPEM/ServerKeyPEM/CACertPEM 上传目标机；ClientCertPEM/ClientKeyPEM
// 仅控制端持有。
type EphemeralCerts struct {
	CACertPEM     []byte
	ServerCertPEM []byte
	ServerKeyPEM  []byte
	ClientCertPEM []byte
	ClientKeyPEM  []byte
}

// IssueEphemeral 生成会话级临时 CA 与服务端/客户端证书对（ECDSA P-256，内存态）。
// 服务端证书 SAN 固定为 wdp-push-server（控制端只验链不验名，SAN 不参与校验）。
func IssueEphemeral() (*EphemeralCerts, error) {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	notBefore, notAfter := time.Now().Add(-5*time.Minute), time.Now().Add(ephemeralValidity)
	caTpl := &x509.Certificate{
		SerialNumber:          randomSerial(),
		Subject:               pkix.Name{CommonName: "wdp-push-ca", Organization: []string{"wdp"}},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            0, // 禁止签发中间 CA
		MaxPathLenZero:        true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTpl, caTpl, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, err
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		return nil, err
	}

	serverDER, serverKeyDER, err := signEphemeralLeaf(caCert, caKey, "wdp-push-server",
		[]x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, notBefore, notAfter)
	if err != nil {
		return nil, err
	}
	clientDER, clientKeyDER, err := signEphemeralLeaf(caCert, caKey, "wdp-push-control",
		[]x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, notBefore, notAfter)
	if err != nil {
		return nil, err
	}

	out := &EphemeralCerts{
		CACertPEM:     pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		ServerCertPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: serverDER}),
		ClientCertPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: clientDER}),
	}
	if out.ServerKeyPEM, err = ecKeyPEM(serverKeyDER); err != nil {
		return nil, err
	}
	if out.ClientKeyPEM, err = ecKeyPEM(clientKeyDER); err != nil {
		return nil, err
	}
	return out, nil
}

// signEphemeralLeaf 用临时 CA 签发一张叶子证书，返回证书与私钥 DER。
// dnsName 非空时写入 DNS SAN（仅服务端证书使用）。
func signEphemeralLeaf(caCert *x509.Certificate, caKey *ecdsa.PrivateKey, cn string,
	ekus []x509.ExtKeyUsage, notBefore, notAfter time.Time) (certDER, keyDER []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	tpl := &x509.Certificate{
		SerialNumber: randomSerial(),
		Subject:      pkix.Name{CommonName: cn, Organization: []string{"wdp"}},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  ekus,
	}
	if ekus[0] == x509.ExtKeyUsageServerAuth {
		tpl.DNSNames = []string{cn}
	}
	certDER, err = x509.CreateCertificate(rand.Reader, tpl, caCert, &key.PublicKey, caKey)
	if err != nil {
		return nil, nil, err
	}
	if keyDER, err = x509.MarshalECPrivateKey(key); err != nil {
		return nil, nil, err
	}
	return certDER, keyDER, nil
}

// ecKeyPEM 将 SEC1 EC 私钥 DER 编码为 PEM。
func ecKeyPEM(der []byte) ([]byte, error) {
	if _, err := x509.ParseECPrivateKey(der); err != nil {
		return nil, fmt.Errorf("临时私钥编码异常: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: plainPEMType, Bytes: der}), nil
}
