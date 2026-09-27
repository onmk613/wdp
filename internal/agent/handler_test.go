package agent

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
)

// TestHandleUploadBodyLimit 上传请求体上限的边界行为：恰好等于上限接受、
// 超出上限 413 且目标不落盘、失败后无临时文件残留（413 判定由
// limitedReader 哨兵错误驱动，经 fsatomic 的失败清理路径删临时件）。
func TestHandleUploadBodyLimit(t *testing.T) {
	s := New(":0")
	s.SetMaxRequestBody(1) // 1 MiB
	limit := s.maxRequestBodyLimit()
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	dir := t.TempDir()
	put := func(name string, size int64) *http.Response {
		t.Helper()
		dst := filepath.Join(dir, name)
		req, err := http.NewRequest(http.MethodPut,
			ts.URL+"/file?path="+url.QueryEscape(dst),
			io.LimitReader(zeroReader{}, size))
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp
	}

	if resp := put("exact.bin", limit); resp.StatusCode != http.StatusOK {
		t.Fatalf("恰好等于上限应接受, 实际 %d", resp.StatusCode)
	}
	if st, err := os.Stat(filepath.Join(dir, "exact.bin")); err != nil || st.Size() != limit {
		t.Fatalf("目标文件应完整落盘: %v %v", st, err)
	}

	if resp := put("over.bin", limit+1); resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("超出上限应 413, 实际 %d", resp.StatusCode)
	}
	if _, err := os.Stat(filepath.Join(dir, "over.bin")); !os.IsNotExist(err) {
		t.Fatalf("被拒上传不应落盘: %v", err)
	}
	matches, err := filepath.Glob(filepath.Join(dir, ".wdp-fsatomic-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("失败路径残留临时文件: %v", matches)
	}
}

// zeroReader 提供任意长度的零字节流（大 body 构造用，不占内存）。
type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}
