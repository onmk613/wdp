package agent

import (
	"encoding/json"
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

// TestPerformCleanupCustom JSON 指定额外文件/目录与单元名：合法领地内的
// 额外路径（证书目录与 runs 目录内）一并删除，默认清理对象照常删除。
func TestPerformCleanupCustom(t *testing.T) {
	s := newCleanupTestServer(t)
	// 证书目录内的额外目录与 runs 根目录内的额外文件：均在白名单前缀内
	certDir := filepath.Dir(s.tlsCertFile)
	runsRoot := filepath.Join(t.TempDir(), "runs")
	s.SetRunsDir(runsRoot)
	extraDir := filepath.Join(certDir, "old-certs")
	extraFile := filepath.Join(runsRoot, "run-legacy")
	if err := os.MkdirAll(filepath.Join(extraDir, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(runsRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(extraFile, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	s.initiateShutdown(shutdownReq{SystemdUnit: "my-agent", Files: []string{extraDir, extraFile}}, true)
	assertGone(t, "指定目录", extraDir)
	assertGone(t, "指定文件", extraFile)
	for _, p := range []string{s.selfBin, s.tlsCAFile} {
		assertGone(t, "默认清理对象", p)
	}
}

// TestRemoveExtraPathsScopeGuard files 参数收紧到 agent 领地：根路径与
// 领地外路径（含符号父目录逃逸、自身二进制的无关同名前缀）拒绝且文件
// 保留；自身二进制附属（<bin>.log）照常受理。
func TestRemoveExtraPathsScopeGuard(t *testing.T) {
	s := newCleanupTestServer(t)
	dir := t.TempDir()
	victim := filepath.Join(dir, "victim")
	if err := os.WriteFile(victim, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	sidecar := s.selfBin + ".log"
	if err := os.WriteFile(sidecar, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// selfBin 的字符串前缀但不等于 selfBin/附属：不得放行（放在领地外
	// 目录，避开测试夹具把证书与二进制放同一临时目录造成的前缀命中）
	evil := filepath.Join(dir, filepath.Base(s.selfBin)+"-evil")
	if err := os.WriteFile(evil, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	accepted, refused := s.removeExtraPaths([]string{
		"", "   ", "/", "/../..",
		victim,
		dir + string(filepath.Separator) + "miss",
		evil,
		sidecar,
	})
	if _, err := os.Stat("/"); err != nil {
		t.Fatalf("根目录不应被删除: %v", err)
	}
	if _, err := os.Stat(victim); err != nil {
		t.Fatalf("领地外路径被删除: %v", err)
	}
	if _, err := os.Stat(evil); err != nil {
		t.Fatalf("二进制前缀仿冒路径被删除: %v", err)
	}
	if _, err := os.Stat(sidecar); !os.IsNotExist(err) {
		t.Fatalf("自身附属 .log 应被删除: %v", err)
	}
	if len(accepted) != 1 || accepted[0] != sidecar {
		t.Fatalf("受理清单应只含附属文件: %v", accepted)
	}
	wantRefused := []string{"/", "/../..", victim, filepath.Join(dir, "miss"), evil}
	if len(refused) != len(wantRefused) {
		t.Fatalf("拒绝清单不符: %v (want %v)", refused, wantRefused)
	}
	for i, r := range wantRefused {
		if refused[i] != r {
			t.Fatalf("拒绝清单顺序/内容不符: %v (want %v)", refused, wantRefused)
		}
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

// TestHandleShutdownRefusedFilesInResponse 越界 files 在响应中说明：
// 清理异步执行，拒绝原因若只进日志调用方不可见（会误以为清理完成）。
func TestHandleShutdownRefusedFilesInResponse(t *testing.T) {
	s := newCleanupTestServer(t)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	body := `{"files":["/etc/passwd"]}`
	resp, err := http.Post(ts.URL+"/shutdown", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("拒绝额外路径不应改变状态码: %d", resp.StatusCode)
	}
	var out struct {
		Ok           bool     `json:"ok"`
		RefusedFiles []string `json:"refused_files"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if !out.Ok || len(out.RefusedFiles) != 1 || out.RefusedFiles[0] != "/etc/passwd" {
		t.Fatalf("响应应说明被拒路径: %+v", out)
	}
	if _, err := os.Stat("/etc/passwd"); err != nil {
		t.Fatalf("/etc/passwd 不应被删除: %v", err)
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

// TestUnitFileName 单元文件名归一：无后缀补 .service，已带后缀不变。
func TestUnitFileName(t *testing.T) {
	cases := map[string]string{
		"wdp-agent":         "wdp-agent.service",
		"wdp-agent.service": "wdp-agent.service",
		"my.agent":          "my.agent.service",
	}
	for in, want := range cases {
		if got := unitFileName(in); got != want {
			t.Fatalf("unitFileName(%q) = %q, want %q", in, got, want)
		}
	}
}
