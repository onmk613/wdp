package agent

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// TestMaxRequestBodyLimit --max-request-mb 注入的请求体上限应生效：
// 超限请求体被截断后 JSON 解码失败，返回 400。
func TestMaxRequestBodyLimit(t *testing.T) {
	s := New(":0")
	s.SetMaxRequestBody(1) // 1MiB
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	big := strings.Repeat("a", 2<<20) // 2MiB > 1MiB 上限
	body := fmt.Sprintf(`{"script": %s}`, strconv.Quote(big))
	resp, err := http.Post(ts.URL+"/exec", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("超限请求体应返回 400, got %v", resp.StatusCode)
	}

	// 默认上限（未设置）下同样请求不应因大小被拒（此处脚本为空，返回的是 400 空脚本——
	// 与超限同码，故改用小请求验证默认路径仍可用）
	small := New(":0")
	small.SetMaxRequestBody(0)
	ts2 := httptest.NewServer(small.Handler())
	t.Cleanup(ts2.Close)
	resp2, err := http.Post(ts2.URL+"/exec", "application/json", strings.NewReader(`{"script":"true"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("回退默认上限后正常请求应可用, got %v", resp2.StatusCode)
	}
}
