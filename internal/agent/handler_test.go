package agent

import (
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
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

// TestHandleUploadModeParam mode 参数的严格解析（与模块层 argMode 同口径）。
// 回归：旧实现 fmt.Sscanf("%o") 接受尾随垃圾（"0755abc" 静默按 0755）
// 且无上界（4755 被 Perm() 静默剥成 0755 之外的错误行为）。
func TestHandleUploadModeParam(t *testing.T) {
	s := New(":0")
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	dir := t.TempDir()
	put := func(name, mode string) int {
		t.Helper()
		q := url.Values{"path": {filepath.Join(dir, name)}}
		if mode != "" {
			q.Set("mode", mode)
		}
		req, err := http.NewRequest(http.MethodPut, ts.URL+"/file?"+q.Encode(), strings.NewReader("x"))
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.StatusCode
	}

	cases := []struct {
		mode   string
		status int
		want   fs.FileMode
	}{
		{"644", http.StatusOK, 0o644},         // agentc 实际下发形态（%o 无前导零）
		{"0755", http.StatusOK, 0o755},        // 前导零同样合法
		{"", http.StatusOK, 0o644},            // 缺省 0644
		{"0755abc", http.StatusBadRequest, 0}, // 尾随垃圾：旧实现静默按 0755
		{"4755", http.StatusBadRequest, 0},    // setuid 位超出权限位域
		{"999", http.StatusBadRequest, 0},     // 非八进制数字
		{"-1", http.StatusBadRequest, 0},      // 负数
	}
	for _, c := range cases {
		name := "f-" + c.mode
		if c.mode == "" {
			name = "f-default"
		}
		if got := put(name, c.mode); got != c.status {
			t.Errorf("mode=%q: status = %d, want %d", c.mode, got, c.status)
			continue
		}
		if c.status != http.StatusOK {
			if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
				t.Errorf("mode=%q: 被拒上传不应落盘", c.mode)
			}
			continue
		}
		st, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("mode=%q: %v", c.mode, err)
		}
		if st.Mode().Perm() != c.want {
			t.Errorf("mode=%q: 落盘权限 = %#o, want %#o", c.mode, st.Mode().Perm(), c.want)
		}
	}
}
