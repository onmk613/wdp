package dumphttp

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestRedact 敏感字段遮蔽：密码/私钥类键的值被替换，其余内容原样。
func TestRedact(t *testing.T) {
	in := `{"script":"echo hi","become_password":"s3cret","password":"p@ss","key":"-----BEGIN PRIVATE KEY-----","name":"ok"}`
	out := Redact(in)
	for _, secret := range []string{"s3cret", "p@ss", "PRIVATE KEY"} {
		if strings.Contains(out, secret) {
			t.Fatalf("敏感值应被遮蔽: %s", out)
		}
	}
	if !strings.Contains(out, `"script":"echo hi"`) || !strings.Contains(out, `"name":"ok"`) {
		t.Fatalf("非敏感内容应保留: %s", out)
	}
}

// TestRequestRestoresBody 转储后请求体可被完整读取（含超过上限的长体）。
func TestRequestRestoresBody(t *testing.T) {
	long := strings.Repeat("a", MaxBodyBytes*3)
	r := httptest.NewRequest("POST", "/exec", strings.NewReader(long))
	dump := Request(r)
	if !strings.Contains(dump, "POST /exec") {
		t.Fatalf("应含请求行: %q", dump)
	}
	if !strings.Contains(dump, "body truncated") {
		t.Fatalf("超限体应标注截断: %q", tail(dump, 200))
	}
	got, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, []byte(long)) {
		t.Fatalf("转储后体应完整可读，实际 %d 字节", len(got))
	}
}

// TestResponseBounded 响应格式化含状态行与受限体标注。
func TestResponseBounded(t *testing.T) {
	body := strings.Repeat("x", MaxBodyBytes+100)
	out := Response(200, map[string][]string{"Content-Type": {"application/json"}}, []byte(body[:MaxBodyBytes]), true)
	if !strings.Contains(out, "HTTP/1.1 200") || !strings.Contains(out, "Content-Type: application/json") {
		t.Fatalf("应含状态行与头: %q", out)
	}
	if !strings.Contains(out, "body truncated") {
		t.Fatalf("截断应标注: %q", tail(out, 200))
	}
}

// TestRecorderLimitsBody Recorder 只记录受限体，业务写入不受影响。
func TestRecorderLimitsBody(t *testing.T) {
	w := httptest.NewRecorder()
	rec := NewRecorder(w)
	payload := strings.Repeat("z", MaxBodyBytes*2)
	n, err := rec.Write([]byte(payload))
	if err != nil || n != len(payload) {
		t.Fatalf("底层写入应完整: n=%d err=%v", n, err)
	}
	if !rec.Truncated {
		t.Fatal("超限应标记截断")
	}
	if rec.Status() != http.StatusOK {
		t.Fatalf("默认状态应为 200，实际 %d", rec.Status())
	}
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
