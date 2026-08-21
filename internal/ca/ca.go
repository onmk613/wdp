// Package ca 提供自建 CA 与证书签发（crypto/x509 标准库实现）。
// 用于 agent 通道 mTLS 双向认证，能力包括：
//
//   - 初始化：生成 ca.crt / ca.key（私钥可加密存储）
//   - 导入：复用已有 CA 证书/私钥对（二次分发同一信任链，支持组织既有 CA）
//   - 签发服务端证书：SAN = 主机名 + 附加地址（多地址主机一张证书全覆盖）
//   - 签发控制端客户端证书
//   - 续期：保留 SAN/EKU/密钥
//
// 安全设计：
//   - CA 私钥可用口令加密落盘（PBKDF2-SHA256 10万轮 + AES-256-GCM），
//     口令可显式提供或经环境变量 WDP_CA_PASSPHRASE 传入
//   - 叶子证书默认 30 天（签发时可调），到期轮换（保留 SAN/EKU/密钥）
//   - CA 设置 PathLen=0：即便私钥泄漏也不能签发中间 CA
//   - 签发/续期输出证书 SHA256 指纹，供 agent 端精确吊销
package ca

import (
	"crypto"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/pbkdf2"
)

// CAFile / KeyFile 是 CA 产物文件名。
const (
	CAFile     = "ca.crt"
	KeyFile    = "ca.key"
	caValidity = 10 * 365 * 24 * time.Hour // CA 有效期 10 年
	// DefaultDays 是叶子证书默认有效期（短周期 + renew 轮换，压缩泄漏窗口）。
	DefaultDays = 30
	// PassphraseEnv 是 CA 私钥口令的环境变量名。
	PassphraseEnv = "WDP_CA_PASSPHRASE"

	encPEMType   = "WDP ENCRYPTED EC PRIVATE KEY"
	plainPEMType = "EC PRIVATE KEY"
	kdfIter      = 100000
)

// PassphraseFromEnv 返回环境变量中的 CA 口令。
func PassphraseFromEnv() string { return os.Getenv(PassphraseEnv) }

// Init 在 dir 生成自签 CA。passphrase 非空时私钥加密存储。
// 返回 (caPath, keyPath, 证书指纹)。口令解析：显式参数优先，其次环境变量
// WDP_CA_PASSPHRASE，两者皆空 = 明文存储（与 --passphrase 帮助文案一致）。
func Init(dir, passphrase string) (string, string, string, error) {
	passphrase = firstNonEmpty(passphrase, PassphraseFromEnv())
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", "", err
	}
	caPath, keyPath := filepath.Join(dir, CAFile), filepath.Join(dir, KeyFile)
	if _, err := os.Stat(caPath); err == nil {
		return "", "", "", fmt.Errorf("%s 已存在（如需重建请先删除）", caPath)
	}

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", "", "", err
	}
	tpl := &x509.Certificate{
		SerialNumber:          randomSerial(),
		Subject:               pkix.Name{CommonName: "wdp-ca", Organization: []string{"wdp"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(caValidity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            0, // 禁止签发中间 CA
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &caKey.PublicKey, caKey)
	if err != nil {
		return "", "", "", err
	}
	if err := writePEM(caPath, "CERTIFICATE", der, 0o644); err != nil {
		return "", "", "", err
	}
	keyDER, err := x509.MarshalECPrivateKey(caKey)
	if err != nil {
		return "", "", "", err
	}
	if err := writeKey(keyPath, keyDER, passphrase); err != nil {
		return "", "", "", err
	}
	return caPath, keyPath, FingerprintDER(der), nil
}

// Import 将已有 CA 证书/私钥对导入 dir 作为本工具的 CA 复用（二次分发：
// 多环境共享同一信任链，已信任该 CA 的 agent 无需重新建立信任）。
// 校验：证书是 CA 且未过期、私钥与证书公钥匹配。passphrase 同时用于解密源私钥
// 与加密落盘副本（显式参数优先，其次环境变量；两者皆空 = 明文存储）。
func Import(dir, certSrc, keySrc, passphrase string) (string, string, string, error) {
	passphrase = firstNonEmpty(passphrase, PassphraseFromEnv())
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", "", err
	}
	caPath, keyPath := filepath.Join(dir, CAFile), filepath.Join(dir, KeyFile)
	if _, err := os.Stat(caPath); err == nil {
		return "", "", "", fmt.Errorf("%s 已存在（如需替换请先删除）", caPath)
	}
	cert, err := parseCertificate(certSrc)
	if err != nil {
		return "", "", "", fmt.Errorf("读取 CA 证书失败: %w", err)
	}
	if !cert.IsCA {
		return "", "", "", fmt.Errorf("%s 不是 CA 证书（BasicConstraints CA=false）", certSrc)
	}
	if time.Now().After(cert.NotAfter) {
		return "", "", "", fmt.Errorf("CA 证书已过期（%s），无法用于签发", cert.NotAfter.Format(time.RFC3339))
	}
	der, err := readKey(keySrc, passphrase)
	if err != nil {
		return "", "", "", fmt.Errorf("读取 CA 私钥失败: %w", err)
	}
	signer, err := parsePrivateKey(der)
	if err != nil {
		return "", "", "", fmt.Errorf("读取 CA 私钥失败: %w", err)
	}
	if !pubEqual(cert.PublicKey, signer.Public()) {
		return "", "", "", fmt.Errorf("CA 证书与私钥不匹配（%s / %s）", certSrc, keySrc)
	}
	if err := writePEM(caPath, "CERTIFICATE", cert.Raw, 0o644); err != nil {
		return "", "", "", err
	}
	if err := writeKey(keyPath, der, passphrase); err != nil {
		return "", "", "", err
	}
	return caPath, keyPath, FingerprintDER(cert.Raw), nil
}

// LoadCAAt 按显式路径读取 CA 证书与私钥（加密私钥需口令：显式参数优先，其次环境变量）。
// 私钥支持 SEC1 EC 与 PKCS8（EC/RSA/Ed25519 等实现 crypto.Signer 的类型）。
func LoadCAAt(certPath, keyPath, passphrase string) (*x509.Certificate, crypto.Signer, error) {
	cert, err := parseCertificate(certPath)
	if err != nil {
		return nil, nil, fmt.Errorf("读取 CA 证书失败: %w", err)
	}
	der, err := readKey(keyPath, firstNonEmpty(passphrase, PassphraseFromEnv()))
	if err != nil {
		return nil, nil, err
	}
	key, err := parsePrivateKey(der)
	if err != nil {
		return nil, nil, err
	}
	return cert, key, nil
}

// LoadCA 读取 <dir>/ca.crt 与 <dir>/ca.key。
func LoadCA(dir, passphrase string) (*x509.Certificate, crypto.Signer, error) {
	return LoadCAAt(filepath.Join(dir, CAFile), filepath.Join(dir, KeyFile), passphrase)
}

// IssueOptions 是签发参数。CACertPath/CAKeyPath 为空时取 <Dir>/ca.crt|ca.key，
// 可显式指定以复用任意位置的 CA（如导入的或组织既有 CA）。
type IssueOptions struct {
	Dir        string   // 叶子证书输出目录
	CACertPath string   // CA 证书路径（空 = <Dir>/ca.crt）
	CAKeyPath  string   // CA 私钥路径（空 = <Dir>/ca.key）
	Passphrase string   // CA 私钥口令（空 = 取环境变量）
	SANs       []string // 服务端证书附加 SAN
	Client     bool     // 签控制端客户端证书
	Days       int      // 有效期天数，<=0 用 DefaultDays
}

// Issue 用 CA 签发证书。client=false 签服务端证书（SAN 含 name，IP 名同时加 IP SAN），
// client=true 签控制端客户端证书。sans 为服务端证书追加的额外 SAN
// （IP 或域名，自动去重）——多地址/NAT/端口转发主机一张证书覆盖全部可达地址。
// 产物 <dir>/<name>.crt / <name>.key。返回 (certPath, keyPath, 指纹)。
func Issue(o IssueOptions, name string) (string, string, string, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", "", "", err
	}
	tpl, err := leafTemplate(name, o.SANs, o.Client, o.Days)
	if err != nil {
		return "", "", "", err
	}
	return signAndWrite(o.Dir, o.CACertPath, o.CAKeyPath, o.Passphrase, name, tpl, key)
}

// RenewOptions 是续期参数（CA 路径语义同 IssueOptions）。
type RenewOptions struct {
	Dir        string // 叶子证书输出目录，兼原证书/私钥读取目录
	CACertPath string // CA 证书路径（空 = <Dir>/ca.crt）
	CAKeyPath  string // CA 私钥路径（空 = <Dir>/ca.key）
	Passphrase string // CA 私钥口令（空 = 取环境变量）
	NewKey     bool   // 换新私钥（旧证书立即失效）
	Days       int    // 新有效期天数，<=0 用 DefaultDays
}

// Renew 续期已有证书：保留 CN/SAN/EKU 与原私钥（newKey=true 换新私钥）。
// 返回 (certPath, keyPath, 指纹)。
func Renew(o RenewOptions, name string) (string, string, string, error) {
	certPath := filepath.Join(o.Dir, name+".crt")
	old, err := parseCertificate(certPath)
	if err != nil {
		return "", "", "", fmt.Errorf("读取原证书失败（%s）: %w", certPath, err)
	}

	tpl, err := leafTemplate("", nil, false, o.Days) // 基础模板，字段随后从旧证书继承（SAN 全量继承）
	if err != nil {
		return "", "", "", err
	}
	tpl.Subject = old.Subject
	tpl.DNSNames = old.DNSNames
	tpl.IPAddresses = old.IPAddresses
	tpl.ExtKeyUsage = old.ExtKeyUsage

	var key *ecdsa.PrivateKey
	if o.NewKey {
		if key, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader); err != nil {
			return "", "", "", err
		}
	} else {
		keyDER, err := readKey(filepath.Join(o.Dir, name+".key"), "")
		if err != nil {
			return "", "", "", fmt.Errorf("读取原私钥失败: %w", err)
		}
		if key, err = x509.ParseECPrivateKey(keyDER); err != nil {
			return "", "", "", fmt.Errorf("解析原私钥失败: %w", err)
		}
	}
	return signAndWrite(o.Dir, o.CACertPath, o.CAKeyPath, o.Passphrase, name, tpl, key)
}

// leafTemplate 构造叶子证书模板。sans 追加额外 SAN（IP → IP SAN，域名 → DNS SAN，
// 与 name 重复的自动忽略）；客户端证书不含 SAN，传 sans 视为用法错误。
func leafTemplate(name string, sans []string, client bool, days int) (*x509.Certificate, error) {
	if days <= 0 {
		days = DefaultDays
	}
	tpl := &x509.Certificate{
		SerialNumber: randomSerial(),
		Subject:      pkix.Name{CommonName: name, Organization: []string{"wdp"}},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(0, 0, days),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
	}
	if client {
		if len(sans) > 0 {
			return nil, fmt.Errorf("SAN 仅用于服务端证书（--client 客户端证书不含 SAN）")
		}
		tpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
		return tpl, nil
	}
	tpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
	tpl.DNSNames = []string{name}
	if ip := net.ParseIP(name); ip != nil {
		tpl.IPAddresses = []net.IP{ip}
	}
	seen := map[string]bool{name: true}
	for _, s := range sans {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		if ip := net.ParseIP(s); ip != nil {
			tpl.IPAddresses = append(tpl.IPAddresses, ip)
		} else {
			tpl.DNSNames = append(tpl.DNSNames, s)
		}
	}
	return tpl, nil
}

// signAndWrite 用 CA 签发模板并落盘证书/私钥（私钥不加密——叶子密钥短周期轮换）。
// CA 取自 caCertPath/caKeyPath（空时回退 <dir>/ca.crt|ca.key）。
func signAndWrite(dir, caCertPath, caKeyPath, passphrase, name string, tpl *x509.Certificate, key *ecdsa.PrivateKey) (string, string, string, error) {
	if caCertPath == "" {
		caCertPath = filepath.Join(dir, CAFile)
	}
	if caKeyPath == "" {
		caKeyPath = filepath.Join(dir, KeyFile)
	}
	caCert, caKey, err := LoadCAAt(caCertPath, caKeyPath, passphrase)
	if err != nil {
		return "", "", "", err
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, caCert, &key.PublicKey, caKey)
	if err != nil {
		return "", "", "", err
	}
	// 输出目录可与 CA 目录分离（--ca-cert/--ca-key），不存在时创建
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", "", "", err
	}
	certPath := filepath.Join(dir, name+".crt")
	keyPath := filepath.Join(dir, name+".key")
	if err := writePEM(certPath, "CERTIFICATE", der, 0o644); err != nil {
		return "", "", "", err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return "", "", "", err
	}
	if err := writePEM(keyPath, plainPEMType, keyDER, 0o600); err != nil {
		return "", "", "", err
	}
	return certPath, keyPath, FingerprintDER(der), nil
}

// ---- 加密私钥信封：PBKDF2-SHA256(10万轮) → AES-256-GCM ----
// payload = salt(16) | nonce(12) | AEAD(DER)

// writeKey 落盘私钥：passphrase 非空时加密为 wdp 信封，否则明文
// （PEM 类型按 DER 编码自动识别 SEC1 EC / PKCS8，导入的密钥保持原编码）。
func writeKey(path string, der []byte, passphrase string) error {
	if passphrase == "" {
		return writePEM(path, keyPEMType(der), der, 0o600)
	}
	enc, err := encryptDER(der, passphrase)
	if err != nil {
		return err
	}
	return writePEM(path, encPEMType, enc, 0o600)
}

func encryptDER(der []byte, passphrase string) ([]byte, error) {
	salt := make([]byte, 16)
	nonce := make([]byte, 12)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	aead, err := newAEAD(passphrase, salt)
	if err != nil {
		return nil, err
	}
	ct := aead.Seal(nil, nonce, der, nil)
	out := make([]byte, 0, len(salt)+len(nonce)+len(ct))
	out = append(out, salt...)
	out = append(out, nonce...)
	out = append(out, ct...)
	return out, nil
}

func decryptDER(payload []byte, passphrase string) ([]byte, error) {
	if len(payload) < 16+12+16 {
		return nil, fmt.Errorf("加密私钥内容不完整")
	}
	salt, nonce, ct := payload[:16], payload[16:28], payload[28:]
	aead, err := newAEAD(passphrase, salt)
	if err != nil {
		return nil, err
	}
	der, err := aead.Open(nil, nonce, ct, nil)
	if err != nil {
		return nil, fmt.Errorf("私钥解密失败（口令错误或文件损坏）")
	}
	return der, nil
}

func newAEAD(passphrase string, salt []byte) (cipher.AEAD, error) {
	key := pbkdf2.Key([]byte(passphrase), salt, kdfIter, 32, sha256.New)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// readKey 读取私钥（自动识别加密/明文信封）。
func readKey(path, passphrase string) ([]byte, error) {
	keyPEM, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取私钥失败: %w", err)
	}
	block, _ := pem.Decode(keyPEM)
	if block == nil {
		return nil, fmt.Errorf("解析私钥 PEM 失败")
	}
	switch block.Type {
	case encPEMType:
		if passphrase == "" {
			return nil, fmt.Errorf("CA 私钥已加密：需要 --passphrase 或环境变量 %s", PassphraseEnv)
		}
		return decryptDER(block.Bytes, passphrase)
	case plainPEMType, "PRIVATE KEY":
		return block.Bytes, nil
	default:
		return nil, fmt.Errorf("未知的私钥类型 %q", block.Type)
	}
}

// ---- 指纹 ----

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
		return "", fmt.Errorf("%s 不是证书 PEM", path)
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
		return "", fmt.Errorf("非法指纹 %q（需要 SHA256）", s)
	}
	return s, nil
}

// ---- 基础工具 ----

// parseCertificate 读取证书 PEM 文件并解析。
func parseCertificate(path string) (*x509.Certificate, error) {
	pemBytes, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(pemBytes)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("%s 不是证书 PEM", path)
	}
	return x509.ParseCertificate(block.Bytes)
}

// parsePrivateKey 解析私钥 DER（SEC1 EC 或 PKCS8），须实现 crypto.Signer
// （导入的组织 CA 可能是 RSA/Ed25519 等 PKCS8 密钥）。
func parsePrivateKey(der []byte) (crypto.Signer, error) {
	if key, err := x509.ParseECPrivateKey(der); err == nil {
		return key, nil
	}
	key, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return nil, fmt.Errorf("解析私钥失败（支持 SEC1 EC / PKCS8）: %w", err)
	}
	signer, ok := key.(crypto.Signer)
	if !ok {
		return nil, fmt.Errorf("私钥类型 %T 不支持签名", key)
	}
	return signer, nil
}

// pubEqual 比较两个公钥（标准库密钥类型均实现 Equal）。
func pubEqual(a, b crypto.PublicKey) bool {
	type equaler interface{ Equal(crypto.PublicKey) bool }
	ae, ok := a.(equaler)
	return ok && ae.Equal(b)
}

// keyPEMType 按 DER 编码识别明文私钥 PEM 类型。
func keyPEMType(der []byte) string {
	if _, err := x509.ParseECPrivateKey(der); err == nil {
		return plainPEMType
	}
	return "PRIVATE KEY"
}

func writePEM(path, blockType string, der []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	defer f.Close()
	return pem.Encode(f, &pem.Block{Type: blockType, Bytes: der})
}

func randomSerial() *big.Int {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return big.NewInt(time.Now().UnixNano())
	}
	return n
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
