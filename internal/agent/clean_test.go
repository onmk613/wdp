package agent

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newCleanupTestServer 构造自清理测试服务：selfBin 与证书材料全部指向
// 临时文件，防误删测试二进制；systemd 停用在无 systemctl 的环境自然跳过。
func newCleanupTestServer(t *testing.T) *Server {
	t.Helper()
	s := New(":0")
	dir := t.TempDir()
	touch := func(name string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	s.selfBin = touch("bin")
	s.tlsCertFile = touch("agent.crt")
	s.tlsKeyFile = touch("agent.key")
	s.tlsCAFile = touch("ca.crt")
	return s
}

// assertGone 断言路径已被清理（容忍清理 goroutine 的调度延迟）。
func assertGone(t *testing.T, what, path string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s 应被清理: %s", what, path)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestPerformCleanupDefault 默认清理（空请求）：二进制与证书材料
// （cert/key/CA）全部删除。
func TestPerformCleanupDefault(t *testing.T) {
	s := newCleanupTestServer(t)
	s.initiateShutdown(shutdownReq{}, true)
	for _, p := range []string{s.selfBin, s.tlsCertFile, s.tlsKeyFile, s.tlsCAFile} {
		assertGone(t, "默认清理对象", p)
	}
}

// TestPerformCleanupCustom JSON 指定额外文件/目录与单元名：
// 额外路径一并删除，默认清理对象照常删除。
func TestPerformCleanupCustom(t *testing.T) {
	s := newCleanupTestServer(t)
	dir := t.TempDir()
	extraFile := filepath.Join(dir, "extra.conf")
	extraDir := filepath.Join(dir, "sub")
	if err := os.WriteFile(extraFile, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(extraDir, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	s.initiateShutdown(shutdownReq{SystemdUnit: "my-agent", Files: []string{extraFile, extraDir}}, true)
	assertGone(t, "指定文件", extraFile)
	assertGone(t, "指定目录", extraDir)
	for _, p := range []string{s.selfBin, s.tlsCAFile} {
		assertGone(t, "默认清理对象", p)
	}
}

// TestCleanupUnit 单元名归一化：请求指定优先，缺省取启动参数，
// 均空回退内置默认 wdp-agent。
func TestCleanupUnit(t *testing.T) {
	s := New(":0")
	if got := s.cleanupUnit(""); got != "wdp-agent" {
		t.Fatalf("缺省应为 wdp-agent，实际 %s", got)
	}
	s.SetSystemdUnit("custom")
	if got := s.cleanupUnit(""); got != "custom" {
		t.Fatalf("应取启动参数单元名，实际 %s", got)
	}
	if got := s.cleanupUnit("mine"); got != "mine" {
		t.Fatalf("请求指定应优先，实际 %s", got)
	}
}

// TestHandleShutdownBodyForms 请求体形态：空 body、空白、{}、null 均
// 视为默认清理（200）；坏 JSON 拒绝（400）且不触发清理。
func TestHandleShutdownBodyForms(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		code int
	}{
		{"empty", "", http.StatusOK},
		{"blank", "  \n\t", http.StatusOK},
		{"empty-json", "{}", http.StatusOK},
		{"null", "null", http.StatusOK},
		{"bad-json", "not-json", http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newCleanupTestServer(t)
			ts := httptest.NewServer(s.Handler())
			defer ts.Close()

			resp, err := http.Post(ts.URL+"/shutdown", "application/json", strings.NewReader(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != tc.code {
				t.Fatalf("状态码应为 %d，实际 %d", tc.code, resp.StatusCode)
			}
			if tc.code == http.StatusBadRequest {
				return // 坏 JSON：清理不应发生（响应即拒绝，异步清理不会启动）
			}
			assertGone(t, "默认清理对象", s.selfBin)
		})
	}
}

// TestRemoveExtraPathsRootGuard 根路径拒删、空串跳过（手滑防呆）。
func TestRemoveExtraPathsRootGuard(t *testing.T) {
	s := New(":0")
	s.removeExtraPaths([]string{"", "   ", "/", "/../.."})
	if _, err := os.Stat("/"); err != nil {
		t.Fatalf("根目录不应被删除: %v", err)
	}
}

// TestInitiateShutdownNoCleanup cleanup=false 仅关停不清理
// （空闲退出未配 --cleanup-on-shutdown 的常驻 agent 用）。
func TestInitiateShutdownNoCleanup(t *testing.T) {
	s := newCleanupTestServer(t)
	s.initiateShutdown(shutdownReq{}, false)
	if _, err := os.Stat(s.selfBin); err != nil {
		t.Fatalf("cleanup=false 不应删除二进制: %v", err)
	}
}
