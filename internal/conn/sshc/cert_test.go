package sshc

// 伴随证书（<私钥>-cert.pub）加载测试：生成测试 CA 与用户证书，
// 验证 certSignerFor 组合行为与 authMethods 的身份文件来源优先级。

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"wdp/internal/model"
)

// writeKeyWithCert 生成一对临时私钥/伴随证书文件，返回路径。
func writeKeyWithCert(t *testing.T, dir, name string) (keyPath, certPath string) {
	t.Helper()
	_, userKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	_, caKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caSigner, err := ssh.NewSignerFromKey(caKey)
	if err != nil {
		t.Fatal(err)
	}
	userSigner, err := ssh.NewSignerFromKey(userKey)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(userKey, "wdp-test")
	if err != nil {
		t.Fatal(err)
	}
	keyPath = filepath.Join(dir, name)
	certPath = keyPath + "-cert.pub"
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	cert := &ssh.Certificate{
		CertType:        ssh.UserCert,
		Key:             userSigner.PublicKey(),
		KeyId:           "wdp-test",
		ValidPrincipals: []string{"testuser"},
		ValidAfter:      uint64(time.Now().Add(-time.Minute).Unix()),
		ValidBefore:     uint64(time.Now().Add(time.Hour).Unix()),
	}
	if err := cert.SignCert(rand.Reader, caSigner); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(certPath, ssh.MarshalAuthorizedKey(cert), 0o644); err != nil {
		t.Fatal(err)
	}
	return keyPath, certPath
}

func TestCertSignerForCompanionCert(t *testing.T) {
	dir := t.TempDir()
	keyPath, _ := writeKeyWithCert(t, dir, "id_ed25519")

	data, _ := os.ReadFile(keyPath)
	signer, err := ssh.ParsePrivateKey(data)
	if err != nil {
		t.Fatal(err)
	}
	cert, warn := certSignerFor(keyPath, signer)
	if cert == nil || warn != "" {
		t.Fatalf("伴随证书应加载成功: %v %q", cert, warn)
	}
	if _, ok := cert.PublicKey().(*ssh.Certificate); !ok {
		t.Fatalf("证书签名者公钥应为证书: %T", cert.PublicKey())
	}

	// 无伴随证书：常态静默
	bare := filepath.Join(dir, "bare_key")
	if err := os.WriteFile(bare, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if cert, warn := certSignerFor(bare, signer); cert != nil || warn != "" {
		t.Fatalf("无证书应返回零值: %v %q", cert, warn)
	}

	// 损坏证书：跳过并给出诊断
	if err := os.WriteFile(bare+"-cert.pub", []byte("garbage\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if cert, warn := certSignerFor(bare, signer); cert != nil || warn == "" {
		t.Fatalf("损坏证书应诊断: %v %q", cert, warn)
	}
}

// TestConnectWithCertOnlyServer 端到端复刻"交互 ssh 能通、wdp 不通"现场：
// 服务器只信任 CA 签发的用户证书（裸公钥一律拒绝）。带伴随证书的身份文件
// 必须认证成功；同一私钥去掉伴随证书文件必须失败（旧版行为，回归锚点）。
func TestConnectWithCertOnlyServer(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")
	dir := t.TempDir()

	_, caKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caSigner, err := ssh.NewSignerFromKey(caKey)
	if err != nil {
		t.Fatal(err)
	}
	_, hostKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	hostSigner, err := ssh.NewSignerFromKey(hostKey)
	if err != nil {
		t.Fatal(err)
	}

	certChecker := &ssh.CertChecker{
		IsUserAuthority: func(k ssh.PublicKey) bool {
			return bytes.Equal(k.Marshal(), caSigner.PublicKey().Marshal())
		},
	}
	scfg := &ssh.ServerConfig{PublicKeyCallback: func(meta ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		cert, ok := key.(*ssh.Certificate)
		if !ok {
			return nil, errors.New("bare keys not accepted (CA-only server)")
		}
		return certChecker.Authenticate(meta, cert)
	}}
	scfg.AddHostKey(hostSigner)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			sconn, chans, reqs, err := ssh.NewServerConn(conn, scfg)
			if err != nil {
				conn.Close()
				continue
			}
			go ssh.DiscardRequests(reqs)
			go func() {
				for ch := range chans {
					ch.Reject(ssh.Prohibited, "test server")
				}
				sconn.Close()
			}()
		}
	}()

	// 证书签发给 principal=testuser，客户端必须以同一用户连接
	keyPath, _ := writeUserKeyCertSignedBy(t, dir, "client_key", caSigner, "testuser")
	newHost := func(key string) *model.Host {
		return &model.Host{
			Name: "certsrv", Address: "127.0.0.1", Conn: "ssh",
			Port: listener.Addr().(*net.TCPAddr).Port, User: "testuser",
			KeyPath: key, HostKeyCheck: false, ConnectTimeoutSec: 3,
		}
	}

	// 带伴随证书：认证成功
	c := New(newHost(keyPath))
	if err := c.Connect(context.Background()); err != nil {
		t.Fatalf("伴随证书应认证成功: %v", err)
	}
	c.Close()

	// 同一私钥、无伴随证书：服务器只认 CA，必须失败（用户现场）
	bare := filepath.Join(dir, "bare_key")
	data, _ := os.ReadFile(keyPath)
	if err := os.WriteFile(bare, data, 0o600); err != nil {
		t.Fatal(err)
	}
	c2 := New(newHost(bare))
	if err := c2.Connect(context.Background()); err == nil {
		c2.Close()
		t.Fatal("裸密钥对 CA-only 服务器应认证失败")
	}
}

// writeUserKeyCertSignedBy 生成由指定 CA 签发证书的私钥 + 伴随证书文件。
func writeUserKeyCertSignedBy(t *testing.T, dir, name string, caSigner ssh.Signer, principal string) (keyPath, certPath string) {
	t.Helper()
	_, userKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	userSigner, err := ssh.NewSignerFromKey(userKey)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(userKey, "wdp-test")
	if err != nil {
		t.Fatal(err)
	}
	keyPath = filepath.Join(dir, name)
	certPath = keyPath + "-cert.pub"
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	cert := &ssh.Certificate{
		CertType:        ssh.UserCert,
		Key:             userSigner.PublicKey(),
		KeyId:           "wdp-e2e",
		ValidPrincipals: []string{principal},
		ValidAfter:      uint64(time.Now().Add(-time.Minute).Unix()),
		ValidBefore:     uint64(time.Now().Add(time.Hour).Unix()),
	}
	if err := cert.SignCert(rand.Reader, caSigner); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(certPath, ssh.MarshalAuthorizedKey(cert), 0o644); err != nil {
		t.Fatal(err)
	}
	return keyPath, certPath
}

func TestAuthMethodsIdentitySources(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "") // 排除 agent 干扰
	dir := t.TempDir()
	key1, _ := writeKeyWithCert(t, dir, "key1")
	key2, _ := writeKeyWithCert(t, dir, "key2")

	// 身份文件来源优先级：显式 key_path > IdentityFiles > 内置默认
	cases := []struct {
		desc         string
		h            *model.Host
		wantPrimary  []string
		wantDefaults bool
		wantMethods  int // 密钥认证方法数（全部签名者合并为单个 publickey 方法）
	}{
		{"显式 key_path", &model.Host{Name: "h", Conn: "ssh", User: "root", KeyPath: key1},
			[]string{key1}, false, 1},
		{"显式 key_path 优先于 IdentityFiles", &model.Host{Name: "h", Conn: "ssh", User: "root",
			KeyPath: key1, IdentityFiles: []string{key1, key2}},
			[]string{key1}, false, 1},
		{"ssh config 身份文件（多文件累积）", &model.Host{Name: "h", Conn: "ssh", User: "root",
			IdentityFiles: []string{key1, key2}},
			[]string{key1, key2}, false, 1},
	}
	for _, c := range cases {
		primary, defaults := keyFilePaths(c.h)
		if len(primary) != len(c.wantPrimary) {
			t.Fatalf("%s: primary %v，期望 %v", c.desc, primary, c.wantPrimary)
		}
		for i := range c.wantPrimary {
			if primary[i] != c.wantPrimary[i] {
				t.Fatalf("%s: primary %v，期望 %v", c.desc, primary, c.wantPrimary)
			}
		}
		if c.wantDefaults != (len(defaults) == 3) {
			t.Fatalf("%s: defaults %v", c.desc, defaults)
		}
		methods, cleanup, warns := authMethods(c.h)
		cleanup()
		if len(methods) != c.wantMethods {
			t.Fatalf("%s: 方法数 %d，期望 %d", c.desc, len(methods), c.wantMethods)
		}
		if len(warns) != 0 {
			t.Fatalf("%s: 意外告警 %v", c.desc, warns)
		}
	}
}
