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
		strings.NewReader(`{"script":"echo marker-cmd\nexport TOK=deep-marker-secret"}`))
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
	// 日志记命令首行预览（可定位执行了什么）+ sha256 指纹；深行内容
	// 不得随日志文件落盘（令牌常在深行）
	if !strings.Contains(string(data), "sha256=") || !strings.Contains(string(data), "marker-cmd") {
		t.Fatalf("日志文件应含执行记录（首行预览 + 摘要）:\n%s", data)
	}
	if strings.Contains(string(data), "deep-marker-secret") {
		t.Fatalf("日志文件不得含脚本深行内容:\n%s", data)
	}
	if !strings.Contains(out.String(), "web/1") {
		t.Fatalf("输出应含主机与文件路径: %q", out.String())
	}
	// 拉回的日志文件 0600：目标 agent 开 trace 时报文转储随日志落盘，
	// 多用户控制机上不能组外可读
	if st, serr := os.Stat(p); serr != nil {
		t.Fatal(serr)
	} else if st.Mode().Perm() != 0o600 {
		t.Fatalf("日志文件应 0600，实际 %#o", st.Mode().Perm())
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
