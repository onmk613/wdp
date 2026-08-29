package sshc

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ed25519"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// 回归：@revoked 吊销必须全局生效——逐行容错加载不得让普通行的放行
// 短路吊销行（对齐整文件 knownhosts.New 的语义：吊销先于主机匹配）。
func TestTolerantKnownHostsRevocation(t *testing.T) {
	_, normalKey, _ := ed25519.GenerateKey(nil)
	pub1, _ := ssh.NewPublicKey(normalKey.Public())
	_, otherKey, _ := ed25519.GenerateKey(nil)
	pub2, _ := ssh.NewPublicKey(otherKey.Public())

	normalLine := knownhosts.Line([]string{"revoked-host"}, pub1)
	revokedLine := "@revoked * " + string(ssh.MarshalAuthorizedKey(pub1))
	dir := t.TempDir()
	kh := filepath.Join(dir, "known_hosts")
	if err := os.WriteFile(kh, []byte(normalLine+"\n"+revokedLine+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// 基准：整文件加载（x/crypto 原生语义）必须拒绝被吊销密钥
	strict, err := knownhosts.New(kh)
	if err != nil {
		t.Fatal(err)
	}
	addr := &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 22}
	if err := strict("revoked-host", addr, pub1); err == nil || !strings.Contains(err.Error(), "revoked") {
		t.Fatalf("strict knownhosts.New should reject the revoked key, got %v", err)
	}

	cb, err := tolerantKnownHosts(kh)
	if err != nil {
		t.Fatal(err)
	}
	// 同一密钥虽有普通行记录，也必须被吊销拒绝
	if err := cb("revoked-host", addr, pub1); err == nil || !strings.Contains(err.Error(), "revoked") {
		t.Fatalf("tolerant loader must honor @revoked before normal-line pass, got %v", err)
	}
	// 未吊销的其它密钥：仍按普通行语义处理（未知主机）
	if err := cb("revoked-host", addr, pub2); err == nil {
		t.Fatal("non-revoked unknown key should be rejected as unknown host")
	}
}

// 通配与具体主机混排时的正例：普通主机行放行未吊销密钥。
func TestTolerantKnownHostsNormalPass(t *testing.T) {
	_, k, _ := ed25519.GenerateKey(nil)
	pub, _ := ssh.NewPublicKey(k.Public())
	dir := t.TempDir()
	kh := filepath.Join(dir, "known_hosts")
	line := knownhosts.Line([]string{"web1"}, pub)
	if err := os.WriteFile(kh, []byte(line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cb, err := tolerantKnownHosts(kh)
	if err != nil {
		t.Fatal(err)
	}
	addr := &net.TCPAddr{IP: net.ParseIP("10.0.0.1"), Port: 22}
	if err := cb("web1:22", addr, pub); err != nil {
		t.Fatalf("known host with matching key should pass, got %v", err)
	}
}
