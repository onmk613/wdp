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
	"strings"
	"testing"
	"time"
)

func TestInitEncryptedKey(t *testing.T) {
	dir := t.TempDir()
	crt, key, _, err := Init(dir, "s3cret")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(crt); err != nil {
		t.Fatal(err)
	}
	// 私钥应为加密信封（非明文 EC PRIVATE KEY）
	pemBytes, _ := os.ReadFile(key)
	block, _ := pem.Decode(pemBytes)
	if block == nil || block.Type != "WDP ENCRYPTED EC PRIVATE KEY" {
		t.Fatalf("私钥未加密: %s", block.Type)
	}

	// 无口令加载 → 报错
	if _, _, err := LoadCA(dir, ""); err == nil {
		t.Fatal("加密私钥无口令应报错")
	}
	// 错误口令 → 报错
	if _, _, err := LoadCA(dir, "wrong"); err == nil {
		t.Fatal("错误口令应报错")
	}
	// 正确口令 → 成功签发
	cert, caKey, err := LoadCA(dir, "s3cret")
	if err != nil {
		t.Fatal(err)
	}
	if caKey == nil || !cert.IsCA {
		t.Fatal("CA 加载异常")
	}
	// 环境变量口令
	t.Setenv(PassphraseEnv, "s3cret")
	if _, _, err := LoadCA(dir, ""); err != nil {
		t.Fatalf("环境变量口令应生效: %v", err)
	}
}

func TestCAPathLenZero(t *testing.T) {
	dir := t.TempDir()
	_, _, _, err := Init(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	cert, _, err := LoadCA(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if !cert.MaxPathLenZero || cert.MaxPathLen != 0 {
		t.Fatalf("CA 应禁止签发中间 CA: MaxPathLen=%d", cert.MaxPathLen)
	}
}

func TestIssueShortValidityAndFingerprint(t *testing.T) {
	dir := t.TempDir()
	Init(dir, "")
	crt, _, fp, err := Issue(IssueOptions{Dir: dir, Days: 30}, "10.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if len(fp) != len("sha256:")+64 {
		t.Fatalf("指纹格式: %q", fp)
	}
	// 默认有效期 30 天
	_, _, fp2, err := Issue(IssueOptions{Dir: dir}, "host1")
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
	Init(dir, "")
	crt, keyPath, _, err := Issue(IssueOptions{Dir: dir, Days: 1}, "agent1")
	if err != nil {
		t.Fatal(err)
	}
	oldPEM, _ := os.ReadFile(crt)
	ob, _ := pem.Decode(oldPEM)
	old, _ := x509.ParseCertificate(ob.Bytes)
	oldKey, _ := os.ReadFile(keyPath)

	// 过期后 renew（保留私钥）
	time.Sleep(1100 * time.Millisecond) // NotBefore=now-1h, NotAfter=now+1s 已过期
	newCrt, newKeyPath, _, err := Renew(RenewOptions{Dir: dir, Days: 90}, "agent1")
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
	if !renewed.NotAfter.After(time.Now().Add(89 * 24 * time.Hour)) {
		t.Fatalf("续期后有效期异常: %v", renewed.NotAfter)
	}
	// 私钥保留（文件内容一致）
	newKey, _ := os.ReadFile(newKeyPath)
	if string(newKey) != string(oldKey) {
		t.Fatal("默认应保留原私钥")
	}
}

func TestRenewNewKey(t *testing.T) {
	dir := t.TempDir()
	Init(dir, "")
	_, keyPath, _, _ := Issue(IssueOptions{Dir: dir}, "svc")
	oldKey, _ := os.ReadFile(keyPath)
	if _, newKeyPath, _, err := Renew(RenewOptions{Dir: dir, NewKey: true}, "svc"); err != nil {
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
	for i := 0; i < n; i++ {
		s += "a"
	}
	return s
}

func TestIssueMultiSAN(t *testing.T) {
	dir := t.TempDir()
	Init(dir, "")
	// name 是域名；附加 IP 与域名（含与 name 重复项与空串，应去重/忽略）
	crt, _, _, err := Issue(IssueOptions{Dir: dir, SANs: []string{"10.0.0.14", "web1.internal", "web1", ""}}, "web1")
	if err != nil {
		t.Fatal(err)
	}
	pemBytes, _ := os.ReadFile(crt)
	block, _ := pem.Decode(pemBytes)
	cert, _ := x509.ParseCertificate(block.Bytes)

	hasDNS := func(want string) bool {
		for _, n := range cert.DNSNames {
			if n == want {
				return true
			}
		}
		return false
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

func TestIssueSANRejectedForClient(t *testing.T) {
	dir := t.TempDir()
	Init(dir, "")
	if _, _, _, err := Issue(IssueOptions{Dir: dir, SANs: []string{"x.internal"}, Client: true}, "ctl"); err == nil {
		t.Fatal("客户端证书携带 SAN 应报错（SAN 仅用于服务端证书）")
	}
}

func TestRenewPreservesMultiSAN(t *testing.T) {
	dir := t.TempDir()
	Init(dir, "")
	crt, _, _, err := Issue(IssueOptions{Dir: dir, SANs: []string{"10.0.0.14", "web1.internal"}, Days: 1}, "web1")
	if err != nil {
		t.Fatal(err)
	}
	newCrt, _, _, err := Renew(RenewOptions{Dir: dir, Days: 90}, "web1")
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

// TestPassphraseFromEnv 环境变量口令在写入时同样生效（init/import 落盘加密，
// 而非仅读取时解密）；环境变量与显式参数同时存在时显式优先。
func TestPassphraseFromEnv(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(PassphraseEnv, "envpass")
	_, keyPath, _, err := Init(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	keyPEM, _ := os.ReadFile(keyPath)
	block, _ := pem.Decode(keyPEM)
	if block == nil || block.Type != encPEMType {
		t.Fatalf("环境变量口令应使私钥加密落盘: %v", block.Type)
	}
	// 不带口令（环境变量已设）可直接加载
	if _, _, err := LoadCA(dir, ""); err != nil {
		t.Fatalf("环境变量口令应可解密: %v", err)
	}
	// 显式口令优先于环境变量
	dir2 := t.TempDir()
	if _, _, _, err := Init(dir2, "flagpass"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadCA(dir2, "flagpass"); err != nil {
		t.Fatal(err)
	}

	// import：源为明文私钥时，环境变量同样加密落盘副本
	src, dst := t.TempDir(), t.TempDir()
	if _, _, _, err := Init(src, ""); err != nil {
		t.Fatal(err)
	}
	if _, keyDst, _, err := Import(dst, filepath.Join(src, CAFile), filepath.Join(src, KeyFile), ""); err != nil {
		t.Fatal(err)
	} else {
		keyPEM, _ := os.ReadFile(keyDst)
		b, _ := pem.Decode(keyPEM)
		if b == nil || b.Type != encPEMType {
			t.Fatalf("import 落盘副本应按环境变量加密: %v", b.Type)
		}
	}
}

// TestInspect CA 与叶子证书的信息解析（ca show 的域逻辑）。
func TestInspect(t *testing.T) {
	dir := t.TempDir()
	Init(dir, "")

	caInfo, err := Inspect(filepath.Join(dir, CAFile))
	if err != nil {
		t.Fatal(err)
	}
	if !caInfo.IsCA || !caInfo.SelfSigned || !caInfo.PathLenZero {
		t.Fatalf("CA 信息不符: %+v", caInfo)
	}
	if !strings.HasPrefix(caInfo.Fingerprint, "sha256:") || caInfo.PublicKey != "ECDSA P-256" {
		t.Fatalf("指纹/公钥: %q %q", caInfo.Fingerprint, caInfo.PublicKey)
	}
	// CA 密钥用途含 CertSign，且不含 SAN/EKU 服务端字段
	if !contains(caInfo.KeyUsage, "CertSign") || len(caInfo.ExtKeyUsage) != 0 || len(caInfo.DNSNames) != 0 {
		t.Fatalf("CA 用途/SAN: %v %v %v", caInfo.KeyUsage, caInfo.ExtKeyUsage, caInfo.DNSNames)
	}

	crt, _, fp, err := Issue(IssueOptions{Dir: dir, SANs: []string{"10.0.0.14", "web1.internal"}}, "web1")
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
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// TestImportCA 二次分发：CA 导入到新目录后指纹一致、可正常签发，叶子证书能通过导入 CA 验签。
func TestImportCA(t *testing.T) {
	srcDir, dstDir := t.TempDir(), t.TempDir()
	_, _, fpSrc, err := Init(srcDir, "s3cret")
	if err != nil {
		t.Fatal(err)
	}
	caPath, keyPath, fpDst, err := Import(dstDir,
		filepath.Join(srcDir, CAFile), filepath.Join(srcDir, KeyFile), "s3cret")
	if err != nil {
		t.Fatal(err)
	}
	if fpSrc != fpDst {
		t.Fatalf("导入后指纹应一致: %s != %s", fpSrc, fpDst)
	}
	// 导入副本同样以口令加密
	keyPEM, _ := os.ReadFile(keyPath)
	block, _ := pem.Decode(keyPEM)
	if block == nil || block.Type != encPEMType {
		t.Fatalf("导入私钥应加密: %v", block.Type)
	}
	// 导入的 CA 可签发，且叶子证书通过其验签
	crt, _, _, err := Issue(IssueOptions{Dir: dstDir, Passphrase: "s3cret"}, "agent-imported")
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := parseCertificate(crt)
	if err != nil {
		t.Fatal(err)
	}
	impCA, err := parseCertificate(caPath)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(impCA)
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
		t.Fatalf("叶子证书应通过导入 CA 验签: %v", err)
	}
}

// TestImportMismatchedPair 证书与私钥不属同一 CA 时拒绝导入。
func TestImportMismatchedPair(t *testing.T) {
	dirA, dirB, dst := t.TempDir(), t.TempDir(), t.TempDir()
	Init(dirA, "")
	Init(dirB, "")
	if _, _, _, err := Import(dst,
		filepath.Join(dirA, CAFile), filepath.Join(dirB, KeyFile), ""); err == nil {
		t.Fatal("证书与私钥不匹配应报错")
	}
	// 导入失败不落盘
	if _, err := os.Stat(filepath.Join(dst, CAFile)); !os.IsNotExist(err) {
		t.Fatal("失败的导入不应写入 ca.crt")
	}
}

// TestImportRejectsLeafCert 非 CA 证书（叶子证书）拒绝导入。
func TestImportRejectsLeafCert(t *testing.T) {
	dir, dst := t.TempDir(), t.TempDir()
	Init(dir, "")
	leafCrt, _, _, err := Issue(IssueOptions{Dir: dir}, "agent1")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := Import(dst, leafCrt, filepath.Join(dir, KeyFile), ""); err == nil {
		t.Fatal("叶子证书应拒绝导入")
	}
}

// TestIssueWithExternalCA CA 与叶子产物分离：CA 在 dirA，证书输出到 dirB（--ca-cert/--ca-key）。
func TestIssueWithExternalCA(t *testing.T) {
	caDir, outDir := t.TempDir(), t.TempDir()
	if _, _, _, err := Init(caDir, ""); err != nil {
		t.Fatal(err)
	}
	crt, _, _, err := Issue(IssueOptions{
		Dir:        outDir,
		CACertPath: filepath.Join(caDir, CAFile),
		CAKeyPath:  filepath.Join(caDir, KeyFile),
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
	caCert, err := parseCertificate(filepath.Join(caDir, CAFile))
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(caCert)
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool}); err != nil {
		t.Fatalf("应由外部 CA 签发: %v", err)
	}
}

// TestImportPKCS8CA 外部组织 CA（openssl 风格 PKCS8 私钥）导入后可签发。
func TestImportPKCS8CA(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "org-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := writePEM(filepath.Join(src, "org-ca.crt"), "CERTIFICATE", der, 0o644); err != nil {
		t.Fatal(err)
	}
	p8, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := writePEM(filepath.Join(src, "org-ca.key"), "PRIVATE KEY", p8, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := Import(dst,
		filepath.Join(src, "org-ca.crt"), filepath.Join(src, "org-ca.key"), ""); err != nil {
		t.Fatalf("PKCS8 CA 应可导入: %v", err)
	}
	crt, _, _, err := Issue(IssueOptions{Dir: dst}, "agent-org")
	if err != nil {
		t.Fatalf("导入的组织 CA 应可签发: %v", err)
	}
	leaf, _ := parseCertificate(crt)
	if leaf.Issuer.CommonName != "org-ca" {
		t.Fatalf("签发者应为导入的组织 CA: %v", leaf.Issuer)
	}
}
