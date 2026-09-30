package ca

// CSR 链路测试：ParseCSR 的验签与算法白名单、SignCSR 的身份由调用方
// 决定、GenCSR 的幂等复用、Keyless 续期不依赖也不产生叶子私钥。

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
)

// mkCSR 生成一把钥匙与对应 CSR PEM（带可选模板定制）。
func mkCSR(t *testing.T, key crypto.Signer, tpl *x509.CertificateRequest) []byte {
	t.Helper()
	if tpl == nil {
		tpl = &x509.CertificateRequest{}
	}
	tpl.PublicKey = key.Public()
	der, err := x509.CreateCertificateRequest(rand.Reader, tpl, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})
}

func ed25519Key(t *testing.T) crypto.Signer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return priv
}

// testCA 临时自举一套 CA（签发测试用）。
func testCA(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if _, _, _, err := Init(InitOptions{Dir: dir, Days: 30}); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestParseCSRVerifiesAndWhitelists(t *testing.T) {
	if _, err := ParseCSR([]byte("not pem")); err == nil {
		t.Fatal("非 PEM 应拒绝")
	}

	// 合法 ed25519
	if _, err := ParseCSR(mkCSR(t, ed25519Key(t), nil)); err != nil {
		t.Fatalf("合法 ed25519 CSR 应通过: %v", err)
	}
	// 合法 ECDSA P-256
	ec, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseCSR(mkCSR(t, ec, nil)); err != nil {
		t.Fatalf("合法 ECDSA P-256 CSR 应通过: %v", err)
	}
	// RSA 不在白名单
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseCSR(mkCSR(t, rsaKey, nil)); err == nil {
		t.Fatal("RSA CSR 应拒绝（算法白名单）")
	}
	// 自签名不成立：对合法 CSR 的签名字节做翻转（解析仍完整，验签必败
	// ——Go 的 CreateCertificateRequest 会忽略模板 PublicKey，构造
	// “公钥 A + 钥匙 B 签名”走不通，篡改签名是等价攻击面）
	badDER := func() []byte {
		t.Helper()
		der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, ed25519Key(t))
		if err != nil {
			t.Fatal(err)
		}
		der[len(der)-2] ^= 0xFF // 签名 BIT STRING 尾部字节
		return der
	}()
	bad := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: badDER})
	if _, err := ParseCSR(bad); err == nil {
		t.Fatal("签名不匹配的 CSR 应拒绝（提交方不持私钥）")
	}
}

func TestSignCSRIdentityFromCallerAndNoKeyFile(t *testing.T) {
	dir := testCA(t)
	key := ed25519Key(t)
	// CSR 自报的身份（CN/SAN）必须被忽略——签名方只信调用方给的 SANs
	csrPEM := mkCSR(t, key, &x509.CertificateRequest{
		Subject:  pkix.Name{CommonName: "evil"},
		DNSNames: []string{"evil.example.com"},
	})
	csr, err := ParseCSR(csrPEM)
	if err != nil {
		t.Fatal(err)
	}
	crtPath, _, err := SignCSR(SignCSROptions{
		Dir: filepath.Join(dir, "hosts"), CACertPath: filepath.Join(dir, "ca.crt"),
		CAKeyPath: filepath.Join(dir, "ca.key"), SANs: []string{"10.1.2.3", "web1"},
		Profile: ProfileServer, Days: 7,
	}, csr, "web1")
	if err != nil {
		t.Fatal(err)
	}
	cert := parseCertAt(t, crtPath)
	if cert.Subject.CommonName != "web1" {
		t.Fatalf("CN 应取 name 参数: %q", cert.Subject.CommonName)
	}
	if len(cert.DNSNames) != 1 || cert.DNSNames[0] != "web1" || len(cert.IPAddresses) != 1 || cert.IPAddresses[0].String() != "10.1.2.3" {
		t.Fatalf("SAN 应来自调用方而非 CSR: %v %v", cert.DNSNames, cert.IPAddresses)
	}
	if cert.DNSNames[0] == "evil.example.com" {
		t.Fatal("CSR 自报 SAN 泄入证书")
	}
	// 证书公钥 == CSR 公钥
	want, _ := PubkeySHA(key.Public())
	got, _ := PubkeySHA(cert.PublicKey)
	if want != got {
		t.Fatal("证书公钥应与 CSR 一致")
	}
	// 只写 .crt：签发方无私钥可写
	if _, err := os.Stat(crtPath + ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "hosts", "web1.key")); !os.IsNotExist(err) {
		t.Fatalf("CSR 签发不得产生私钥文件: %v", err)
	}
}

func TestGenCSRIdempotent(t *testing.T) {
	dir := t.TempDir()
	keyPath, csrPath := filepath.Join(dir, "agent.key"), filepath.Join(dir, "agent.csr")

	if reused, err := GenCSR(keyPath, csrPath); err != nil || reused {
		t.Fatalf("首次生成: reused=%v err=%v", reused, err)
	}
	first := readCSRPubkey(t, csrPath)
	st, err := os.Stat(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("私钥权限应为 0600: %v", st.Mode().Perm())
	}

	// 复跑：复用既有私钥（重装不换身份）
	if reused, err := GenCSR(keyPath, csrPath); err != nil || !reused {
		t.Fatalf("二次生成应复用: reused=%v err=%v", reused, err)
	}
	if second := readCSRPubkey(t, csrPath); first != second {
		t.Fatal("复用私钥时 CSR 公钥不得变化")
	}

	// 不可解析的既有文件：显式报错，不静默覆盖
	if err := os.WriteFile(keyPath, []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := GenCSR(keyPath, csrPath); err == nil {
		t.Fatal("坏私钥应报错而非覆盖")
	}
}

func TestRenewKeyless(t *testing.T) {
	dir := testCA(t)
	// 正常 Issue 出一对（模拟旧流程留下的证书+key）
	if _, _, _, err := Issue(IssueOptions{
		Dir: dir, SANs: []string{"h1"}, Profile: ProfileServer, Days: 5,
	}, "h1"); err != nil {
		t.Fatal(err)
	}
	// 删掉 server 侧私钥（CSR 纳管后 server 不再持有）
	if err := os.Remove(filepath.Join(dir, "h1.key")); err != nil {
		t.Fatal(err)
	}
	before := parseCertAt(t, filepath.Join(dir, "h1.crt"))

	newCrt, newKey, _, err := Renew(RenewOptions{
		CertPath: filepath.Join(dir, "h1.crt"), OutPath: filepath.Join(dir, "h1.crt"),
		CACertPath: filepath.Join(dir, "ca.crt"), CAKeyPath: filepath.Join(dir, "ca.key"),
		Days: 5, Keyless: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if newKey != "" {
		t.Fatalf("无钥续期不应产出私钥路径: %q", newKey)
	}
	fresh := parseCertAt(t, newCrt)
	if !fresh.NotAfter.After(before.NotAfter) {
		t.Fatal("续期应延长有效期")
	}
	if len(fresh.DNSNames) != 1 || fresh.DNSNames[0] != "h1" {
		t.Fatalf("SAN 应继承: %v", fresh.DNSNames)
	}
	// 公钥不变（agent 侧私钥仍然配对）
	o, _ := PubkeySHA(before.PublicKey)
	n, _ := PubkeySHA(fresh.PublicKey)
	if o != n {
		t.Fatal("无钥续期不得更换公钥")
	}
	if _, err := os.Stat(filepath.Join(dir, "h1.key")); !os.IsNotExist(err) {
		t.Fatal("无钥续期不得产生私钥文件")
	}
	// 互斥校验
	if _, _, _, err := Renew(RenewOptions{CertPath: newCrt, KeyPath: "x", Keyless: true}); err == nil {
		t.Fatal("Keyless 与 KeyPath 组合应拒绝")
	}
}

// ---- 测试助手 ----

func readCSRPubkey(t *testing.T, csrPath string) string {
	t.Helper()
	b, err := os.ReadFile(csrPath)
	if err != nil {
		t.Fatal(err)
	}
	csr, err := ParseCSR(b)
	if err != nil {
		t.Fatal(err)
	}
	sha, _ := PubkeySHA(csr.PublicKey)
	return sha
}

func parseCertAt(t *testing.T, path string) *x509.Certificate {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	blk, _ := pem.Decode(b)
	if blk == nil {
		t.Fatal("not pem")
	}
	cert, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}
