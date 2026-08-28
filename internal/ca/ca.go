package ca

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	DefaultCAFile  = "ca.crt"
	DefaultKeyFile = "ca.key"
	DefaultCADays  = 1
	DefaultDays    = 30
)

// Profile 是证书用途档案
type Profile string

const (
	// ProfileServer 服务端证书（ServerAuth，wdp agent 的默认用途）。
	ProfileServer Profile = "server"
	// ProfileClient 客户端证书（ClientAuth，控制端/用户身份用）。
	ProfileClient Profile = "client"
	// ProfilePeer 双向证书（ServerAuth+ClientAuth，双向 mTLS
	// 的 peer 节点证书：同一张证书既是服务端又是客户端）。
	ProfilePeer Profile = "peer"
)

// ekus 返回档案对应的扩展密钥用途。
func (p Profile) ekus() []x509.ExtKeyUsage {
	switch p {
	case ProfileClient:
		return []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	case ProfilePeer:
		return []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}
	default: // server / 空
		return []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
	}
}

// NormalizeProfile 归一化档案名（空 = server；非法报错）。
func NormalizeProfile(p string) (Profile, error) {
	switch Profile(strings.ToLower(strings.TrimSpace(p))) {
	case "", ProfileServer:
		return ProfileServer, nil
	case ProfileClient:
		return ProfileClient, nil
	case ProfilePeer:
		return ProfilePeer, nil
	}
	return "", fmt.Errorf("unknown profile %q (server|client|peer)", p)
}

// Subject 是证书主题
type Subject struct {
	CN string   // CommonName
	O  []string // Organization
	OU []string // OrganizationalUnit
	C  []string // Country（两字母代码）
	ST []string // State/Province
	L  []string // Locality（城市）
}

// fillDefaults 空字段补 wdp 缺省（O=wdp；CN 由调用方定，不再覆盖）。
func (s Subject) fillDefaults() Subject {
	if len(s.O) == 0 {
		s.O = []string{"wdp"}
	}
	return s
}

// name 转换为 pkix.Name。
func (s Subject) name(commonName string) pkix.Name {
	return pkix.Name{
		CommonName:         commonName,
		Organization:       s.O,
		OrganizationalUnit: s.OU,
		Country:            s.C,
		Province:           s.ST,
		Locality:           s.L,
	}
}

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

// LoadCAAt 按显式路径读取根 CA（签发者）证书与私钥：即 issue/renew 的
// --ca-cert/--ca-key，用它签出新证书；私钥明文 SEC1 EC / PKCS8
// （EC/RSA/Ed25519 等实现 crypto.Signer 的类型，兼容 openssl 等自制根 CA）。
func LoadCAAt(certPath, keyPath string) (*x509.Certificate, crypto.Signer, error) {
	cert, err := parseCertificate(certPath)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to read CA certificate: %w", err)
	}
	if !cert.IsCA {
		return nil, nil, fmt.Errorf("%s is a leaf certificate, not a root CA (--ca-cert must point to the signing CA)", certPath)
	}
	if time.Now().After(cert.NotAfter) {
		return nil, nil, fmt.Errorf("root CA certificate has expired (%s), cannot sign", cert.NotAfter.Format(time.RFC3339))
	}
	der, err := readKey(keyPath)
	if err != nil {
		return nil, nil, err
	}
	key, err := parsePrivateKey(der)
	if err != nil {
		return nil, nil, err
	}
	// 证书/私钥错配校验：签发用 key、颁发者用 cert，标准库不会报错——
	// 产出链不上去的废证书，问题拖到 mTLS 握手才暴露
	if !pubEqual(cert.PublicKey, key.Public()) {
		return nil, nil, fmt.Errorf("CA certificate and private key do not match (%s / %s)", certPath, keyPath)
	}
	return cert, key, nil
}

// LoadCA 读取 <dir>/ca.crt 与 <dir>/ca.key。
// func LoadCA(dir string) (*x509.Certificate, crypto.Signer, error) {
// 	return LoadCAAt(filepath.Join(dir, CAFile), filepath.Join(dir, KeyFile))
// }

// IssueOptions 是签发参数。CACertPath/CAKeyPath 是**根 CA（签发者）**的
// 证书与私钥路径——用它签出新证书，不是产物；为空时取 <Dir>/ca.crt|ca.key，
// 可显式指定以使用自制根 CA（openssl 等）。
type IssueOptions struct {
	Dir        string   // 叶子证书输出目录
	CACertPath string   // CA 证书路径（空 = <Dir>/ca.crt）
	CAKeyPath  string   // CA 私钥路径（空 = <Dir>/ca.key）
	SANs       []string // 附加 SAN，自动识别：IP → IP SAN；uri:… → URI；email:… → Email；其余 → DNS
	Profile    Profile  // 用途档案（空 = server）
	Subject    Subject  // 主题（CN 固定取 name 参数；O/OU/C/ST/L 可覆盖，O 缺省 wdp）
	Days       int      // 有效期天数：>0 显式指定；<=0 自动档（按 CA 剩余寿命分段，见 autoLeafNotAfter）；一律不超 CA 到期
	Key        KeySpec  // 叶子私钥规格（默认 ed25519）
}

// Issue 用根 CA 签发证书。职责划分：name 只决定产物文件名
// （<dir>/<name>.crt/.key）与 CN 缺省值（--cn/Subject.CN 可覆盖 CN，
// 文件名始终跟 name）；**全部 SAN 经 sans 显式给出**（自动识别
// DNS/IP/URI/Email）。server/peer 档案零 SAN 直接拒绝——现代 TLS
// 校验只认 SAN 不认 CN，无 SAN 的服务端证书过不了主机名校验。
func Issue(o IssueOptions, name string) (string, string, string, error) {
	profile, err := NormalizeProfile(string(o.Profile))
	if err != nil {
		return "", "", "", err
	}
	// name 为空时以档案名为证书名（文件名与 CN 缺省）：
	// wdp ca issue --san web1 --profile peer → peer.crt/.key, CN=peer
	if name == "" {
		name = string(profile)
	}
	if profile != ProfileClient && len(o.SANs) == 0 {
		return "", "", "", fmt.Errorf(
			"profile %q requires at least one SAN via --san (modern TLS verifies SAN only, CN is not checked)", profile)
	}
	key, err := o.Key.Generate()
	if err != nil {
		return "", "", "", err
	}
	tpl, err := leafTemplate(name, o.SANs, profile, o.Days)
	if err != nil {
		return "", "", "", err
	}
	cn := name
	if o.Subject.CN != "" {
		cn = o.Subject.CN
	}
	tpl.Subject = o.Subject.fillDefaults().name(cn)
	return signAndWrite(o.Dir, o.CACertPath, o.CAKeyPath, name, tpl, key)
}

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
	if filepath.Dir(outPath) == filepath.Dir(o.CertPath) {
		ts := time.Now().Format("20060102-150405")
		for _, p := range []string{o.CertPath, o.KeyPath} {
			if p != "" {
				if _, err := os.Stat(p); err == nil {
					_ = os.Rename(p, p+".old."+ts)
				}
			}
		}
	}
	return signAndWrite(filepath.Dir(outPath), o.CACertPath, o.CAKeyPath,
		strings.TrimSuffix(filepath.Base(outPath), ".crt"), tpl, key)
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

// leafTemplate 构造叶子证书模板。SAN 全部来自 sans（name 不进 SAN），
// 自动识别：IP 字面量 → IP SAN，uri:… → URI SAN，email:… → Email SAN，
// 其余 → DNS SAN，重复项忽略。客户端证书同样允许携带 SAN
// （etcd peer 等双向场景需要）。
// days>0 为显式有效期；days<=0 为自动档（NotAfter 留零，由 signAndWrite
// 按签发 CA 的剩余寿命解析，见 autoLeafNotAfter）。
func leafTemplate(name string, sans []string, profile Profile, days int) (*x509.Certificate, error) {
	tpl := &x509.Certificate{
		SerialNumber: randomSerial(),
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
		case net.ParseIP(s) != nil:
			tpl.IPAddresses = append(tpl.IPAddresses, net.ParseIP(s))
		default:
			tpl.DNSNames = append(tpl.DNSNames, s)
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
// 叶子有效期钳制到 CA 到期时刻：链校验随 CA 过期即失效，签出超出 CA 的
// NotAfter 是撒谎证书（默认 1 天 CA 时尤其常见），钳制让证书自身诚实。
func signAndWrite(dir, caCertPath, caKeyPath, name string, tpl *x509.Certificate, key crypto.Signer) (string, string, string, error) {
	if caCertPath == "" {
		caCertPath = filepath.Join(dir, DefaultCAFile)
	}
	if caKeyPath == "" {
		caKeyPath = filepath.Join(dir, DefaultKeyFile)
	}
	caCert, caKey, err := LoadCAAt(caCertPath, caKeyPath)
	if err != nil {
		return "", "", "", err
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
	der, err := x509.CreateCertificate(rand.Reader, tpl, caCert, key.Public(), caKey)
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
	if err := writeKey(keyPath, key); err != nil {
		return "", "", "", err
	}
	return certPath, keyPath, FingerprintDER(der), nil
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
		return "", fmt.Errorf("%s is not a certificate PEM", path)
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
		return "", fmt.Errorf("invalid fingerprint %q (SHA256 required)", s)
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
		return nil, fmt.Errorf("%s is not a certificate PEM", path)
	}
	return x509.ParseCertificate(block.Bytes)
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

func writePEM(path, blockType string, der []byte, mode os.FileMode) error {
	// 原子写：临时文件 + rename（O_TRUNC 原地重写，进程在截断后写完前
	// 崩溃或磁盘满即私钥/证书永久损坏）。rename 落盘文件的权限即临时文件
	// 的 chmod 结果，重写已存在的宽松权限文件时自动收紧到 mode
	tmp, err := os.CreateTemp(filepath.Dir(path), ".wdp-ca-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	abort := func(e error) error {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return e
	}
	if err := pem.Encode(tmp, &pem.Block{Type: blockType, Bytes: der}); err != nil {
		return abort(err)
	}
	if err := tmp.Chmod(mode); err != nil {
		return abort(err)
	}
	if err := tmp.Sync(); err != nil {
		return abort(err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, path)
}

func randomSerial() *big.Int {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return big.NewInt(time.Now().UnixNano())
	}
	return n
}
