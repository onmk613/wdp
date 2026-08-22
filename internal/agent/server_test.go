package agent

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"wdp/internal/connection"
	"wdp/internal/connection/agentconn"
	"wdp/internal/model"
)

// startAgent 启动测试 agent。
func startAgent(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	ts := httptest.NewServer(New(":0").Handler())
	t.Cleanup(ts.Close)
	return ts, strings.TrimPrefix(ts.URL, "http://")
}

func TestHealth(t *testing.T) {
	ts, _ := startAgent(t)
	resp, err := ts.Client().Get(ts.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	for _, want := range []string{`"ok":true`, `"version"`, `"hostname"`} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("health 响应缺少 %s: %s", want, body)
		}
	}
}

func TestExecEcho(t *testing.T) {
	ts, _ := startAgent(t)
	resp, err := ts.Client().Post(ts.URL+"/exec", "application/json",
		strings.NewReader(`{"script":"echo hello; echo err >&2; exit 0"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	s := string(body)
	if !strings.Contains(s, `"stdout":"hello\n"`) || !strings.Contains(s, `"stderr":"err\n"`) {
		t.Fatalf("exec 响应: %s", s)
	}
}

func TestExecNonZeroAndTimeout(t *testing.T) {
	ts, _ := startAgent(t)
	resp, _ := ts.Client().Post(ts.URL+"/exec", "application/json",
		strings.NewReader(`{"script":"exit 7"}`))
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"code":7`) {
		t.Fatalf("退出码: %s", body)
	}
	resp2, _ := ts.Client().Post(ts.URL+"/exec", "application/json",
		strings.NewReader(`{"script":"sleep 5","timeout_ms":200}`))
	body2, _ := io.ReadAll(resp2.Body)
	if !strings.Contains(string(body2), `"timed_out":true`) {
		t.Fatalf("超时: %s", body2)
	}
}

func TestFileRoundTrip(t *testing.T) {
	ts, _ := startAgent(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "a", "b.txt")

	// 上传
	q := url.Values{}
	q.Set("path", p)
	q.Set("mode", "755")
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/file?"+q.Encode(), strings.NewReader("content-xyz"))
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	data, err := os.ReadFile(p)
	if err != nil || string(data) != "content-xyz" {
		t.Fatalf("上传内容: %q err=%v", data, err)
	}
	fi, _ := os.Stat(p)
	if fi.Mode().Perm() != 0o755 {
		t.Fatalf("权限: %v", fi.Mode().Perm())
	}

	// 下载
	resp2, err := ts.Client().Get(ts.URL + "/file?path=" + p)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	got, _ := io.ReadAll(resp2.Body)
	if string(got) != "content-xyz" {
		t.Fatalf("下载内容: %q", got)
	}
}

func TestAgentConnRoundTrip(t *testing.T) {
	ts, _ := startAgent(t)
	host := &model.Host{Name: "t", Conn: "agent", AgentURL: ts.URL, Address: "127.0.0.1"}
	conn := agentconn.New(host, nil)
	if err := conn.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}

	// exec
	out, err := conn.Exec(context.Background(), connection.ExecRequest{Script: "echo agent-ok"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Code != 0 || !strings.Contains(out.Stdout, "agent-ok") {
		t.Fatalf("exec: %+v", out)
	}

	// upload / download
	dir := t.TempDir()
	p := filepath.Join(dir, "f.txt")
	if err := conn.UploadFile(context.Background(), p, bytes.NewReader([]byte("round-trip")), 0o600); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := conn.DownloadFile(context.Background(), p, &buf); err != nil {
		t.Fatal(err)
	}
	if buf.String() != "round-trip" {
		t.Fatalf("下载: %q", buf.String())
	}
}
