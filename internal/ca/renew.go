package ca

import (
	"crypto"
	"crypto/x509"
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

// restoreRenamed 逆序恢复已改名的备份（备份/签发失败路径专用）。
// renamed 为 (原路径, 备份路径) 平铺序列；全部成功返回 nil，部分失败
// 时聚合返回（备份名下的旧件仍可手工找回）。
func restoreRenamed(renamed []string) error {
	var errs []error
	for i := len(renamed) - 2; i >= 0; i -= 2 {
		if err := os.Rename(renamed[i+1], renamed[i]); err != nil {
			errs = append(errs, fmt.Errorf("failed to restore %s from %s: %w", renamed[i], renamed[i+1], err))
		}
	}
	return errors.Join(errs...)
}

// Renew 更新延期已有证书：身份字段（CN/SAN 四类/EKU/密钥算法）全部
// 原样继承，仅把到期时刻从原值向后延 Days 天（仍钳制到签发 CA 到期）。
// 新旧产物同目录时，旧证书/私钥先改名为 <路径>.old.<时间戳> 备份再写新件；
// 输出到别的目录则旧件原地不动。保留私钥模式会校验钥匙与旧证书公钥
// 配对，不配对直接拒绝（防拿错钥匙静默换身份）。
// 返回 (newCert, newKey, 指纹)。
func Renew(o RenewOptions) (string, string, string, error) {
	old, err := loadRenewTarget(o)
	if err != nil {
		return "", "", "", err
	}
	tpl, err := renewTemplate(old, o.Days)
	if err != nil {
		return "", "", "", err
	}
	key, err := renewKey(o, old)
	if err != nil {
		return "", "", "", err
	}

	outPath := o.OutPath
	if outPath == "" {
		outPath = filepath.Join(".", filepath.Base(o.CertPath))
	}

	renamed, err := backupRenewOutputs(outPath, o)
	if err != nil {
		return "", "", "", err
	}
	newCrt, newKey, newFP, err := signAndWrite(filepath.Dir(outPath), o.CACertPath, o.CAKeyPath,
		strings.TrimSuffix(filepath.Base(outPath), ".crt"), tpl, key)
	if err != nil {
		// 签发失败时恢复此前的改名备份：旧证书/私钥不能因重签失败而丢失。
		// 恢复失败同样上报（join 而非覆盖）：静默丢弃会让旧件只存在于
		// .old.<时间戳> 备份名下，调用方无从得知
		return "", "", "", errors.Join(err, restoreRenamed(renamed))
	}
	newCrt, newKey, err = placeRenewOutputs(outPath, newCrt, newKey)
	if err != nil {
		return "", "", "", err
	}
	return newCrt, newKey, newFP, nil
}

// loadRenewTarget 读取并校验待续期的旧证书。
func loadRenewTarget(o RenewOptions) (*x509.Certificate, error) {
	if o.CertPath == "" {
		return nil, errors.New("renew requires --cert (the certificate to renew)")
	}
	old, err := parseCertificate(o.CertPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read original certificate (%s): %w", o.CertPath, err)
	}
	// renew 走的是叶子模板（固定 KeyUsage、无 BasicConstraints），对根 CA
	// 续期会把信任根降级成普通叶子证书并覆盖原文件——必须拒绝并指引
	// ca init 重建。
	if old.IsCA {
		return nil, fmt.Errorf(
			"%s is a CA certificate; renew would re-issue it as a non-CA leaf (run `ca init` to rebuild the CA instead)", o.CertPath)
	}
	return old, nil
}

// renewTemplate 构造续期模板：身份字段全部从旧证书继承，到期时刻在原值
// 上延 days 天（<=0 默认 DefaultDays）；延期后仍已过期则报错。
func renewTemplate(old *x509.Certificate, days int) (*x509.Certificate, error) {
	if days <= 0 {
		days = DefaultDays
	}
	notAfter := old.NotAfter.AddDate(0, 0, days)
	if !notAfter.After(time.Now()) {
		return nil, fmt.Errorf(
			"renewing by %d day(s) from %s leaves the certificate expired; increase --days",
			days, old.NotAfter.Format(time.RFC3339))
	}

	tpl, err := leafTemplate("", nil, ProfileServer, 0) // 身份字段全部从旧证书继承
	if err != nil {
		return nil, err
	}
	tpl.Subject = old.Subject
	tpl.DNSNames = old.DNSNames
	tpl.IPAddresses = old.IPAddresses
	tpl.URIs = old.URIs
	tpl.EmailAddresses = old.EmailAddresses
	tpl.ExtKeyUsage = old.ExtKeyUsage
	tpl.NotBefore = time.Now().Add(-time.Hour)
	tpl.NotAfter = notAfter
	return tpl, nil
}

// renewKey 解析续期用私钥：--new-key 沿用原密钥算法与规格现生成；保留
// 私钥模式读取旧钥并校验与旧证书公钥配对（防拿错钥匙静默换身份）。
func renewKey(o RenewOptions, old *x509.Certificate) (crypto.Signer, error) {
	if o.NewKey {
		// 换钥沿用原密钥的算法与规格（ECDSA 取曲线位数，RSA 取模长）
		return keySpecOf(old.PublicKey).Generate()
	}
	if o.KeyPath == "" {
		return nil, errors.New("renew requires --key (or --new-key to rotate)")
	}
	keyDER, err := readKey(o.KeyPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read original private key: %w", err)
	}
	parsed, err := parsePrivateKey(keyDER)
	if err != nil {
		return nil, fmt.Errorf("failed to parse original private key: %w", err)
	}
	if !pubEqual(old.PublicKey, parsed.Public()) {
		return nil, fmt.Errorf(
			"private key %s does not match the certificate's public key (%s)", o.KeyPath, o.CertPath)
	}
	return parsed, nil
}

// backupRenewOutputs 新旧产物同目录时把旧证书/私钥改名为 .old.<时间戳>
// 备份（返回改名序列，签发失败时逆序恢复）；输出到别处则旧件原地不动。
func backupRenewOutputs(outPath string, o RenewOptions) (renamed []string, err error) {
	// 目录比较须先 Abs（含 Clean）："./ca.crt" 与 "ca.crt/../ca.crt"、
	// 相对与绝对混写等非规范形式字符串不等却指向同一目录——不规范化
	// 会误判为"不同目录"而跳过备份，新件直接覆盖旧件。
	sameDir := func(a, b string) bool {
		absA, errA := filepath.Abs(a)
		absB, errB := filepath.Abs(b)
		if errA != nil || errB != nil {
			// Abs 依赖 cwd，极小概率失败（目录被删等）：退化为 Clean 比较
			return filepath.Clean(a) == filepath.Clean(b)
		}
		return absA == absB
	}
	if !sameDir(filepath.Dir(outPath), filepath.Dir(o.CertPath)) {
		return nil, nil
	}
	ts := time.Now().Format("20060102-150405")
	for _, p := range []string{o.CertPath, o.KeyPath} {
		if p == "" {
			continue
		}
		if _, err := os.Stat(p); err != nil {
			continue
		}
		// 备份改名失败必须中止签发：注释承诺"旧件先备份再覆盖"，忽略
		// 失败继续写新件会把旧证书/私钥直接覆盖丢失；已改名的前序备份
		// 回滚还原，失败原因一并上报
		if err := os.Rename(p, p+".old."+ts); err != nil {
			return nil, errors.Join(
				fmt.Errorf("failed to back up %s: %w", p, err),
				restoreRenamed(renamed))
		}
		renamed = append(renamed, p, p+".old."+ts)
	}
	return renamed, nil
}

// placeRenewOutputs 对齐签发产物到请求的精确输出路径：signAndWrite 固定
// 落盘 <name>.crt|.key，OutPath 无 .crt 后缀时产物路径会漂移（--out
// /x/newcert 实得 /x/newcert.crt）——rename 到请求的精确路径；新私钥维持
// "同目录同名 .key"约定。
func placeRenewOutputs(outPath, newCrt, newKey string) (string, string, error) {
	wantKey := strings.TrimSuffix(outPath, ".crt") + ".key"
	if newCrt != outPath {
		if err := os.Rename(newCrt, outPath); err != nil {
			return "", "", err
		}
		newCrt = outPath
	}
	if newKey != wantKey {
		if err := os.Rename(newKey, wantKey); err != nil {
			return "", "", err
		}
		newKey = wantKey
	}
	return newCrt, newKey, nil
}
