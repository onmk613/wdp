package web

// 控制端客户端证书（ctl）生命周期回归：
//   - 默认自动档只有 30 天，到期后 server→agent 的全部 mTLS 连接失败，
//     现场表现为"整片 agent 不可达"，只能靠重建 CA 恢复。现在显式签发
//     365 天并在剩余不足 30 天时自动续期；
//   - 续期保留密钥对，因此写进 agent 单元的公钥（SPKI）pin 指纹依然匹配
//     ——这是"pin 控制端证书"与"自动续期"能同时成立的前提。

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"wdp/internal/ca"
	"wdp/internal/store"
)

func TestCtlCertAutoRenewed(t *testing.T) {
	dir := t.TempDir()
	caDir := filepath.Join(dir, "ca")
	if _, _, _, err := ca.Init(ca.InitOptions{Dir: caDir, Days: 3650}); err != nil {
		t.Fatal(err)
	}
	// 造一张"即将到期"的 ctl 证书（保留初始密钥对由 Renew 负责）
	ctlCert, ctlKey := filepath.Join(caDir, "ctl.crt"), filepath.Join(caDir, "ctl.key")
	if _, _, _, err := ca.Issue(ca.IssueOptions{Dir: caDir, Profile: ca.ProfileClient, Days: 2}, "ctl"); err != nil {
		t.Fatal(err)
	}
	before, err := ca.Inspect(ctlCert)
	if err != nil {
		t.Fatal(err)
	}

	st, err := store.Open(filepath.Join(dir, "wdp.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.CreateUser("admin", "$2a$10$x", "admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := New(st, Options{AdminUser: "admin", CADir: caDir}, nil); err != nil {
		t.Fatal(err)
	}

	after, err := ca.Inspect(ctlCert)
	if err != nil {
		t.Fatal(err)
	}
	if !after.NotAfter.After(before.NotAfter) {
		t.Fatalf("临期 ctl 证书应被自动续期: before=%s after=%s", before.NotAfter, after.NotAfter)
	}
	if time.Until(after.NotAfter) < 300*24*time.Hour {
		t.Fatalf("续期后应有较长有效期: %s", after.NotAfter)
	}
	// 续期保留密钥对（否则 agent 的 SPKI pin 会失效）
	if err := ca.VerifyKeyPair(ctlCert, ctlKey); err != nil {
		t.Fatalf("续期应保留原密钥对: %v", err)
	}
}

// TestClientPinsSurviveRenewal 证书 DER 指纹会随续期变化，公钥指纹不会：
// agent 的准许名单同时收录两者，续期后仍能匹配。
func TestClientPinsSurviveRenewal(t *testing.T) {
	caDir := t.TempDir()
	if _, _, _, err := ca.Init(ca.InitOptions{Dir: caDir, Days: 3650}); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := ca.Issue(ca.IssueOptions{Dir: caDir, Profile: ca.ProfileClient, Days: 2}, "ctl"); err != nil {
		t.Fatal(err)
	}
	ctl := filepath.Join(caDir, "ctl.crt")
	certFP1, err := ca.FingerprintFile(ctl)
	if err != nil {
		t.Fatal(err)
	}
	spkiFP1, err := ca.FingerprintPublicKeyFile(ctl)
	if err != nil {
		t.Fatal(err)
	}

	if _, _, _, err := ca.Renew(ca.RenewOptions{
		CertPath: ctl, KeyPath: filepath.Join(caDir, "ctl.key"),
		CACertPath: filepath.Join(caDir, "ca.crt"), CAKeyPath: filepath.Join(caDir, "ca.key"),
		OutPath: ctl, Days: 365, // OutPath 必须显式给，否则新件落到进程 CWD
	}); err != nil {
		t.Fatal(err)
	}
	certFP2, _ := ca.FingerprintFile(ctl)
	spkiFP2, _ := ca.FingerprintPublicKeyFile(ctl)

	if certFP1 == certFP2 {
		t.Fatal("续期后证书指纹应变化（这正是需要 SPKI pin 的原因）")
	}
	if spkiFP1 != spkiFP2 {
		t.Fatalf("公钥指纹应跨续期不变: %s vs %s", spkiFP1, spkiFP2)
	}
	if !strings.HasPrefix(spkiFP1, "sha256:") {
		t.Fatalf("公钥指纹格式: %s", spkiFP1)
	}
}
