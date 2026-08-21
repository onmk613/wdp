package sshconn

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

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

func TestParseHostSpec(t *testing.T) {
	cases := []struct {
		spec    string
		address string
		port    int
		wantErr bool
	}{
		{"10.0.0.1", "10.0.0.1", 22, false},
		{"web1.example.com", "web1.example.com", 22, false},
		{"10.0.0.1:2222", "10.0.0.1", 2222, false},
		{"[10.0.0.1]:2222", "10.0.0.1", 2222, false},
		{"::1", "::1", 22, false},
		{"[::1]:2222", "::1", 2222, false},
		{"10.0.0.1:abc", "", 0, true},
		{"10.0.0.1:", "", 0, true},
		{"10.0.0.1:0", "", 0, true},
		{"10.0.0.1:70000", "", 0, true},
		{"host:22:33", "", 0, true},
	}
	for _, c := range cases {
		h, err := parseHostSpec(c.spec)
		if c.wantErr {
			if err == nil {
				t.Errorf("%q 应报错，得到 %+v", c.spec, h)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: %v", c.spec, err)
			continue
		}
		if h.Address != c.address || h.Port != c.port || h.Conn != "ssh" || h.Name != c.spec {
			t.Errorf("%q: got %+v", c.spec, h)
		}
	}
}

func TestDialableMarker(t *testing.T) {
	cases := map[string]bool{
		"10.0.0.11":        true,
		"[10.0.0.11]:22":   true,
		"web1.example.com": true,
		"*.example.com":    false,
		"web?":             false,
		"web[0-9]":         false,
		"|1|abc=def":       false,
		"":                 false,
	}
	for m, want := range cases {
		if got := dialableMarker(m); got != want {
			t.Errorf("dialableMarker(%q) = %v, want %v", m, got, want)
		}
	}
}

// TestHostsFromKnownHostsAll all 模式：普通与 @revoked 主机被枚举，CA/通配/哈希/注释被跳过。
func TestHostsFromKnownHostsAll(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "known_hosts")
	content := strings.Join([]string{
		"# comment",
		"10.0.0.11 " + testKeyA,
		"10.0.0.11 " + testKeyB, // 同主机重复标记，去重
		"[10.0.0.12]:2222 " + testKeyA,
		"@revoked rev.example.com " + testKeyA,
		"@cert-authority ca.example.com " + testKeyA,
		"*.example.com " + testKeyA,
		"|1|hashed=entry " + testKeyA,
		"",
	}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	hosts, err := hostsFromKnownHosts(path)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, h := range hosts {
		names = append(names, h.Name)
	}
	want := []string{"10.0.0.11", "[10.0.0.12]:2222", "rev.example.com"}
	if len(names) != len(want) {
		t.Fatalf("主机数 %d，期望 %d: %v", len(names), len(want), names)
	}
	for i, w := range want {
		if names[i] != w {
			t.Fatalf("第 %d 个主机 %q，期望 %q（全部: %v）", i, names[i], w, names)
		}
	}
}

// TestHostsFromSpecsAll 仅指定单个 all 时展开为 known_hosts 主机；其余按字面解析。
func TestHostsFromSpecsAll(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "known_hosts")
	if err := os.WriteFile(path, []byte("10.0.0.11 "+testKeyA+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	hosts, err := HostsFromSpecs([]string{"all"}, path)
	if err != nil || len(hosts) != 1 || hosts[0].Address != "10.0.0.11" {
		t.Fatalf("all 应展开为 known_hosts 主机: %+v, err=%v", hosts, err)
	}
	hosts, err = HostsFromSpecs([]string{"10.0.0.2:2222", "10.0.0.3"}, path)
	if err != nil || len(hosts) != 2 ||
		hosts[0].Port != 2222 || hosts[1].Port != 22 {
		t.Fatalf("多个主机应按字面解析: %+v, err=%v", hosts, err)
	}
	if _, err := HostsFromSpecs([]string{"bad:spec:x"}, path); err == nil {
		t.Fatal("非法主机格式应报错")
	}
}

// loadTestKnownHosts 写入内容并加载为 KnownHosts。
func loadTestKnownHosts(t *testing.T, content string) *KnownHosts {
	t.Helper()
	path := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	kh, err := LoadKnownHosts(path)
	if err != nil {
		t.Fatal(err)
	}
	return kh
}

// TestRemoveHosts 已下线主机的全部条目（多把钥匙、别名共享行、@revoked）整行删除；
// @cert-authority 与其他主机的条目保留。
func TestRemoveHosts(t *testing.T) {
	kh := loadTestKnownHosts(t, strings.Join([]string{
		"10.0.0.11 " + testKeyA,
		"10.0.0.11 " + testKeyB,                  // 同主机第二把钥匙，一并删除
		"web1.example.com,10.0.0.11 " + testKeyA, // 别名共享行：整行删除
		"@revoked 10.0.0.11 " + testKeyB,         // 吊销记录同属该主机，一并删除
		"@cert-authority 10.0.0.11 " + testKeyA,  // CA 记录：保留
		"other.example.com " + testKeyA,          // 其他主机：保留
		"[10.0.0.11]:2222 " + testKeyA,           // 同 IP 不同端口：保留
	}, "\n")+"\n")
	if kh.Dirty() {
		t.Fatal("刚加载应为干净状态")
	}
	n := kh.RemoveHosts([]*model.Host{{Name: "10.0.0.11", Address: "10.0.0.11", Port: 22}})
	if n != 4 {
		t.Fatalf("应删除 4 行，得到 %d", n)
	}
	if !kh.Dirty() {
		t.Fatal("删除后应为脏状态")
	}
	if err := kh.Save(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(kh.path)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{
		"@cert-authority 10.0.0.11 " + testKeyA,
		"other.example.com " + testKeyA,
		"[10.0.0.11]:2222 " + testKeyA,
	}, "\n") + "\n"
	if string(data) != want {
		t.Fatalf("删除结果不符:\n--- got ---\n%s\n--- want ---\n%s", data, want)
	}
}

// TestRemoveHostsPort 非 22 端口主机只删除 [host]:port 条目，不影响同 IP 裸条目。
func TestRemoveHostsPort(t *testing.T) {
	kh := loadTestKnownHosts(t, strings.Join([]string{
		"10.0.0.11 " + testKeyA,
		"[10.0.0.11]:2222 " + testKeyA,
	}, "\n")+"\n")
	n := kh.RemoveHosts([]*model.Host{{Name: "10.0.0.11:2222", Address: "10.0.0.11", Port: 2222}})
	if n != 1 {
		t.Fatalf("应仅删除 1 行，得到 %d", n)
	}
	if err := kh.Save(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(kh.path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(data), "10.0.0.11 "+testKeyA) {
		t.Fatalf("裸 IP 条目应保留: %s", data)
	}
}

// TestRemoveHostsUnknown 无匹配条目时不动文件、不置脏。
func TestRemoveHostsUnknown(t *testing.T) {
	kh := loadTestKnownHosts(t, "other.example.com "+testKeyA+"\n")
	if n := kh.RemoveHosts([]*model.Host{{Name: "10.0.0.99", Address: "10.0.0.99", Port: 22}}); n != 0 {
		t.Fatalf("无匹配不应删除，得到 %d", n)
	}
	if kh.Dirty() {
		t.Fatal("无匹配不应置脏")
	}
	if kh.RemoveHosts(nil) != 0 {
		t.Fatal("空主机列表应为 no-op")
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
