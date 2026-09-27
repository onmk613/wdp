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
	assertTypeOncePerFamily(t, body)
}

// TestRenderMetricsTypeOncePerFamily 回归：同一 family 的 TYPE 行恰输出
// 一次、同名样本连续。此前按"与前一个样本同名才跳过"去重，多挂载点/
// 多设备的交错指标名（size/avail/files/files_free × 每挂载点）会让同一
// family 的 TYPE 行输出多次，Prometheus 抓取端按 "second TYPE line"
// 拒收整个 scrape。
func TestRenderMetricsTypeOncePerFamily(t *testing.T) {
	// 模拟 collectFilesystem 两个挂载点的交错输出顺序（修复前的实际形态）
	in := []sample{
		{name: "node_filesystem_size_bytes", labels: `{mount="/a"}`, value: 1},
		{name: "node_filesystem_avail_bytes", labels: `{mount="/a"}`, value: 2},
		{name: "node_filesystem_files", labels: `{mount="/a"}`, value: 3},
		{name: "node_filesystem_size_bytes", labels: `{mount="/b"}`, value: 4},
		{name: "node_filesystem_avail_bytes", labels: `{mount="/b"}`, value: 5},
	}
	assertTypeOncePerFamily(t, renderMetrics(in))
}

// assertTypeOncePerFamily 校验 Prometheus 文本的两个格式不变量：
// 每个 family 的 TYPE 行至多一条；同一 family 的样本行连续。
func assertTypeOncePerFamily(t *testing.T, body string) {
	t.Helper()
	seenType := map[string]bool{}
	closed := map[string]bool{} // 已被其他 family 打断的 family 不允许再出现
	cur := ""
	for _, line := range strings.Split(strings.TrimSpace(body), "\n") {
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "# TYPE ") {
			name := strings.TrimSuffix(strings.TrimPrefix(line, "# TYPE "), " untyped")
			if seenType[name] {
				t.Fatalf("family %s 的 TYPE 行重复:\n%s", name, body)
			}
			if closed[name] {
				t.Fatalf("family %s 的样本不连续（TYPE 之外再次出现）:\n%s", name, body)
			}
			seenType[name] = true
			cur = name
			continue
		}
		if strings.HasPrefix(line, "#") {
			continue
		}
		name := line
		if i := strings.IndexByte(name, '{'); i >= 0 {
			name = name[:i]
		} else if i := strings.IndexByte(name, ' '); i >= 0 {
			name = name[:i]
		}
		if name != cur {
			closed[cur] = true
			if seenType[name] {
				t.Fatalf("family %s 的样本不连续（离开 %s 后再次出现）:\n%s", name, cur, body)
			}
		}
	}
}
