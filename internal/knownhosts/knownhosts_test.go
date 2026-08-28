package knownhosts

import (
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"wdp/internal/model"
)

const (
	testKeyA = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOdIhVbA7hFy7a5l51qezQcO97P3dCMVrbf9tjaEJ8iM"
	testKeyB = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFXk1tSNKTm+ar9T9CWFo5JeSbsQsXw7GLKGc30JxPBM"
)

func mustKey(t *testing.T, blob string) ssh.PublicKey {
	t.Helper()
	k, _, _, _, err := ssh.ParseAuthorizedKey([]byte(blob))
	if err != nil {
		t.Fatalf("解析测试公钥失败: %v", err)
	}
	return k
}

func entry(raw string) *khEntry { return parseKHEntry(raw) }

func TestReconcileAdded(t *testing.T) {
	key := mustKey(t, testKeyA)
	action, entries := reconcile(nil, "10.0.0.11", key, "10.0.0.11 "+testKeyA)
	if action != ActionAdded || len(entries) != 1 || entries[0].removed {
		t.Fatalf("空 known_hosts 应新增: action=%v entries=%+v", action, entries)
	}
}

func TestReconcileExists(t *testing.T) {
	key := mustKey(t, testKeyA)
	entries := []*khEntry{entry("10.0.0.11 " + testKeyA)}
	action, next := reconcile(entries, "10.0.0.11", key, "10.0.0.11 "+testKeyA)
	if action != ActionExists || len(next) != 1 || next[0].removed {
		t.Fatalf("同指纹应已存在且无改动: action=%v entries=%+v", action, next)
	}
}

// TestReconcileOverwriteChangedKey 核心场景：同一 IP、指纹变化 → 删除旧行写入新行。
func TestReconcileOverwriteChangedKey(t *testing.T) {
	newKey := mustKey(t, testKeyB)
	entries := []*khEntry{entry("10.0.0.11 " + testKeyA)}
	action, next := reconcile(entries, "10.0.0.11", newKey, "10.0.0.11 "+testKeyB)
	if action != ActionUpdated {
		t.Fatalf("指纹变化应更新: action=%v", action)
	}
	if len(next) != 2 || !next[0].removed || next[1].removed {
		t.Fatalf("旧行应删除、新行应保留: %+v", next)
	}
	if next[0].raw != "10.0.0.11 "+testKeyA || next[1].raw != "10.0.0.11 "+testKeyB {
		t.Fatalf("行内容不对: %+v", next)
	}
}

// TestReconcileCleansStaleDup 旧、新两行并存（此前 append 行为留下的历史包袱），
// 再扫到新指纹：删除旧行，保留同指纹行，不重复追加。
func TestReconcileCleansStaleDup(t *testing.T) {
	newKey := mustKey(t, testKeyB)
	entries := []*khEntry{
		entry("10.0.0.11 " + testKeyA),
		entry("10.0.0.11 " + testKeyB),
	}
	action, next := reconcile(entries, "10.0.0.11", newKey, "10.0.0.11 "+testKeyB)
	if action != ActionUpdated {
		t.Fatalf("清理过期重复行应计为更新: action=%v", action)
	}
	if len(next) != 2 || !next[0].removed || next[1].removed {
		t.Fatalf("仅旧指纹行应删除: %+v", next)
	}
}

func TestReconcileRevokedBlocked(t *testing.T) {
	key := mustKey(t, testKeyA)
	entries := []*khEntry{entry("@revoked 10.0.0.11 " + testKeyA)}
	action, next := reconcile(entries, "10.0.0.11", key, "10.0.0.11 "+testKeyA)
	if action != ActionRevoked || len(next) != 1 || next[0].removed {
		t.Fatalf("@revoked 同指纹应拒绝覆盖: action=%v entries=%+v", action, next)
	}
}

// TestReconcileIgnoresOtherMarkers 不同端口/主机名的条目与 @cert-authority 均不受影响。
func TestReconcileIgnoresOtherMarkers(t *testing.T) {
	key := mustKey(t, testKeyA)
	entries := []*khEntry{
		entry("10.0.0.11 " + testKeyB),                 // 22 端口旧指纹（将被更新）
		entry("[10.0.0.11]:2222 " + testKeyB),          // 其它端口条目：不动
		entry("@cert-authority 10.0.0.11 " + testKeyB), // CA 条目：不动
	}
	action, next := reconcile(entries, "10.0.0.11", key, "10.0.0.11 "+testKeyA)
	if action != ActionUpdated {
		t.Fatalf("action=%v", action)
	}
	if !next[0].removed || next[1].removed || next[2].removed {
		t.Fatalf("只应删除同主机段的普通旧指纹行: %+v", next)
	}
	if len(next) != 4 {
		t.Fatalf("应追加新行: len=%d", len(next))
	}
}

// TestReconcileOtherPortProtected 扫描 2222 端口不得误删裸 IP 条目（属于 22 端口主机）。
func TestReconcileOtherPortProtected(t *testing.T) {
	key := mustKey(t, testKeyA)
	entries := []*khEntry{entry("10.0.0.11 " + testKeyB)}
	action, next := reconcile(entries, "[10.0.0.11]:2222", key, "[10.0.0.11]:2222 "+testKeyA)
	if action != ActionAdded {
		t.Fatalf("非 22 端口扫描与裸 IP 条目互不影响: action=%v", action)
	}
	if len(next) != 2 || next[0].removed {
		t.Fatalf("裸 IP 条目应保留: %+v", next)
	}
}

// TestReconcileMultiHostLineRemoved 旧行同时挂了多个主机名，换机后整行删除（别名一并失效）。
func TestReconcileMultiHostLineRemoved(t *testing.T) {
	key := mustKey(t, testKeyA)
	entries := []*khEntry{entry("web1.example.com,10.0.0.11 " + testKeyB)}
	action, next := reconcile(entries, "10.0.0.11", key, "10.0.0.11 "+testKeyA)
	if action != ActionUpdated || len(next) != 2 || !next[0].removed {
		t.Fatalf("含别名的旧指纹行应整行删除: action=%v entries=%+v", action, next)
	}
}

// TestScanRewriteFile 端到端：注释/空行/无关条目原样保留，目标行被替换。
func TestScanRewriteFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "known_hosts")
	content := "# comment\n\nother.example.com " + testKeyA + "\n10.0.0.11 " + testKeyA + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	entries, err := readKnownHosts(path)
	if err != nil {
		t.Fatal(err)
	}
	newKey := mustKey(t, testKeyB)
	action, entries := reconcile(entries, "10.0.0.11", newKey, "10.0.0.11 "+testKeyB)
	if action != ActionUpdated {
		t.Fatalf("action=%v", action)
	}
	if err := writeKnownHostsFile(path, entries); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	want := "# comment\n\nother.example.com " + testKeyA + "\n10.0.0.11 " + testKeyB + "\n"
	if got != want {
		t.Fatalf("重写结果不符:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
	if !strings.Contains(got, testKeyA) || !strings.Contains(got, testKeyB) {
		t.Fatalf("指纹内容不符: %s", got)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("权限应保持 0600: %v", fi.Mode().Perm())
	}
}

// TestKnownHostsMarker 主机段格式：非 22 端口或 IPv6（含冒号）一律 [host]:port，
// 对齐 OpenSSH put_host_port——否则 IPv6@22 与校验侧 JoinHostPort 产物失配。
func TestKnownHostsMarker(t *testing.T) {
	cases := []struct {
		name string
		addr string
		port int
		want string
	}{
		{"域名22裸写", "web1.example.com", 22, "web1.example.com"},
		{"IPv4非22方括号", "10.0.0.5", 2222, "[10.0.0.5]:2222"},
		{"IPv6@22也方括号", "fd00::5", 22, "[fd00::5]:22"},
		{"IPv6非22方括号", "fd00::5", 2222, "[fd00::5]:2222"},
	}
	for _, tc := range cases {
		h := &model.Host{Address: tc.addr, Port: tc.port}
		if got := KnownHostsMarker(h); got != tc.want {
			t.Errorf("%s: marker=%q want %q", tc.name, got, tc.want)
		}
	}
}

// khFixture 生成一对真实 ed25519 密钥与对应 known_hosts 行。
func khFixture(t *testing.T, host string) (line string, pub ssh.PublicKey) {
	t.Helper()
	pubKey, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pub, err = ssh.NewPublicKey(pubKey)
	if err != nil {
		t.Fatal(err)
	}
	return knownhosts.Line([]string{host}, pub), pub
}

// TestScanSshSelfHeal 坏行自愈：LoadKnownHosts 标记坏行，Save 后从文件
// 消失；好行保留。
func TestScanSshSelfHeal(t *testing.T) {
	dir := t.TempDir()
	khPath := filepath.Join(dir, "known_hosts")
	goodLine, _ := khFixture(t, "10.0.0.9")
	content := "!!!broken-line-not-a-key\n" + goodLine + "\n"
	if err := os.WriteFile(khPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	kh, err := LoadKnownHosts(khPath)
	if err != nil {
		t.Fatal(err)
	}
	if !kh.Dirty() {
		t.Fatal("坏行标记删除后应为 dirty")
	}
	if err := kh.Save(); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(khPath)
	if strings.Contains(string(data), "!!!broken") {
		t.Fatalf("坏行应被清除:\n%s", data)
	}
	if !strings.Contains(string(data), "10.0.0.9") {
		t.Fatalf("好行应保留:\n%s", data)
	}
}
