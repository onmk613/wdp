package ca

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestInitDefaultValidity 默认 CA 有效期 1 天（远程部署短周期信任根），
// 私钥明文落盘（0600、目录 0700，静态防护靠文件权限）。
func TestInitDefaultValidity(t *testing.T) {
	dir := t.TempDir()
	crt, key, _, err := Init(InitOptions{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(crt); err != nil {
		t.Fatal(err)
	}
	caCert, err := parseCertificate(crt)
	if err != nil {
		t.Fatal(err)
	}
	if d := caCert.NotAfter.Sub(caCert.NotBefore); d > 26*time.Hour || d < 22*time.Hour {
		t.Fatalf("默认 CA 有效期应约 1 天，实际 %v", d)
	}
	// 私钥明文（非旧加密信封）
	keyPEM, _ := os.ReadFile(key)
	block, _ := pem.Decode(keyPEM)
	if block == nil || block.Type != "PRIVATE KEY" {
		t.Fatalf("私钥应明文存储: %v", block.Type)
	}
	// 无口令直接加载可用
	if _, _, err := LoadCAAt(filepath.Join(dir, DefaultCAFile), filepath.Join(dir, DefaultKeyFile)); err != nil {
		t.Fatalf("明文私钥应可直接加载: %v", err)
	}
}

// TestInitCustomDays CA 有效期自定义（常驻 agent 模式加大窗口）。
func TestInitCustomDays(t *testing.T) {
	dir := t.TempDir()
	crt, _, _, err := Init(InitOptions{Dir: dir, Days: 365})
	if err != nil {
		t.Fatal(err)
	}
	caCert, err := parseCertificate(crt)
	if err != nil {
		t.Fatal(err)
	}
	if d := caCert.NotAfter.Sub(caCert.NotBefore); d > 367*24*time.Hour || d < 363*24*time.Hour {
		t.Fatalf("--days 365 有效期异常，实际 %v", d)
	}
}

// TestAutoLeafValidity 自动档叶子有效期三分段：
//   - 短 CA（<=30 天，如默认 1 天）→ 跟随 CA 一起到期
//   - 中间带（30~60 天）→ 剩余的 50%
//   - 长期 CA（>60 天）→ 默认最长 30 天
//
// 显式 --days 不受分段影响，仅受"不超过 CA 到期"钳制（见 TestLeafClampedToCA）。
func TestAutoLeafValidity(t *testing.T) {
	day := 24 * time.Hour
	for _, tc := range []struct {
		caDays int
		want   time.Duration // 期望的叶子有效跨度（粗界）
	}{
		{1, day},        // 短 CA：跟随 CA
		{15, 15 * day},  // <=30 天：仍跟随 CA
		{40, 20 * day},  // 中间带：50%
		{60, 30 * day},  // 边界：50% = 30 天
		{365, 30 * day}, // 长期：上限 30 天
	} {
		dir := t.TempDir()
		if _, _, _, err := Init(InitOptions{Dir: dir, Days: tc.caDays}); err != nil {
			t.Fatal(err)
		}
		crt, _, _, err := Issue(IssueOptions{Dir: dir, SANs: []string{"auto"}}, "auto") // Days 省略 = 自动档
		if err != nil {
			t.Fatalf("ca=%dd: %v", tc.caDays, err)
		}
		leaf, err := parseCertificate(crt)
		if err != nil {
			t.Fatal(err)
		}
		got := leaf.NotAfter.Sub(leaf.NotBefore)
		// NotBefore 前移 1h + AddDate 天数粒度，留 ±2h 容差
		if got < tc.want-2*time.Hour || got > tc.want+2*time.Hour {
			t.Fatalf("CA %d 天：自动档叶子有效期应约 %v，实际 %v", tc.caDays, tc.want, got)
		}
		// 硬规则：任何情况下不超过 CA 到期
		caCert, _, err := LoadCAAt(filepath.Join(dir, DefaultCAFile), filepath.Join(dir, DefaultKeyFile))
		if err != nil {
			t.Fatal(err)
		}
		if leaf.NotAfter.After(caCert.NotAfter) {
			t.Fatalf("叶子不得晚于 CA 到期: %v > %v", leaf.NotAfter, caCert.NotAfter)
		}
	}
}

// TestCAPathLenCustom --path-len 参数：默认 0（禁中间 CA）；N>0 写入
// MaxPathLen=N；-1 不限。
func TestCAPathLenCustom(t *testing.T) {
	dir := t.TempDir()
	if _, _, _, err := Init(InitOptions{Dir: dir, PathLen: 2}); err != nil {
		t.Fatal(err)
	}
	cert, _, err := LoadCAAt(filepath.Join(dir, DefaultCAFile), filepath.Join(dir, DefaultKeyFile))
	if err != nil {
		t.Fatal(err)
	}
	if cert.MaxPathLen != 2 || cert.MaxPathLenZero {
		t.Fatalf("PathLen=2 应写入 MaxPathLen=2，实际 %d zero=%v", cert.MaxPathLen, cert.MaxPathLenZero)
	}
	// -1 = 不限
	dir2 := t.TempDir()
	if _, _, _, err := Init(InitOptions{Dir: dir2, PathLen: -1}); err != nil {
		t.Fatal(err)
	}
	cert2, _, err := LoadCAAt(filepath.Join(dir2, DefaultCAFile), filepath.Join(dir2, DefaultKeyFile))
	if err != nil {
		t.Fatal(err)
	}
	if cert2.MaxPathLen != -1 || cert2.MaxPathLenZero {
		t.Fatalf("PathLen=-1 应为不限，实际 %d zero=%v", cert2.MaxPathLen, cert2.MaxPathLenZero)
	}
}

// TestLeafClampedToCA 叶子有效期钳制：签发请求超过 CA 到期时 NotAfter
// 被压到 CA 到期时刻（链随 CA 过期，超出的天数是无效谎言）。
func TestLeafClampedToCA(t *testing.T) {
	dir := t.TempDir()
	caCrt, _, _, err := Init(InitOptions{Dir: dir}) // 1 天 CA
	if err != nil {
		t.Fatal(err)
	}
	caCert, err := parseCertificate(caCrt)
	if err != nil {
		t.Fatal(err)
	}
	crt, _, _, err := Issue(IssueOptions{Dir: dir, Days: 30, SANs: []string{"clamped"}}, "clamped")
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := parseCertificate(crt)
	if err != nil {
		t.Fatal(err)
	}
	if !leaf.NotAfter.Equal(caCert.NotAfter) {
		t.Fatalf("叶子有效期应钳制到 CA 到期: leaf=%v ca=%v", leaf.NotAfter, caCert.NotAfter)
	}
}

func TestCAPathLenZero(t *testing.T) {
	dir := t.TempDir()
	_, _, _, err := Init(InitOptions{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	cert, _, err := LoadCAAt(filepath.Join(dir, DefaultCAFile), filepath.Join(dir, DefaultKeyFile))
	if err != nil {
		t.Fatal(err)
	}
	if !cert.MaxPathLenZero || cert.MaxPathLen != 0 {
		t.Fatalf("CA 应禁止签发中间 CA: MaxPathLen=%d", cert.MaxPathLen)
	}
}

func TestIssueShortValidityAndFingerprint(t *testing.T) {
	dir := t.TempDir()
	Init(InitOptions{Dir: dir, Days: 365})
	crt, _, fp, err := Issue(IssueOptions{Dir: dir, Days: 30, SANs: []string{"10.0.0.1"}}, "10.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if len(fp) != len("sha256:")+64 {
		t.Fatalf("指纹格式: %q", fp)
	}
	// 默认有效期 30 天
	_, _, fp2, err := Issue(IssueOptions{Dir: dir, SANs: []string{"host1"}}, "host1")
	if err != nil {
		t.Fatal(err)
	}
	if fp == fp2 {
		t.Fatal("不同证书指纹应不同")
	}
	pemBytes, _ := os.ReadFile(crt)
	block, _ := pem.Decode(pemBytes)
	cert, _ := x509.ParseCertificate(block.Bytes)
	// --days 30（含 NotBefore 前移 1h）
	if d := cert.NotAfter.Sub(cert.NotBefore); d > 32*24*time.Hour || d < 29*24*time.Hour {
		t.Fatalf("--days 30 有效期异常，实际 %v", d)
	}
	if len(cert.IPAddresses) != 1 || cert.IPAddresses[0].String() != "10.0.0.1" {
		t.Fatalf("IP SAN: %v", cert.IPAddresses)
	}
	// IP 名由 x509 规范化移入 IP SAN（DNSNames 不重复保留）
}

func TestRenewPreservesSANsAndKey(t *testing.T) {
	dir := t.TempDir()
	Init(InitOptions{Dir: dir, Days: 365})
	crt, keyPath, _, err := Issue(IssueOptions{Dir: dir, Days: 1, SANs: []string{"agent1"}}, "agent1")
	if err != nil {
		t.Fatal(err)
	}
	oldPEM, _ := os.ReadFile(crt)
	ob, _ := pem.Decode(oldPEM)
	old, _ := x509.ParseCertificate(ob.Bytes)
	oldKey, _ := os.ReadFile(keyPath)

	// 延期 +90 天（保留私钥）；旧件自动备份
	oldPath := filepath.Join(dir, "agent1.crt")
	newCrt, newKeyPath, _, err := Renew(RenewOptions{
		CertPath: oldPath, KeyPath: filepath.Join(dir, "agent1.key"),
		OutPath: oldPath, Days: 90,
	})
	if err != nil {
		t.Fatal(err)
	}
	newPEM, _ := os.ReadFile(newCrt)
	nb, _ := pem.Decode(newPEM)
	renewed, _ := x509.ParseCertificate(nb.Bytes)

	if renewed.DNSNames[0] != old.DNSNames[0] {
		t.Fatalf("SAN 未保留: %v", renewed.DNSNames)
	}
	if len(renewed.ExtKeyUsage) != len(old.ExtKeyUsage) {
		t.Fatal("EKU 未保留")
	}
	want := old.NotAfter.AddDate(0, 0, 90)
	if renewed.NotAfter.Sub(want) > 2*time.Hour || want.Sub(renewed.NotAfter) > 2*time.Hour {
		t.Fatalf("新到期应 = 原到期+90天: got %v want %v", renewed.NotAfter, want)
	}
	entries, _ := os.ReadDir(dir)
	backup := 0
	for _, e := range entries {
		if strings.Contains(e.Name(), ".old.") {
			backup++
		}
	}
	if backup < 2 {
		t.Fatalf("旧证书与私钥都应备份: %d 个 .old. 文件", backup)
	}
	// 私钥保留（文件内容一致）
	newKey, _ := os.ReadFile(newKeyPath)
	if string(newKey) != string(oldKey) {
		t.Fatal("默认应保留原私钥")
	}
}

func TestRenewNewKey(t *testing.T) {
	dir := t.TempDir()
	Init(InitOptions{Dir: dir})
	_, keyPath, _, _ := Issue(IssueOptions{Dir: dir, SANs: []string{"svc"}}, "svc")
	oldKey, _ := os.ReadFile(keyPath)
	if _, newKeyPath, _, err := Renew(RenewOptions{
		CertPath:   filepath.Join(dir, "svc.crt"),
		OutPath:    filepath.Join(dir, "svc.crt"),
		CACertPath: filepath.Join(dir, DefaultCAFile), CAKeyPath: filepath.Join(dir, DefaultKeyFile),
		NewKey: true,
	}); err != nil {
		t.Fatal(err)
	} else {
		newKey, _ := os.ReadFile(newKeyPath)
		if string(newKey) == string(oldKey) {
			t.Fatal("--new-key 应更换私钥")
		}
	}
}

func TestParsePin(t *testing.T) {
	good := "sha256:" + repeatHex(64)
	if _, err := ParsePin(good); err != nil {
		t.Fatalf("ParsePin(%q) err=%v", good, err)
	}
	coloned := repeatHex(64)
	var withColons string
	for i := 0; i < len(coloned); i += 2 {
		if withColons != "" {
			withColons += ":"
		}
		withColons += coloned[i : i+2]
	}
	if _, err := ParsePin(withColons); err != nil {
		t.Fatalf("冒号分隔指纹应接受: %v", err)
	}
	if _, err := ParsePin("sha256:abcd"); err == nil {
		t.Fatal("短指纹应报错")
	}
	if _, err := ParsePin("zz" + repeatHex(62)); err == nil {
		t.Fatal("非 hex 应报错")
	}
}

func repeatHex(n int) string {
	s := ""
	for range n {
		s += "a"
	}
	return s
}

func TestIssueMultiSAN(t *testing.T) {
	dir := t.TempDir()
	Init(InitOptions{Dir: dir})
	// name 是域名；附加 IP 与域名（含与 name 重复项与空串，应去重/忽略）
	crt, _, _, err := Issue(IssueOptions{Dir: dir, SANs: []string{"web1", "10.0.0.14", "web1.internal", ""}}, "web1")
	if err != nil {
		t.Fatal(err)
	}
	pemBytes, _ := os.ReadFile(crt)
	block, _ := pem.Decode(pemBytes)
	cert, _ := x509.ParseCertificate(block.Bytes)

	hasDNS := func(want string) bool {
		return slices.Contains(cert.DNSNames, want)
	}
	if !hasDNS("web1") || !hasDNS("web1.internal") {
		t.Fatalf("DNS SAN: %v", cert.DNSNames)
	}
	if len(cert.IPAddresses) != 1 || cert.IPAddresses[0].String() != "10.0.0.14" {
		t.Fatalf("IP SAN: %v", cert.IPAddresses)
	}
	// 语义验证：所有可达地址都能通过主机名校验（多地址主机的核心诉求）
	for _, addr := range []string{"web1", "web1.internal", "10.0.0.14"} {
		if err := cert.VerifyHostname(addr); err != nil {
			t.Fatalf("VerifyHostname(%s): %v", addr, err)
		}
	}
	// 未包含的地址仍被拒（校验没有因多 SAN 而放宽）
	if err := cert.VerifyHostname("other.internal"); err == nil {
		t.Fatal("未包含的地址应校验失败")
	}
}

func TestRenewPreservesMultiSAN(t *testing.T) {
	dir := t.TempDir()
	Init(InitOptions{Dir: dir, Days: 365})
	crt, _, _, err := Issue(IssueOptions{Dir: dir, SANs: []string{"web1", "10.0.0.14", "web1.internal"}, Days: 1}, "web1")
	if err != nil {
		t.Fatal(err)
	}
	newCrt, _, _, err := Renew(RenewOptions{
		CertPath: filepath.Join(dir, "web1.crt"), KeyPath: filepath.Join(dir, "web1.key"),
		OutPath: filepath.Join(dir, "web1.crt"), Days: 90,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{crt, newCrt} {
		pemBytes, _ := os.ReadFile(path)
		block, _ := pem.Decode(pemBytes)
		cert, _ := x509.ParseCertificate(block.Bytes)
		if len(cert.IPAddresses) != 1 || cert.IPAddresses[0].String() != "10.0.0.14" {
			t.Fatalf("renew 后 IP SAN 未保留: %v", cert.IPAddresses)
		}
		found := 0
		for _, n := range cert.DNSNames {
			if n == "web1" || n == "web1.internal" {
				found++
			}
		}
		if found != 2 {
			t.Fatalf("renew 后 DNS SAN 未保留: %v", cert.DNSNames)
		}
	}
}

// TestInspect CA 与叶子证书的信息解析（ca show 的域逻辑）。
func TestInspect(t *testing.T) {
	dir := t.TempDir()
	Init(InitOptions{Dir: dir})

	caInfo, err := Inspect(filepath.Join(dir, DefaultCAFile))
	if err != nil {
		t.Fatal(err)
	}
	if !caInfo.IsCA || !caInfo.SelfSigned || !caInfo.PathLenZero {
		t.Fatalf("CA 信息不符: %+v", caInfo)
	}
	if !strings.HasPrefix(caInfo.Fingerprint, "sha256:") || caInfo.PublicKey != "Ed25519" {
		t.Fatalf("指纹/公钥: %q %q", caInfo.Fingerprint, caInfo.PublicKey)
	}
	// CA 密钥用途含 CertSign，且不含 SAN/EKU 服务端字段
	if !contains(caInfo.KeyUsage, "CertSign") || len(caInfo.ExtKeyUsage) != 0 || len(caInfo.DNSNames) != 0 {
		t.Fatalf("CA 用途/SAN: %v %v %v", caInfo.KeyUsage, caInfo.ExtKeyUsage, caInfo.DNSNames)
	}

	crt, _, fp, err := Issue(IssueOptions{Dir: dir, SANs: []string{"web1", "10.0.0.14", "web1.internal"}}, "web1")
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := Inspect(crt)
	if err != nil {
		t.Fatal(err)
	}
	if leaf.IsCA || leaf.SelfSigned {
		t.Fatalf("叶子证书信息不符: %+v", leaf)
	}
	if !contains(leaf.DNSNames, "web1") || !contains(leaf.DNSNames, "web1.internal") ||
		len(leaf.IPs) != 1 || leaf.IPs[0] != "10.0.0.14" {
		t.Fatalf("叶子 SAN: %v %v", leaf.DNSNames, leaf.IPs)
	}
	if !contains(leaf.ExtKeyUsage, "ServerAuth") || !contains(leaf.KeyUsage, "DigitalSignature") {
		t.Fatalf("叶子用途: %v %v", leaf.ExtKeyUsage, leaf.KeyUsage)
	}
	if leaf.Fingerprint != fp {
		t.Fatalf("指纹应与签发输出一致: %s != %s", leaf.Fingerprint, fp)
	}
}

func contains(list []string, want string) bool {
	return slices.Contains(list, want)
}

// TestLoadCAMismatchedPair 根证书与私钥不属同一 CA 时拒绝签发
// （--ca-cert/--ca-key 指向两套 CA 的产物）。
func TestLoadCAMismatchedPair(t *testing.T) {
	dirA, dirB := t.TempDir(), t.TempDir()
	Init(InitOptions{Dir: dirA})
	Init(InitOptions{Dir: dirB})
	if _, _, err := LoadCAAt(filepath.Join(dirA, DefaultCAFile), filepath.Join(dirB, DefaultKeyFile)); err == nil {
		t.Fatal("证书与私钥不匹配应报错")
	}
}

// TestLoadCARejectsLeafCert --ca-cert 指向叶子证书（非根 CA）时拒绝：
// 典型误用是把"生成的证书"当根证书传入。
func TestLoadCARejectsLeafCert(t *testing.T) {
	dir := t.TempDir()
	Init(InitOptions{Dir: dir})
	leafCrt, _, _, err := Issue(IssueOptions{Dir: dir, SANs: []string{"agent1"}}, "agent1")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadCAAt(leafCrt, filepath.Join(dir, DefaultKeyFile)); err == nil {
		t.Fatal("叶子证书作根 CA 应报错")
	}
}

// TestIssueWithExternalCA CA 与叶子产物分离：CA 在 dirA，证书输出到 dirB（--ca-cert/--ca-key）。
func TestIssueWithExternalCA(t *testing.T) {
	caDir, outDir := t.TempDir(), t.TempDir()
	if _, _, _, err := Init(InitOptions{Dir: caDir}); err != nil {
		t.Fatal(err)
	}
	crt, _, _, err := Issue(IssueOptions{
		Dir:        outDir,
		CACertPath: filepath.Join(caDir, DefaultCAFile),
		CAKeyPath:  filepath.Join(caDir, DefaultKeyFile),
		SANs:       []string{"web-ext"},
	}, "web-ext")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(crt) != outDir {
		t.Fatalf("证书应输出到 %s: %s", outDir, crt)
	}
	leaf, err := parseCertificate(crt)
	if err != nil {
		t.Fatal(err)
	}
	caCert, err := parseCertificate(filepath.Join(caDir, DefaultCAFile))
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(caCert)
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool}); err != nil {
		t.Fatalf("应由外部 CA 签发: %v", err)
	}
}

// TestIssueWithCustomPKCS8RootCA 自定义根证书：openssl 风格 PKCS8 私钥的
// 外部根 CA 不经导入，直接经 --ca-cert/--ca-key 指向即可签发，产物由其验签。
func TestIssueWithCustomPKCS8RootCA(t *testing.T) {
	src, out := t.TempDir(), t.TempDir()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "org-root-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	rootDER, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := writePEM(filepath.Join(src, "org-ca.crt"), "CERTIFICATE", rootDER, 0o644); err != nil {
		t.Fatal(err)
	}
	p8, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := writePEM(filepath.Join(src, "org-ca.key"), "PRIVATE KEY", p8, 0o600); err != nil {
		t.Fatal(err)
	}
	crt, _, _, err := Issue(IssueOptions{
		Dir:        out,
		CACertPath: filepath.Join(src, "org-ca.crt"),
		CAKeyPath:  filepath.Join(src, "org-ca.key"),
		SANs:       []string{"agent-org"},
	}, "agent-org")
	if err != nil {
		t.Fatalf("自制 PKCS8 根 CA 应可直接签发: %v", err)
	}
	leaf, _ := parseCertificate(crt)
	if leaf.Issuer.CommonName != "org-root-ca" {
		t.Fatalf("签发者应为自制根 CA: %v", leaf.Issuer)
	}
	root, _ := parseCertificate(filepath.Join(src, "org-ca.crt"))
	pool := x509.NewCertPool()
	pool.AddCert(root)
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool}); err != nil {
		t.Fatalf("叶子应通过自制根 CA 验签: %v", err)
	}
}

// TestReadKeySkipsParamsBlock openssl ecparam -genkey 产物在私钥块前带
// EC PARAMETERS 块：readKey 逐块扫描跳过非私钥块，自制根 CA 可直接使用。
func TestReadKeySkipsParamsBlock(t *testing.T) {
	dir := t.TempDir()
	if _, _, _, err := Init(InitOptions{Dir: dir}); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(dir, DefaultKeyFile)
	plain, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	withParams := append([]byte("-----BEGIN EC PARAMETERS-----\nBggqhkjOPQMBBw==\n-----END EC PARAMETERS-----\n"), plain...)
	_ = os.WriteFile(keyPath, withParams, 0o600)
	if _, _, err := LoadCAAt(filepath.Join(dir, DefaultCAFile), filepath.Join(dir, DefaultKeyFile)); err != nil {
		t.Fatalf("带 EC PARAMETERS 前导块应可读取: %v", err)
	}
}

// TestIssueNameDefaultsToProfile name 为空时以档案名为证书名：
// --profile peer --san etcd1 → peer.crt/.key、CN=peer；SAN 仍全量生效。
func TestIssueNameDefaultsToProfile(t *testing.T) {
	dir := t.TempDir()
	Init(InitOptions{Dir: dir, Days: 365})
	crt, keyPath, _, err := Issue(IssueOptions{Dir: dir, Profile: ProfilePeer, SANs: []string{"etcd1"}}, "")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(crt) != "peer.crt" || filepath.Base(keyPath) != "peer.key" {
		t.Fatalf("产物应按档案名命名: %s %s", crt, keyPath)
	}
	leaf, _ := parseCertificate(crt)
	if leaf.Subject.CommonName != "peer" {
		t.Fatalf("CN 缺省应为档案名: %s", leaf.Subject.CommonName)
	}
	if len(leaf.DNSNames) != 1 || leaf.DNSNames[0] != "etcd1" {
		t.Fatalf("SAN 应只有显式给出的 etcd1: %v", leaf.DNSNames)
	}
}

// TestIssueServerRequiresSAN server/peer 档案零 SAN 拒绝（TLS 只校验 SAN）；
// client 档案无 SAN 合法。
func TestIssueServerRequiresSAN(t *testing.T) {
	dir := t.TempDir()
	Init(InitOptions{Dir: dir})
	if _, _, _, err := Issue(IssueOptions{Dir: dir}, "no-san"); err == nil {
		t.Fatal("server 档案零 SAN 应报错")
	}
	if _, _, _, err := Issue(IssueOptions{Dir: dir, Profile: ProfilePeer}, "no-san-peer"); err == nil {
		t.Fatal("peer 档案零 SAN 应报错")
	}
	if _, _, _, err := Issue(IssueOptions{Dir: dir, Profile: ProfileClient}, "sanless-client"); err != nil {
		t.Fatalf("client 档案无 SAN 应允许: %v", err)
	}
}

// TestProfilesEKU 用途档案：server/client/peer 的 EKU 组合。
func TestProfilesEKU(t *testing.T) {
	dir := t.TempDir()
	Init(InitOptions{Dir: dir})
	for _, tc := range []struct {
		profile Profile
		want    []x509.ExtKeyUsage
	}{
		{ProfileServer, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}},
		{ProfileClient, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}},
		{ProfilePeer, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}},
	} {
		crt, _, _, err := Issue(IssueOptions{Dir: dir, Profile: tc.profile, SANs: []string{"x.internal"}}, "n-"+string(tc.profile))
		if err != nil {
			t.Fatalf("%s 签发失败: %v", tc.profile, err)
		}
		cert, err := parseCertificate(crt)
		if err != nil {
			t.Fatal(err)
		}
		if len(cert.ExtKeyUsage) != len(tc.want) {
			t.Fatalf("%s EKU 数量: %v", tc.profile, cert.ExtKeyUsage)
		}
		for i, e := range tc.want {
			if cert.ExtKeyUsage[i] != e {
				t.Fatalf("%s EKU[%d]=%d want %d", tc.profile, i, cert.ExtKeyUsage[i], e)
			}
		}
	}
	// 客户端证书携带 SAN 合法（etcd peer 等双向场景）
	if _, _, _, err := Issue(IssueOptions{Dir: dir, SANs: []string{"x.internal"}, Profile: ProfileClient}, "ctl"); err != nil {
		t.Fatalf("客户端证书携带 SAN 应允许: %v", err)
	}
	if _, err := NormalizeProfile("bogus"); err == nil {
		t.Fatal("非法档案应报错")
	}
}

// TestIssueKeyAlgorithms 密钥算法：RSA 2048/3072、Ed25519、ECDSA P-384，
// 均可通过链校验且 renew --new-key 沿用原算法。
func TestIssueKeyAlgorithms(t *testing.T) {
	dir := t.TempDir()
	Init(InitOptions{Dir: dir, Days: 365})
	for _, tc := range []struct {
		spec KeySpec
		name string
	}{
		{KeySpec{Algo: "rsa", Size: 2048}, "rsa-host"},
		{KeySpec{Algo: "rsa", Size: 3072}, "rsa3072"},
		{KeySpec{Algo: "ed25519"}, "ed-host"},
		{KeySpec{Algo: "ecdsa", Size: 384}, "p384-host"},
	} {
		crt, _, _, err := Issue(IssueOptions{Dir: dir, Key: tc.spec, SANs: []string{tc.name}}, tc.name)
		if err != nil {
			t.Fatalf("%s 签发失败: %v", tc.name, err)
		}
		leaf, err := parseCertificate(crt)
		if err != nil {
			t.Fatal(err)
		}
		if pubKeyName(leaf.PublicKey) == "" {
			t.Fatalf("%s 公钥算法未知", tc.name)
		}
		if _, keyPath, _, err := Renew(RenewOptions{
			CertPath:   filepath.Join(dir, tc.name+".crt"),
			OutPath:    filepath.Join(dir, tc.name+".crt"),
			CACertPath: filepath.Join(dir, DefaultCAFile), CAKeyPath: filepath.Join(dir, DefaultKeyFile),
			NewKey: true,
		}); err != nil {
			t.Fatalf("%s renew 失败: %v", tc.name, err)
		} else if _, err := os.Stat(keyPath); err != nil {
			t.Fatal(err)
		}
	}
}

// TestSANURLEmail SAN 自动识别：uri:/email: 前缀与 IP/DNS 裸值；
// name 不进 SAN。
func TestSANURLEmail(t *testing.T) {
	dir := t.TempDir()
	Init(InitOptions{Dir: dir})
	crt, _, _, err := Issue(IssueOptions{Dir: dir, SANs: []string{
		"uri:spiffe://wdp/ns/default/sa/agent",
		"email:ops@example.com",
		"10.0.0.9",
		"web.internal",
	}}, "san-host")
	if err != nil {
		t.Fatal(err)
	}
	cert, err := parseCertificate(crt)
	if err != nil {
		t.Fatal(err)
	}
	if len(cert.URIs) != 1 || cert.URIs[0].String() != "spiffe://wdp/ns/default/sa/agent" {
		t.Fatalf("URI SAN: %v", cert.URIs)
	}
	if len(cert.EmailAddresses) != 1 || cert.EmailAddresses[0] != "ops@example.com" {
		t.Fatalf("Email SAN: %v", cert.EmailAddresses)
	}
	if len(cert.IPAddresses) != 1 {
		t.Fatalf("IP SAN: %v", cert.IPAddresses)
	}
	if len(cert.DNSNames) != 1 || cert.DNSNames[0] != "web.internal" {
		t.Fatalf("DNS SAN 应只有显式 web.internal（name 不进 SAN）: %v", cert.DNSNames)
	}
	if _, _, _, err := Issue(IssueOptions{Dir: dir, SANs: []string{"uri:::"}}, "bad"); err == nil {
		t.Fatal("非法 URI SAN 应报错")
	}
}

// TestSubjectFields 主题字段定制（cfssl CN/O/OU/C/ST/L）与 --cn 覆盖。
func TestSubjectFields(t *testing.T) {
	dir := t.TempDir()
	if _, _, _, err := Init(InitOptions{
		Dir: dir, Days: 365,
		Subject: Subject{CN: "corp-root", O: []string{"Corp"}, OU: []string{"Infra"},
			C: []string{"CN"}, ST: []string{"Beijing"}, L: []string{"Beijing"}},
		Key: KeySpec{Algo: "rsa", Size: 2048},
	}); err != nil {
		t.Fatal(err)
	}
	caCert, _, err := LoadCAAt(filepath.Join(dir, DefaultCAFile), filepath.Join(dir, DefaultKeyFile))
	if err != nil {
		t.Fatal(err)
	}
	if caCert.Subject.CommonName != "corp-root" || caCert.Subject.Organization[0] != "Corp" ||
		caCert.Subject.Country[0] != "CN" || caCert.Subject.Province[0] != "Beijing" || caCert.Subject.Locality[0] != "Beijing" {
		t.Fatalf("CA 主题异常: %v", caCert.Subject)
	}
	// 叶子：OU/C 覆盖、O 缺省 wdp、Subject.CN 覆盖 name 推导的 CN
	crt, _, _, err := Issue(IssueOptions{
		Dir: dir, Subject: Subject{CN: "app1.corp.cn", OU: []string{"Apps"}, C: []string{"US"}},
		SANs: []string{"app1.corp"},
	}, "app1")
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := parseCertificate(crt)
	if leaf.Subject.CommonName != "app1.corp.cn" {
		t.Fatalf("Subject.CN 应覆盖 name: %s", leaf.Subject.CommonName)
	}
	if leaf.Subject.OrganizationalUnit[0] != "Apps" || leaf.Subject.Country[0] != "US" {
		t.Fatalf("叶子主题异常: %v", leaf.Subject)
	}
	if leaf.Subject.Organization[0] != "wdp" {
		t.Fatalf("叶子 O 缺省应为 wdp: %v", leaf.Subject)
	}
}

// TestVerifyKeyPair 证书/私钥配对校验：同对通过；交叉错配拒绝；
// 覆盖默认 ed25519 与 rsa 两种密钥。
func TestVerifyKeyPair(t *testing.T) {
	dir := t.TempDir()
	Init(InitOptions{Dir: dir, Days: 365})
	crtA, keyA, _, err := Issue(IssueOptions{Dir: dir, SANs: []string{"a.internal"}}, "a")
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyKeyPair(crtA, keyA); err != nil {
		t.Fatalf("同对应配对: %v", err)
	}
	crtB, keyB, _, err := Issue(IssueOptions{Dir: dir, SANs: []string{"b.internal"}}, "b")
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyKeyPair(crtA, keyB); err == nil {
		t.Fatal("错对应拒绝")
	}
	if err := VerifyKeyPair(crtB, keyA); err == nil {
		t.Fatal("错对应拒绝（反向）")
	}
	// RSA 密钥对同样校验
	crtR, keyR, _, err := Issue(IssueOptions{Dir: dir, Key: KeySpec{Algo: "rsa", Size: 2048}, SANs: []string{"r.internal"}}, "r")
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyKeyPair(crtR, keyR); err != nil {
		t.Fatalf("RSA 同对应配对: %v", err)
	}
	if err := VerifyKeyPair(crtR, keyA); err == nil {
		t.Fatal("RSA/Ed25519 跨算法错对应拒绝")
	}
}

// TestSANWildcard 通配符域名 SAN：*.example.com 作为 DNS SAN 签出后，
// 按标准库语义匹配——单标签子域命中；裸域与多标签子域不命中；
// 部分通配（web*）不是通配符（按字面量比较，永远不匹配）。
func TestSANWildcard(t *testing.T) {
	dir := t.TempDir()
	Init(InitOptions{Dir: dir})
	crt, _, _, err := Issue(IssueOptions{Dir: dir, SANs: []string{
		"*.example.com", "example.com",
	}}, "wild")
	if err != nil {
		t.Fatal(err)
	}
	cert, err := parseCertificate(crt)
	if err != nil {
		t.Fatal(err)
	}
	if len(cert.DNSNames) != 2 {
		t.Fatalf("通配符应作为普通 DNS SAN 保存: %v", cert.DNSNames)
	}
	for _, tc := range []struct {
		host string
		want bool
	}{
		{"foo.example.com", true},  // 单标签子域命中
		{"example.com", true},      // 裸域（显式给出的 SAN）
		{"a.b.example.com", false}, // 多标签子域不命中（标签数不等）
		{"other.com", false},       // 无关域
	} {
		if err := cert.VerifyHostname(tc.host); (err == nil) != tc.want {
			t.Fatalf("VerifyHostname(%s) = %v, want %v", tc.host, err, tc.want)
		}
	}
	// 仅通配符（无裸域）时裸域不命中
	crt2, _, _, err := Issue(IssueOptions{Dir: dir, SANs: []string{"*.only-wild.io"}}, "w2")
	if err != nil {
		t.Fatal(err)
	}
	c2, _ := parseCertificate(crt2)
	if err := c2.VerifyHostname("only-wild.io"); err == nil {
		t.Fatal("*.only-wild.io 不应匹配裸域 only-wild.io")
	}
	// 部分通配按字面量处理：web*.x.io 不匹配 web1.x.io
	crt3, _, _, err := Issue(IssueOptions{Dir: dir, SANs: []string{"web*.x.io"}}, "w3")
	if err != nil {
		t.Fatal(err)
	}
	c3, _ := parseCertificate(crt3)
	if err := c3.VerifyHostname("web1.x.io"); err == nil {
		t.Fatal("部分通配 web*.x.io 应按字面量比较，不匹配 web1.x.io")
	}
}

// TestRenewDeltaSemantics --days 是增量：新到期 = 原到期 + N 天；
// 备份件可解析且即旧证书本体。
func TestRenewDeltaSemantics(t *testing.T) {
	dir := t.TempDir()
	Init(InitOptions{Dir: dir, Days: 365})
	crt, _, _, err := Issue(IssueOptions{Dir: dir, SANs: []string{"d1"}}, "d1")
	if err != nil {
		t.Fatal(err)
	}
	old, _ := parseCertificate(crt)

	if _, _, _, err := Renew(RenewOptions{
		CertPath: crt, KeyPath: filepath.Join(dir, "d1.key"), OutPath: crt, Days: 30,
	}); err != nil {
		t.Fatal(err)
	}
	fresh, err := parseCertificate(crt) // 原路径已是新证书
	if err != nil {
		t.Fatal(err)
	}
	want := old.NotAfter.AddDate(0, 0, 30)
	if fresh.NotAfter.Sub(want) > 2*time.Hour || want.Sub(fresh.NotAfter) > 2*time.Hour {
		t.Fatalf("新到期应 = 原+30 天: got %v want %v", fresh.NotAfter, want)
	}
	// 备份件即旧证书
	backup := ""
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "d1.crt.old.") {
			backup = filepath.Join(dir, e.Name())
		}
	}
	if backup == "" {
		t.Fatal("应存在 d1.crt.old.<ts> 备份")
	}
	bc, err := parseCertificate(backup)
	if err != nil {
		t.Fatalf("备份应可解析: %v", err)
	}
	if !bc.NotAfter.Equal(old.NotAfter) {
		t.Fatal("备份内容应为旧证书")
	}
}

// TestRenewClampedToCA 增量超出 CA 到期时钳制到 CA 到期时刻
// （延期语义下的硬规则与 issue 一致）。
func TestRenewClampedToCA(t *testing.T) {
	dir := t.TempDir()
	Init(InitOptions{Dir: dir, Days: 365})
	crt, _, _, err := Issue(IssueOptions{Dir: dir, SANs: []string{"e1"}, Days: 1}, "e1")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := Renew(RenewOptions{
		CertPath: crt, KeyPath: filepath.Join(dir, "e1.key"), OutPath: crt, Days: 3650,
	}); err != nil {
		t.Fatalf("超 CA 增量应钳制而非报错: %v", err)
	}
	fresh, _ := parseCertificate(crt)
	caCert, _, _ := LoadCAAt(filepath.Join(dir, DefaultCAFile), filepath.Join(dir, DefaultKeyFile))
	if !fresh.NotAfter.Equal(caCert.NotAfter) {
		t.Fatalf("应钳制到 CA 到期: %v != %v", fresh.NotAfter, caCert.NotAfter)
	}
}

// TestRenewExplicitOutDir 输出到别的目录：旧件原地不动（无备份），
// 新证书落在指定路径；--key 缺省（保留模式）与钥匙不配对都拒绝。
func TestRenewExplicitOutDir(t *testing.T) {
	dir, outDir := t.TempDir(), t.TempDir()
	Init(InitOptions{Dir: dir, Days: 365})
	crt, keyPath, _, err := Issue(IssueOptions{Dir: dir, SANs: []string{"o1"}}, "o1")
	if err != nil {
		t.Fatal(err)
	}
	oldKey, _ := os.ReadFile(keyPath)
	oldCrt, _ := os.ReadFile(crt)

	newCrt, newKey, _, err := Renew(RenewOptions{
		CertPath: crt, KeyPath: keyPath,
		OutPath:    filepath.Join(outDir, "o1.crt"),
		CACertPath: filepath.Join(dir, DefaultCAFile), CAKeyPath: filepath.Join(dir, DefaultKeyFile),
		Days: 30,
	})
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(newCrt) != outDir || filepath.Dir(newKey) != outDir {
		t.Fatalf("新产物应输出到 %s: %s %s", outDir, newCrt, newKey)
	}
	// 旧件原地不动、无备份
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".old.") {
			t.Fatalf("输出到别处不应备份旧件: %s", e.Name())
		}
	}
	got, _ := os.ReadFile(crt)
	if string(got) != string(oldCrt) {
		t.Fatal("旧证书不应被改动")
	}
	gotKey, _ := os.ReadFile(keyPath)
	if string(gotKey) != string(oldKey) {
		t.Fatal("旧私钥不应被改动")
	}

	// 保留模式缺 --key → 拒绝
	if _, _, _, err := Renew(RenewOptions{
		CertPath: crt, OutPath: filepath.Join(outDir, "x.crt"),
		CACertPath: filepath.Join(dir, DefaultCAFile), CAKeyPath: filepath.Join(dir, DefaultKeyFile),
		Days: 30,
	}); err == nil {
		t.Fatal("保留模式缺 --key 应报错")
	}
	// 钥匙不配对 → 拒绝（拿 CA 的钥匙来续叶子）
	_, caKeyPath, _, err := Issue(IssueOptions{Dir: dir, SANs: []string{"other"}}, "other")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := Renew(RenewOptions{
		CertPath: crt, KeyPath: caKeyPath,
		OutPath:    filepath.Join(outDir, "y.crt"),
		CACertPath: filepath.Join(dir, DefaultCAFile), CAKeyPath: filepath.Join(dir, DefaultKeyFile),
		Days: 30,
	}); err == nil {
		t.Fatal("钥匙与证书不配对应报错")
	}
}
