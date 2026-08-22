package agent

// 审计修复回归：/file 上传请求体上限 / exec 输出截断。

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestUploadBodyLimit 回归：PUT /file 此前无请求体上限（io.Copy 直写），
// 可把目标机磁盘写满；现与 /exec 一致按 --max-request-mb 拒绝（413）。
func TestUploadBodyLimit(t *testing.T) {
	s := New(":0")
	s.SetMaxRequestBody(1) // 1MiB
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	dir := t.TempDir()
	big := bytes.Repeat([]byte("a"), 2<<20) // 2MiB > 上限
	req, err := http.NewRequest(http.MethodPut, ts.URL+"/file?path="+dir+"/big.bin", bytes.NewReader(big))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("超限上传应返回 413, got %v", resp.StatusCode)
	}

	// 上限内正常上传仍可用
	small := bytes.Repeat([]byte("b"), 1<<10)
	req2, _ := http.NewRequest(http.MethodPut, ts.URL+"/file?path="+dir+"/small.bin", bytes.NewReader(small))
	r3, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer r3.Body.Close()
	if r3.StatusCode != http.StatusOK {
		t.Fatalf("限内上传应成功, got %v", r3.StatusCode)
	}
}

// TestExecOutputTruncated 回归：exec 输出此前无上限缓冲，高输出命令可把
// 常驻 agent 撑爆 OOM；现每流保留 1MiB 前缀并标注截断。
func TestExecOutputTruncated(t *testing.T) {
	s := New(":0")
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	// 输出 ~3MiB（yes 由 head 截断；head 存在于 macOS/Linux 基础环境）
	script := "yes xxxx | head -c 3000000"
	body := strings.NewReader(`{"script":"` + script + `", "timeout_ms": 30000}`)
	resp, err := http.Post(ts.URL+"/exec", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Stdout string `json:"stdout"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Stdout) > (1<<20)+200 { // 1MiB 前缀 + 截断标注
		t.Fatalf("stdout 应被截断到 ~1MiB, got %d", len(out.Stdout))
	}
	if !strings.Contains(out.Stdout, "truncated") && !strings.Contains(out.Stdout, "截断") {
		t.Fatalf("stdout 应带截断标注, tail: %q", out.Stdout[len(out.Stdout)-80:])
	}
}
