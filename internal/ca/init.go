package ca

import (
	"crypto/rand"
	"crypto/x509"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// InitOptions 是 Init 的参数。
type InitOptions struct {
	Dir     string  // CA 目录
	Name    string  // CA 文件名称
	Days    int     // 有效期天数（<=0 = DefaultCADays，即 1 天）
	Subject Subject // 主题（CN 缺省 wdp-ca，O 缺省 wdp）
	Key     KeySpec // CA 私钥规格（默认 ed25519）
	PathLen int     // 可签发的下级 CA 深度：0 = 默认（只签叶子，禁止中间 CA）；>0 = 允许 N 级中间 CA；-1 = 不限
}

// Init 在 dir 生成自签根 CA（PathLen 默认 0：只签叶子、禁止中间 CA；
// o.PathLen>0 放开对应深度的中间 CA，-1 不限——供需要中间 CA 的企业
// PKI 场景）。私钥明文存储（0600）。返回 (caPath, keyPath, 证书指纹)。
func Init(o InitOptions) (string, string, string, error) {
	days := o.Days
	if days <= 0 {
		days = DefaultCADays
	}

	caFile, keyFile := DefaultCAFile, DefaultKeyFile
	if o.Name != "" {
		caFile = o.Name + ".crt"
		keyFile = o.Name + ".key"
	}

	if err := os.MkdirAll(o.Dir, 0o700); err != nil {
		return "", "", "", err
	}
	caPath, keyPath := filepath.Join(o.Dir, caFile), filepath.Join(o.Dir, keyFile)
	if _, err := os.Stat(caPath); err == nil {
		return "", "", "", fmt.Errorf("%s already exists (delete it first to rebuild)", caPath)
	}

	caKey, err := o.Key.Generate()
	if err != nil {
		return "", "", "", err
	}
	subject := o.Subject.fillDefaults()
	if subject.CN == "" {
		subject.CN = "wdp-ca"
	}
	tpl := &x509.Certificate{
		SerialNumber:          randomSerial(),
		Subject:               subject.name(subject.CN),
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(0, 0, days),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            o.PathLen,
		MaxPathLenZero:        o.PathLen == 0, // 默认 0：只签叶子，禁止中间 CA
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, caKey.Public(), caKey)
	if err != nil {
		return "", "", "", err
	}
	if err := writePEM(caPath, "CERTIFICATE", der, 0o644); err != nil {
		return "", "", "", err
	}
	if err := writeKey(keyPath, caKey); err != nil {
		return "", "", "", err
	}
	return caPath, keyPath, FingerprintDER(der), nil
}
