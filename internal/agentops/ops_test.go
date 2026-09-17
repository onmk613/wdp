package agentops

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"wdp/internal/agent"
	"wdp/internal/model"
)

// agentTestHost 构造指向测试服务的 conn: agent 主机。
func agentTestHost(name, url string) *model.Host {
	return &model.Host{Name: name, Conn: "agent", AgentURL: url}
}

// TestStatus status 内核：健康输出含证书到期与剩余天数；
// 不可达主机计失败并使整体退出非零。
func TestStatus(t *testing.T) {
	s := agent.New(":0")
	s.SetIdleTimeout(time.Hour)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	hosts := []*model.Host{agentTestHost("h1", ts.URL), agentTestHost("dead", "http://127.0.0.1:1")}
	var out bytes.Buffer
	err := Status(context.Background(), hosts, 2, nil, &out)
	if err == nil || !strings.Contains(err.Error(), "1/2") {
		t.Fatalf("一台失败应报 1/2，实际: %v", err)
	}
	if !strings.Contains(out.String(), "h1") || !strings.Contains(out.String(), "idle") {
		t.Fatalf("输出应含健康主机与表头: %q", out.String())
	}
}

// TestRetire retire 内核：可达 agent 退役成功；不可达主机计失败
// 并使整体退出非零。
func TestRetire(t *testing.T) {
	s := agent.New(":0")
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	hosts := []*model.Host{agentTestHost("h1", ts.URL), agentTestHost("dead", "http://127.0.0.1:1")}
	err := Retire(context.Background(), hosts, "", nil, 2, nil)
	if err == nil || !strings.Contains(err.Error(), "1/2") {
		t.Fatalf("一台失败应报 1/2，实际: %v", err)
	}
}

// TestLogs logs 内核：可达 agent 的日志逐主机写入本地文件，
// 不可达主机计失败；主机名归一化为安全文件名。
func TestLogs(t *testing.T) {
	s := agent.New(":0")
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	// 先产生一条 info 级运行记录（执行的命令）
	resp, err := http.Post(ts.URL+"/exec", "application/json",
		strings.NewReader(`{"script":"echo marker-cmd"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	outDir := t.TempDir()
	hosts := []*model.Host{
		{Name: "web/1", Conn: "agent", AgentURL: ts.URL},
		agentTestHost("dead", "http://127.0.0.1:1"),
	}
	var out bytes.Buffer
	err = Logs(context.Background(), hosts, outDir, 2, nil, &out)
	if err == nil || !strings.Contains(err.Error(), "1/2") {
		t.Fatalf("一台失败应报 1/2，实际: %v", err)
	}
	p := filepath.Join(outDir, "web_1.log")
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("应写入逐主机日志文件: %v", err)
	}
	// 日志只记命令摘要（sha256 前缀），不记脚本明文——含密命令不得随
	// 日志文件落盘
	if !strings.Contains(string(data), "sha256=") {
		t.Fatalf("日志文件应含执行记录（摘要）:\n%s", data)
	}
	if strings.Contains(string(data), "marker-cmd") {
		t.Fatalf("日志文件不得含脚本明文:\n%s", data)
	}
	if !strings.Contains(out.String(), "web/1") {
		t.Fatalf("输出应含主机与文件路径: %q", out.String())
	}
}

// TestAgentUnitFile systemd 单元内容：监听地址、证书路径、单元名占位齐全。
func TestAgentUnitFile(t *testing.T) {
	u := agentUnitFile("/usr/local/bin/wdp", "/etc/wdp", 7602, "wdp-agent", "/var/log/wdp-agent.log")
	for _, want := range []string{
		"ExecStart=/usr/local/bin/wdp agent --listen 0.0.0.0:7602",
		"--ca /etc/wdp/ca.crt", "--cert /etc/wdp/agent.crt", "--key /etc/wdp/agent.key",
		"--log-file /var/log/wdp-agent.log",
		"Restart=always", "WantedBy=multi-user.target",
	} {
		if !strings.Contains(u, want) {
			t.Fatalf("unit 应含 %q:\n%s", want, u)
		}
	}
}

// TestPickHostBinary install 二进制选择优先级：[agent].push_binary 平台表 >
// 同平台用控制端自身 > 同级 bin 目录；全未命中 false（该主机报错）。
func TestPickHostBinary(t *testing.T) {
	const self = "darwin_arm64"
	cfg := map[string]string{"linux_amd64": "/opt/wdp-linux-amd64"}
	sibling := func(platform string) (string, bool) {
		if platform == "linux_arm64" {
			return "/bin/wdp-linux-arm64", true
		}
		return "", false
	}

	// 平台表命中（优先于自身与同级）
	if b, ok := pickHostBinary("linux_amd64", self, "/exe", cfg, sibling); !ok || b != "/opt/wdp-linux-amd64" {
		t.Fatalf("平台表应命中: %q %v", b, ok)
	}
	// 同平台用控制端自身（优先于同级）
	if b, ok := pickHostBinary("darwin_arm64", self, "/exe", cfg, sibling); !ok || b != "/exe" {
		t.Fatalf("同平台应用自身: %q %v", b, ok)
	}
	// 跨平台同级 bin 目录命中
	if b, ok := pickHostBinary("linux_arm64", self, "/exe", cfg, sibling); !ok || b != "/bin/wdp-linux-arm64" {
		t.Fatalf("同级 bin 目录应命中: %q %v", b, ok)
	}
	// 全未命中 → false（install 该主机报错）
	if _, ok := pickHostBinary("linux_386", self, "/exe", cfg, sibling); ok {
		t.Fatal("全未命中应返回 false")
	}
	// nil cfg / nil sibling 不 panic
	if b, ok := pickHostBinary("linux_arm64", self, "/exe", nil, sibling); !ok || b != "/bin/wdp-linux-arm64" {
		t.Fatalf("nil cfg 应回退同级: %q %v", b, ok)
	}
	if _, ok := pickHostBinary("linux_arm64", self, "/exe", cfg, nil); ok {
		t.Fatal("nil sibling 且未配置平台表应 false")
	}
}

// TestCertNameFor 证书名约定：优先 host 字段（Address），缺省主机名。
func TestCertNameFor(t *testing.T) {
	if got := certNameFor(&model.Host{Name: "web1", Address: "10.0.0.13"}); got != "10.0.0.13" {
		t.Fatalf("应取 host 字段: %s", got)
	}
	if got := certNameFor(&model.Host{Name: "web1"}); got != "web1" {
		t.Fatalf("缺省应取主机名: %s", got)
	}
	if got := certNameFor(&model.Host{Name: "web1", Address: "web1"}); got != "web1" {
		t.Fatalf("地址等于主机名时取主机名: %s", got)
	}
}

// TestParseCertNotAfter 到期时刻解析（空串与非法串不算有效）。
func TestParseCertNotAfter(t *testing.T) {
	if _, ok := parseCertNotAfter(""); ok {
		t.Fatal("空串不应解析成功")
	}
	if _, ok := parseCertNotAfter("not-a-time"); ok {
		t.Fatal("非法串不应解析成功")
	}
	want := time.Now().UTC().Truncate(time.Second)
	got, ok := parseCertNotAfter(want.Format(time.RFC3339))
	if !ok || !got.Equal(want) {
		t.Fatalf("合法 RFC3339 应解析成功: %v %v", got, want)
	}
}
