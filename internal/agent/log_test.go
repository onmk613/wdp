package agent

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// getLogs 拉取一次 /logs 内容。
func getLogs(t *testing.T, ts *httptest.Server) string {
	t.Helper()
	resp, err := http.Get(ts.URL + "/logs")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	return string(b)
}

// waitLogsContain 轮询 /logs 直至包含目标片段（日志异步落缓冲）。
func waitLogsContain(t *testing.T, ts *httptest.Server, want string) string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		logs := getLogs(t, ts)
		if strings.Contains(logs, want) {
			return logs
		}
		if time.Now().After(deadline) {
			t.Fatalf("日志应含 %q，实际:\n%s", want, logs)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestParseLogLevel 级别名解析：合法五种 + 空 = info，非法报错。
func TestParseLogLevel(t *testing.T) {
	cases := map[string]bool{
		"trace": true, "debug": true, "info": true, "warn": true,
		"warning": true, "error": true, "": true, "INFO": true,
		"verbose": false, "fatal": false,
	}
	for name, ok := range cases {
		_, err := ParseLogLevel(name)
		if ok && err != nil {
			t.Fatalf("%q 应合法: %v", name, err)
		}
		if !ok && err == nil {
			t.Fatalf("%q 应报错", name)
		}
	}
}

// TestLogsEndpointInfoLevel info 默认级：必要运行记录入缓冲（命令摘要——
// 脚本常含密码/令牌，日志不记明文），访问日志与 httpdump 不出现。
func TestLogsEndpointInfoLevel(t *testing.T) {
	s := New(":0")
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	resp, err := http.Post(ts.URL+"/exec", "application/json",
		strings.NewReader(`{"script":"echo hello"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	logs := waitLogsContain(t, ts, "sha256=")
	if strings.Contains(logs, "echo hello") {
		t.Fatalf("info 级不得记录脚本明文（含密命令会随日志落盘并被 /logs 拉取）:\n%s", logs)
	}
	if strings.Contains(logs, "httpdump") || strings.Contains(logs, "HTTP POST") {
		t.Fatalf("info 级不应出现访问日志/httpdump:\n%s", logs)
	}
}

// TestLogsTraceHttpDump trace 级：逐请求访问日志 + httpdump，且
// 请求体中的提权密码被遮蔽。
func TestLogsTraceHttpDump(t *testing.T) {
	s := New(":0")
	if err := s.SetLogLevel("trace"); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	resp, err := http.Post(ts.URL+"/exec", "application/json",
		strings.NewReader(`{"script":"id","become_user":"root","become_password":"s3cret-pass"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	logs := waitLogsContain(t, ts, "httpdump request")
	for _, want := range []string{"POST /exec", "httpdump response"} {
		if !strings.Contains(logs, want) {
			t.Fatalf("trace 日志应含 %q:\n%s", want, logs)
		}
	}
	if strings.Contains(logs, "s3cret-pass") {
		t.Fatalf("提权密码应被遮蔽:\n%s", logs)
	}
	if !strings.Contains(logs, "***") {
		t.Fatalf("应含遮蔽标记:\n%s", logs)
	}
}

// TestLogFileSurvivesCleanup 日志文件落盘可写入，且不属于自清理范围
// （退役后保留供审计）。
func TestLogFileSurvivesCleanup(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "wdp-agent.log")
	s := newCleanupTestServer(t) // selfBin/证书均指向临时文件
	if err := s.SetLogFile(logPath); err != nil {
		t.Fatal(err)
	}
	s.logInfo("marker-before-cleanup %s", "x")

	s.initiateShutdown(shutdownReq{}, true) // 默认自清理

	if _, err := os.Stat(logPath); err != nil {
		t.Fatalf("日志文件应保留（自清理不删日志）: %v", err)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "marker-before-cleanup") {
		t.Fatalf("日志文件应含清理前记录:\n%s", data)
	}
	if !strings.Contains(string(data), "self-cleanup starting") {
		t.Fatalf("日志文件应含自清理开始记录:\n%s", data)
	}
}

// TestSetLogLevelInvalid 非法级别名报错。
func TestSetLogLevelInvalid(t *testing.T) {
	s := New(":0")
	if err := s.SetLogLevel("chatty"); err == nil {
		t.Fatal("非法级别应报错")
	}
}

// TestLogLevelFiltersInfo warn 级时 info 记录不进缓冲。
func TestLogLevelFiltersInfo(t *testing.T) {
	s := New(":0")
	if err := s.SetLogLevel("warn"); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	resp, err := http.Post(ts.URL+"/exec", "application/json",
		strings.NewReader(`{"script":"echo hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	time.Sleep(100 * time.Millisecond)
	logs := string(s.logRing.snapshot())
	if strings.Contains(logs, "echo hi") {
		t.Fatalf("warn 级不应记录 info 操作:\n%s", logs)
	}
}
