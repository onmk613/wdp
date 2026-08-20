package ca

import (
	"crypto/x509"
	"encoding/pem"
	"os"
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
	crt, _, fp, err := Issue(dir, "10.0.0.1", nil, false, 30)
	if err != nil {
		t.Fatal(err)
	}
	if len(fp) != len("sha256:")+64 {
		t.Fatalf("指纹格式: %q", fp)
	}
	// 默认有效期 90 天
	_, _, fp2, err := Issue(dir, "host1", nil, false, 0)
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
	crt, keyPath, _, err := Issue(dir, "agent1", nil, false, 1)
	if err != nil {
		t.Fatal(err)
	}
	oldPEM, _ := os.ReadFile(crt)
	ob, _ := pem.Decode(oldPEM)
	old, _ := x509.ParseCertificate(ob.Bytes)
	oldKey, _ := os.ReadFile(keyPath)

	// 过期后 renew（保留私钥）
	time.Sleep(1100 * time.Millisecond) // NotBefore=now-1h, NotAfter=now+1s 已过期
	newCrt, newKeyPath, _, err := Renew(dir, "agent1", false, 90)
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
	_, keyPath, _, _ := Issue(dir, "svc", nil, false, 0)
	oldKey, _ := os.ReadFile(keyPath)
	if _, newKeyPath, _, err := Renew(dir, "svc", true, 0); err != nil {
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
	crt, _, _, err := Issue(dir, "web1", []string{"10.0.0.14", "web1.internal", "web1", ""}, false, 0)
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
	if _, _, _, err := Issue(dir, "ctl", []string{"x.internal"}, true, 0); err == nil {
		t.Fatal("客户端证书携带 SAN 应报错（SAN 仅用于服务端证书）")
	}
}

func TestRenewPreservesMultiSAN(t *testing.T) {
	dir := t.TempDir()
	Init(dir, "")
	crt, _, _, err := Issue(dir, "web1", []string{"10.0.0.14", "web1.internal"}, false, 1)
	if err != nil {
		t.Fatal(err)
	}
	newCrt, _, _, err := Renew(dir, "web1", false, 90)
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
