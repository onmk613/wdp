package agent

// 无 mTLS 启动的告警回归：回环无认证模式仅限单用户主机（回环对同机所有
// 用户可达，agent 常以 root 常驻即本地提权面），启动日志必须高亮提示，
// 而不是只在一行 listening info 里静默带过。

import (
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"wdp/internal/ca"
)

// TestNoAuthStartupWarns 无 mTLS 启动时日志出现 WITHOUT authentication 告警。
func TestNoAuthStartupWarns(t *testing.T) {
	s := New("127.0.0.1:0")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = s.serve(ln) }()
	t.Cleanup(func() { _ = ln.Close() })

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(string(s.logRing.snapshot()), "WITHOUT authentication") {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("无 mTLS 启动应记录高亮告警:\n%s", s.logRing.snapshot())
}

// TestMTLSStartupNoWarn 配置 mTLS 后不出现无认证告警（避免告警噪音化，
// 告警失去指向性）。
func TestMTLSStartupNoWarn(t *testing.T) {
	dir := t.TempDir()
	if _, _, _, err := ca.Init(ca.InitOptions{Dir: dir, Days: 3650}); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := ca.Issue(ca.IssueOptions{Dir: dir, Profile: ca.ProfileServer, SANs: []string{"127.0.0.1"}, Days: 30}, "host"); err != nil {
		t.Fatal(err)
	}
	s := New("127.0.0.1:0")
	if err := s.ConfigureAuth(filepath.Join(dir, "ca.crt"), filepath.Join(dir, "host.crt"), filepath.Join(dir, "host.key")); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = s.serve(ln) }()
	t.Cleanup(func() { _ = ln.Close() })

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		logs := string(s.logRing.snapshot())
		if strings.Contains(logs, "listening on") {
			if strings.Contains(logs, "WITHOUT authentication") {
				t.Fatalf("mTLS 模式不应出现无认证告警:\n%s", logs)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("未等到启动日志:\n%s", s.logRing.snapshot())
}
