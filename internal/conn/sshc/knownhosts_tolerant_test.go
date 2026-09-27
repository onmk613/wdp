package sshc

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// tcpAddr 测试用远端地址（knownhosts 回调会解引用，不能传 nil）。
func tcpAddr() net.Addr { return &net.TCPAddr{IP: net.IPv4(10, 0, 0, 1), Port: 22} }

// khFixture 生成一对真实 ed25519 密钥与对应 known_hosts 行。
func khFixture(t *testing.T, host string) (line string, pub ssh.PublicKey) {
	t.Helper()
	pubKey, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pub, err = ssh.NewPublicKey(pubKey)
	if err != nil {
		t.Fatal(err)
	}
	_ = priv
	return knownhosts.Line([]string{host}, pub), pub
}

// TestTolerantKnownHosts 容错加载：坏行跳过不拖垮好行；CRLF 兼容；
// 指纹不匹配与未知主机的错误语义保持。
func TestTolerantKnownHosts(t *testing.T) {
	dir := t.TempDir()
	khPath := filepath.Join(dir, "known_hosts")
	goodLine, pub := khFixture(t, "10.0.0.1")
	// 第 1 行故意损坏（非法 base64）+ CRLF 行尾，第 2 行正常
	content := "10.0.0.1 ssh-ed25519 AAAA!!!not-base64!!!\r\n" + goodLine + "\n"
	if err := os.WriteFile(khPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	cb, err := tolerantKnownHosts(khPath)
	if err != nil {
		t.Fatalf("坏行应被跳过而非整体失败: %v", err)
	}
	// 好行生效：正确公钥通过
	if err := cb("10.0.0.1:22", tcpAddr(), pub); err != nil {
		t.Fatalf("有效记录应通过: %v", err)
	}
	// 指纹不匹配：换一把钥匙
	otherKey, _, _ := ed25519.GenerateKey(rand.Reader)
	otherPub, oerr := ssh.NewPublicKey(otherKey)
	if oerr != nil {
		t.Fatal(oerr)
	}
	err = cb("10.0.0.1:22", tcpAddr(), otherPub)
	if err == nil {
		t.Fatal("指纹不匹配应拒绝")
	}
	if ke, ok := errors.AsType[*knownhosts.KeyError](err); !ok || len(ke.Want) == 0 {
		t.Fatalf("应为 KeyError(不匹配): %v", err)
	}
	// 未知主机
	if err := cb("10.9.9.9:22", tcpAddr(), pub); err == nil {
		t.Fatal("未知主机应拒绝")
	}
}

// TestTolerantKnownHostsCRLFOnly 全 CRLF 文件：剥 \r 后全部可解析。
func TestTolerantKnownHostsCRLFOnly(t *testing.T) {
	dir := t.TempDir()
	khPath := filepath.Join(dir, "known_hosts")
	l1, p1 := khFixture(t, "10.0.1.1")
	l2, p2 := khFixture(t, "10.0.1.2")
	content := l1 + "\r\n" + l2 + "\r\n"
	if err := os.WriteFile(khPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cb, err := tolerantKnownHosts(khPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := cb("10.0.1.1:22", tcpAddr(), p1); err != nil {
		t.Fatalf("CRLF 行应可用: %v", err)
	}
	if err := cb("10.0.1.2:22", tcpAddr(), p2); err != nil {
		t.Fatalf("CRLF 行应可用: %v", err)
	}
}

// TestTolerantKnownHostsAllBad 全部行损坏：按未知主机语义拒绝（可连接性
// 诊断信息来自 stderr 告警）。
func TestTolerantKnownHostsAllBad(t *testing.T) {
	dir := t.TempDir()
	khPath := filepath.Join(dir, "known_hosts")
	if err := os.WriteFile(khPath, []byte("garbage line one\n!!!another\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cb, err := tolerantKnownHosts(khPath)
	if err != nil {
		t.Fatal(err)
	}
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	pub, _ := ssh.NewPublicKey(priv)
	err = cb("10.9.9.9:22", tcpAddr(), pub)
	if err == nil {
		t.Fatal("无有效记录应拒绝")
	}
	var ke *knownhosts.KeyError
	if !errors.As(err, &ke) {
		t.Fatalf("应为 KeyError 语义: %v", err)
	}
	if len(ke.Want) != 0 {
		t.Fatalf("应为未知主机（Want 空）: %+v", ke)
	}
}

// TestKnownHostsCacheReuse (path, mtime, size) 缓存：同文件未改写时复用
// 已构建回调（指针同一）；改写文件（mtime/size 变化）后重建。缓存命中的
// 回调行为与首建一致（坏行跳过语义不因缓存丢失）。
func TestKnownHostsCacheReuse(t *testing.T) {
	dir := t.TempDir()
	khPath := filepath.Join(dir, "known_hosts")
	goodLine, pub := khFixture(t, "10.0.3.1")
	content := "10.0.3.1 ssh-ed25519 AAAA!!!bad!!!\n" + goodLine + "\n"
	if err := os.WriteFile(khPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	cb1, err := cachedTolerantKnownHosts(khPath)
	if err != nil {
		t.Fatal(err)
	}
	// 坏行跳过语义在首建回调上成立
	if err := cb1("10.0.3.1:22", tcpAddr(), pub); err != nil {
		t.Fatalf("首建回调应放行好行: %v", err)
	}
	cb2, err := cachedTolerantKnownHosts(khPath)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprintf("%p", cb1) != fmt.Sprintf("%p", cb2) {
		t.Fatal("文件未变时应复用同一回调")
	}

	// 改写文件（追加一条新主机记录）：缓存失效重建，新记录生效
	l2, p2 := khFixture(t, "10.0.3.2")
	f, err := os.OpenFile(khPath, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(l2 + "\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	cb3, err := cachedTolerantKnownHosts(khPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := cb3("10.0.3.2:22", tcpAddr(), p2); err != nil {
		t.Fatalf("改写后重建的回调应含新记录: %v", err)
	}
}

// TestTolerantKnownHostsValidFileUnchanged 无坏行文件：行为与严格加载一致。
func TestTolerantKnownHostsValidFileUnchanged(t *testing.T) {
	dir := t.TempDir()
	khPath := filepath.Join(dir, "known_hosts")
	line, pub := khFixture(t, "10.0.2.1")
	if err := os.WriteFile(khPath, []byte(line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cb, err := tolerantKnownHosts(khPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := cb("10.0.2.1:22", tcpAddr(), pub); err != nil {
		t.Fatal(err)
	}
	// 临时文件已清理
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".known_hosts.scan") {
			t.Fatalf("临时文件应清理: %s", e.Name())
		}
	}
}
