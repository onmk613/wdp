package dumphttp

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
)

// MaxBodyBytes 是单个报文体最多转储的字节数
const MaxBodyBytes = 4 << 10

// sensitiveRe 匹配 JSON 体中敏感键的字符串值
var sensitiveRe = regexp.MustCompile(`(?i)"(become_?password|password|passphrase|secret|token|key)"(\s*:\s*)"(?:[^"\\]|\\.)*"`)

// Redact 遮蔽文本中 JSON 敏感字段的值。
func Redact(s string) string {
	return sensitiveRe.ReplaceAllString(s, `"$1"$2"***"`)
}

// Request 格式化请求报文
func Request(r *http.Request) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s %s\r\n", r.Method, r.URL.RequestURI(), r.Proto)
	if r.Host != "" {
		fmt.Fprintf(&b, "Host: %s\r\n", r.Host)
	}
	for k, vv := range r.Header {
		for _, v := range vv {
			fmt.Fprintf(&b, "%s: %s\r\n", k, v)
		}
	}
	if r.Body != nil {
		buf := make([]byte, MaxBodyBytes+1)
		n, _ := io.ReadFull(r.Body, buf)
		full := buf[:n]
		// 还原完整已读部分（含用于判断截断的末字节），业务侧读流不缺数据
		r.Body = restoreBody{
			Reader: io.MultiReader(bytes.NewReader(full), r.Body),
			orig:   r.Body,
		}
		shown, truncated := full, false
		if len(shown) > MaxBodyBytes {
			shown, truncated = shown[:MaxBodyBytes], true
		}
		b.WriteString("\r\n")
		b.WriteString(Redact(string(shown)))
		if truncated {
			fmt.Fprintf(&b, "\r\n...[body truncated at %d bytes]", MaxBodyBytes)
		}
	}
	return b.String()
}

// Response 格式化响应报文
func Response(status int, h http.Header, body []byte, truncated bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "HTTP/1.1 %d %s\r\n", status, http.StatusText(status))
	for k, vv := range h {
		for _, v := range vv {
			fmt.Fprintf(&b, "%s: %s\r\n", k, v)
		}
	}
	b.WriteString("\r\n")
	b.WriteString(Redact(string(body)))
	if truncated {
		fmt.Fprintf(&b, "\r\n...[body truncated at %d bytes]", MaxBodyBytes)
	}
	return b.String()
}

// restoreBody 把预读的体拼回原流
type restoreBody struct {
	io.Reader
	orig io.Closer
}

func (b restoreBody) Close() error { return b.orig.Close() }

// Recorder 包装 ResponseWriter，记录状态码与受限响应体
type Recorder struct {
	http.ResponseWriter
	status    int
	buf       bytes.Buffer
	Truncated bool
}

// NewRecorder 包装 w 开始记录。
func NewRecorder(w http.ResponseWriter) *Recorder {
	return &Recorder{ResponseWriter: w, status: http.StatusOK}
}

// Status 返回已发送的状态码。
func (r *Recorder) Status() int { return r.status }

// Body 返回已记录的受限响应体。
func (r *Recorder) Body() []byte { return r.buf.Bytes() }

func (r *Recorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *Recorder) Write(p []byte) (int, error) {
	if r.buf.Len() < MaxBodyBytes {
		room := MaxBodyBytes - r.buf.Len()
		if len(p) <= room {
			r.buf.Write(p)
		} else {
			r.buf.Write(p[:room])
			r.Truncated = true
		}
	} else if len(p) > 0 {
		r.Truncated = true
	}
	return r.ResponseWriter.Write(p)
}

// Flush 透传给底层 writer（流式响应兼容）。
func (r *Recorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
