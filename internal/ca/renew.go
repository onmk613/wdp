package ca

import (
	"crypto"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// RenewOptions 是续期参数（更新延期，非重签）。全显式、无命名约定：
// 旧证书与旧私钥都经 CertPath/KeyPath 指定；OutPath 是新证书输出路径
// （空 = 当前目录下沿用旧证书文件名；新私钥固定为同目录同名 .key）。
type RenewOptions struct {
	CertPath   string // 旧证书路径（必填）
	KeyPath    string // 旧私钥路径（保留私钥模式必填；--new-key 时可省）
	OutPath    string // 新证书输出路径（空 = ./<旧证书文件名>）
	CACertPath string // 重签用根 CA 证书（空 = <新证书目录>/ca.crt）
	CAKeyPath  string // 根 CA 私钥（空 = <新证书目录>/ca.key）
	NewKey     bool   // 换新私钥（算法沿用原证书；旧证书立即失效）
	Days       int    // 在原到期时刻上**增加**的天数（<=0 = 默认 30）
}

// Renew 更新延期已有证书：身份字段（CN/SAN 四类/EKU/密钥算法）全部
// 原样继承，仅把到期时刻从原值向后延 Days 天（仍钳制到签发 CA 到期）。
// 新旧产物同目录时，旧证书/私钥先改名为 <路径>.old.<时间戳> 备份再写新件；
// 输出到别的目录则旧件原地不动。保留私钥模式会校验钥匙与旧证书公钥
// 配对，不配对直接拒绝（防拿错钥匙静默换身份）。
// 返回 (newCert, newKey, 指纹)。
func Renew(o RenewOptions) (string, string, string, error) {
	if o.CertPath == "" {
		return "", "", "", errors.New("renew requires --cert (the certificate to renew)")
	}
	old, err := parseCertificate(o.CertPath)
	if err != nil {
		return "", "", "", fmt.Errorf("failed to read original certificate (%s): %w", o.CertPath, err)
	}

	days := o.Days
	if days <= 0 {
		days = DefaultDays
	}
	notAfter := old.NotAfter.AddDate(0, 0, days)
	if !notAfter.After(time.Now()) {
		return "", "", "", fmt.Errorf(
			"renewing by %d day(s) from %s leaves the certificate expired; increase --days",
			days, old.NotAfter.Format(time.RFC3339))
	}

	tpl, err := leafTemplate("", nil, ProfileServer, 0) // 身份字段全部从旧证书继承
	if err != nil {
		return "", "", "", err
	}
	tpl.Subject = old.Subject
	tpl.DNSNames = old.DNSNames
	tpl.IPAddresses = old.IPAddresses
	tpl.URIs = old.URIs
	tpl.EmailAddresses = old.EmailAddresses
	tpl.ExtKeyUsage = old.ExtKeyUsage
	tpl.NotBefore = time.Now().Add(-time.Hour)
	tpl.NotAfter = notAfter

	var key crypto.Signer
	if o.NewKey {
		// 换钥沿用原密钥的算法与规格（ECDSA 取曲线位数，RSA 取模长）
		if key, err = keySpecOf(old.PublicKey).Generate(); err != nil {
			return "", "", "", err
		}
	} else {
		if o.KeyPath == "" {
			return "", "", "", errors.New("renew requires --key (or --new-key to rotate)")
		}
		keyDER, err := readKey(o.KeyPath)
		if err != nil {
			return "", "", "", fmt.Errorf("failed to read original private key: %w", err)
		}
		parsed, err := parsePrivateKey(keyDER)
		if err != nil {
			return "", "", "", fmt.Errorf("failed to parse original private key: %w", err)
		}
		if !pubEqual(old.PublicKey, parsed.Public()) {
			return "", "", "", fmt.Errorf(
				"private key %s does not match the certificate's public key (%s)", o.KeyPath, o.CertPath)
		}
		key = parsed
	}

	outPath := o.OutPath
	if outPath == "" {
		outPath = filepath.Join(".", filepath.Base(o.CertPath))
	}

	// 新旧同目录才备份（新件会覆盖旧件）；输出到别处则旧件原地不动
	var renamed []string
	if filepath.Dir(outPath) == filepath.Dir(o.CertPath) {
		ts := time.Now().Format("20060102-150405")
		for _, p := range []string{o.CertPath, o.KeyPath} {
			if p != "" {
				if _, err := os.Stat(p); err == nil {
					_ = os.Rename(p, p+".old."+ts)
					renamed = append(renamed, p, p+".old."+ts)
				}
			}
		}
	}
	newCrt, newKey, newFP, err := signAndWrite(filepath.Dir(outPath), o.CACertPath, o.CAKeyPath,
		strings.TrimSuffix(filepath.Base(outPath), ".crt"), tpl, key)
	if err != nil {
		// 签发失败时恢复此前的改名备份：旧证书/私钥不能因重签失败而丢失
		for i := len(renamed) - 2; i >= 0; i -= 2 {
			_ = os.Rename(renamed[i+1], renamed[i])
		}
		return "", "", "", err
	}
	return newCrt, newKey, newFP, nil
}
