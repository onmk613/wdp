package agent

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

// TestMetricsEndpoint /metrics 输出 Prometheus 文本格式：命名对齐
// node_exporter、标签形态合法、darwin 兜底至少有 load/fs/time。
func TestMetricsEndpoint(t *testing.T) {
	s := &Server{}
	rec := httptest.NewRecorder()
	s.handleMetrics(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("应 200: %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/plain") {
		t.Fatalf("Content-Type 异常: %s", ct)
	}
	body := rec.Body.String()
	nameRe := regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*(\{[^\n]*\})? [0-9.e+-]+$`)
	for _, line := range strings.Split(strings.TrimSpace(body), "\n") {
		if strings.HasPrefix(line, "#") || line == "" {
			continue
		}
		if !nameRe.MatchString(line) {
			t.Fatalf("非法指标行: %q", line)
		}
	}
	for _, want := range []string{"node_time_seconds", "node_filesystem_size_bytes"} {
		if !strings.Contains(body, want) {
			t.Fatalf("缺 %s:\n%s", want, body)
		}
	}
}
