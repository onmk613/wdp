package ca

import (
	"crypto"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// leafTemplate 构造叶子证书模板。SAN 全部来自 sans（name 不进 SAN），
// 自动识别：IP 字面量 → IP SAN，uri:… → URI SAN，email:… → Email SAN，
// 其余 → DNS SAN，重复项忽略。客户端证书同样允许携带 SAN
// （etcd peer 等双向场景需要）。
// days>0 为显式有效期；days<=0 为自动档（NotAfter 留零，由 signAndWrite
// 按签发 CA 的剩余寿命解析，见 autoLeafNotAfter）。
func leafTemplate(name string, sans []string, profile Profile, days int) (*x509.Certificate, error) {
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	tpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: name},
		NotBefore:    time.Now().Add(-time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  profile.ekus(),
	}
	if days > 0 {
		tpl.NotAfter = time.Now().AddDate(0, 0, days)
	}
	seen := map[string]bool{}
	for _, s := range sans {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		switch {
		case strings.HasPrefix(s, "uri:"):
			u, err := url.Parse(strings.TrimPrefix(s, "uri:"))
			if err != nil || u.Scheme == "" {
				return nil, fmt.Errorf("invalid URI SAN %q", s)
			}
			tpl.URIs = append(tpl.URIs, u)
		case strings.HasPrefix(s, "email:"):
			addr := strings.TrimPrefix(s, "email:")
			if addr == "" || !strings.Contains(addr, "@") {
				return nil, fmt.Errorf("invalid email SAN %q", s)
			}
			tpl.EmailAddresses = append(tpl.EmailAddresses, addr)
		default:
			// IP 字面量解析一次复用（原先 case 与 append 各调一次 ParseIP）
			if ip := net.ParseIP(s); ip != nil {
				tpl.IPAddresses = append(tpl.IPAddresses, ip)
			} else {
				tpl.DNSNames = append(tpl.DNSNames, s)
			}
		}
	}
	return tpl, nil
}

// autoLeafNotAfter 自动档叶子有效期（未显式指定 --days 时），按签发 CA
// 剩余寿命分段（三段在边界连续衔接）：
//
//	剩余 <= 30 天（如默认 1 天 CA）→ 跟随 CA 一起到期（短会话信任链
//	    不留尾巴，到期整体作废）
//	剩余 30 ~ 60 天（中间带）      → 剩余的 50%（既留出续期窗口，又
//	    不让叶子活得比 CA 的一半更久）
//	剩余 > 60 天（长期 CA）        → 默认最长 30 天（DefaultDays，
//	    短周期 + renew 轮换压缩泄漏窗口）
func autoLeafNotAfter(caCert *x509.Certificate) time.Time {
	now := time.Now()
	left := caCert.NotAfter.Sub(now)
	switch {
	case left <= DefaultDays*24*time.Hour:
		return caCert.NotAfter
	case left <= 2*DefaultDays*24*time.Hour:
		return now.Add(left / 2)
	default:
		return now.AddDate(0, 0, DefaultDays)
	}
}

// signAndWrite 用 CA 签发模板并落盘证书/私钥（私钥不加密——叶子密钥短周期轮换）。
// CA 取自 caCertPath/caKeyPath（空时回退 <dir>/ca.crt|ca.key）。
func signAndWrite(dir, caCertPath, caKeyPath, name string, tpl *x509.Certificate, key crypto.Signer) (string, string, string, error) {
	certPath, _, fp, err := signCert(dir, caCertPath, caKeyPath, name, tpl, key.Public())
	if err != nil {
		return "", "", "", err
	}
	keyPath := filepath.Join(dir, name+".key")
	if err := writeKey(keyPath, key); err != nil {
		return "", "", "", err
	}
	return certPath, keyPath, fp, nil
}

// signCert 用 CA 给模板与**公钥**签发证书，只写 .crt。签发不需要叶子
// 私钥（签名用 CA 私钥）——CSR 纳管与无钥续期共用此路径。
// 叶子有效期钳制到 CA 到期时刻：链校验随 CA 过期即失效，签出超出 CA 的
// NotAfter 是撒谎证书（默认 1 天 CA 时尤其常见），钳制让证书自身诚实。
func signCert(dir, caCertPath, caKeyPath, name string, tpl *x509.Certificate, pub crypto.PublicKey) (string, []byte, string, error) {
	if caCertPath == "" {
		caCertPath = filepath.Join(dir, DefaultCAFile)
	}
	if caKeyPath == "" {
		caKeyPath = filepath.Join(dir, DefaultKeyFile)
	}
	caCert, caKey, err := LoadCAAt(caCertPath, caKeyPath)
	if err != nil {
		return "", nil, "", err
	}
	// 有效期硬规则：叶子一律不得超过签发 CA 到期时刻（链随 CA 过期，
	// 超出的天数是无效谎言）。自动档（未显式给 --days）在此基础上按
	// CA 剩余寿命就近取值，见 autoLeafNotAfter。
	if tpl.NotAfter.IsZero() {
		tpl.NotAfter = autoLeafNotAfter(caCert)
	}
	if tpl.NotAfter.After(caCert.NotAfter) {
		tpl.NotAfter = caCert.NotAfter
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, caCert, pub, caKey)
	if err != nil {
		return "", nil, "", err
	}
	// 输出目录可与 CA 目录分离（--ca-cert/--ca-key），不存在时创建
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", nil, "", err
	}
	certPath := filepath.Join(dir, name+".crt")
	if err := writePEM(certPath, "CERTIFICATE", der, 0o644); err != nil {
		return "", nil, "", err
	}
	return certPath, der, FingerprintDER(der), nil
}
