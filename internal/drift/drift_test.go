package drift

// drift 检测内核的单元测试：Classify 的五种结论、失败口径、字段级 diff
//（含脱敏占位不构成差异）、marker 读取 play 的只读形态。这些逻辑此前
// 零覆盖，而它是 `wdp drift` 与 CI 判失败口径的唯一依据。

import (
	"encoding/json"
	"strings"
	"testing"

	"wdp/internal/chart"
	"wdp/internal/model"
)

// markerJSON 构造一份 v2 marker（含 resolved values）。
func markerJSON(t *testing.T, version, valuesSHA string, values map[string]any) string {
	t.Helper()
	m := chart.Marker{
		Chart: "app", Version: version, Phase: "deploy",
		DeployedAt: "2026-01-01T00:00:00Z", ValuesSHA: valuesSHA,
		WdpVersion: "test", Values: values, SchemaVer: chart.MarkerSchemaV2,
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func testHosts(names ...string) []*model.Host {
	out := make([]*model.Host, 0, len(names))
	for _, n := range names {
		out = append(out, &model.Host{Name: n})
	}
	return out
}

// TestClassifyStates 五种结论与失败计数口径。
func TestClassifyStates(t *testing.T) {
	const wantSHA = "aaaa"
	values := map[string]any{"port": float64(8080)}
	hosts := testHosts("ok", "drifted", "outdated", "missing", "unreachable", "failed")
	results := map[string]*model.TaskResult{
		"ok":          {Stdout: markerJSON(t, "1.0.0", wantSHA, values)},
		"drifted":     {Stdout: markerJSON(t, "1.0.0", "bbbb", map[string]any{"port": float64(9090)})},
		"outdated":    {Stdout: markerJSON(t, "0.9.0", wantSHA, values)},
		"missing":     {Stdout: "__MISSING__\n"},
		"unreachable": {Unreachable: true, Msg: "dial tcp: timeout"},
		"failed":      {Failed: true, Msg: "read failed: permission denied"},
	}

	rows, failed := Classify(hosts, results, "1.0.0", wantSHA, values)
	got := map[string]Row{}
	for _, r := range rows {
		got[r.Host] = r
	}
	want := map[string]string{
		"ok": "OK", "drifted": "DRIFTED", "outdated": "OUTDATED",
		"missing": "NOT-DEPLOYED", "unreachable": "UNREACHABLE", "failed": "UNREACHABLE",
	}
	for host, state := range want {
		if got[host].State != state {
			t.Errorf("%s: state = %q, want %q (detail=%q)", host, got[host].State, state, got[host].Detail)
		}
	}
	// 失败口径：DRIFTED + UNREACHABLE（NOT-DEPLOYED/OUTDATED 只列出）
	if failed != 3 {
		t.Fatalf("failed = %d, want 3（drifted/unreachable/failed）", failed)
	}
	// DRIFTED 且 marker 为 v2 时附字段级差异
	if !strings.Contains(got["drifted"].Detail, "port") {
		t.Errorf("DRIFTED 应带字段级 diff: %q", got["drifted"].Detail)
	}
	// 缺失 marker 的行给出明确说明
	if got["missing"].Detail != "no release marker" {
		t.Errorf("NOT-DEPLOYED detail = %q", got["missing"].Detail)
	}
}

// TestClassifyMarkerUnreadable 坏 marker 按 DRIFTED 判失败（不能当成未部署）。
func TestClassifyMarkerUnreadable(t *testing.T) {
	rows, failed := Classify(testHosts("bad"), map[string]*model.TaskResult{
		"bad": {Stdout: "{not json"},
	}, "1.0.0", "aaaa", nil)
	if len(rows) != 1 || rows[0].State != "DRIFTED" || !strings.Contains(rows[0].Detail, "marker unreadable") {
		t.Fatalf("坏 marker 应判 DRIFTED: %+v", rows)
	}
	if failed != 1 {
		t.Fatalf("坏 marker 应计失败: %d", failed)
	}
}

// TestClassifyV1MarkerFallsBackToSummary v1 marker 没有 resolved values：
// 摘要一致判 OK，摘要不一致只给摘要口径（无字段级 diff）。
func TestClassifyV1MarkerFallsBackToSummary(t *testing.T) {
	v1 := `{"chart":"app","version":"1.0.0","phase":"deploy","deployed_at":"2026-01-01T00:00:00Z","values_sha256":"aaaa","wdp_version":"0.3.0"}`
	rows, failed := Classify(testHosts("h"), map[string]*model.TaskResult{
		"h": {Stdout: v1},
	}, "1.0.0", "aaaa", map[string]any{"port": 1})
	if failed != 0 || rows[0].State != "OK" {
		t.Fatalf("v1 marker 摘要一致应 OK: %+v failed=%d", rows, failed)
	}
	rows, _ = Classify(testHosts("h"), map[string]*model.TaskResult{
		"h": {Stdout: v1},
	}, "1.0.0", "bbbb", map[string]any{"port": 1})
	if rows[0].State != "DRIFTED" || strings.Contains(rows[0].Detail, "fields:") {
		t.Fatalf("v1 marker 应只给摘要口径: %+v", rows[0])
	}
}

// TestValuesDiff 递归字段差异：值变化/键删除/新增/嵌套，脱敏占位不参与。
func TestValuesDiff(t *testing.T) {
	deployed := map[string]any{
		"port": float64(8080),
		"db":   map[string]any{"host": "10.0.0.1", "password": chart.RedactedValue},
		"gone": "x",
	}
	current := map[string]any{
		"port": float64(9090),
		"db":   map[string]any{"host": "10.0.0.1", "password": "s3cret"},
		"new":  "y",
	}
	diffs := ValuesDiff(deployed, current)
	joined := strings.Join(diffs, "; ")
	for _, want := range []string{"port: 8080 → 9090", "gone: x → (removed)", "new: (absent) → y"} {
		if !strings.Contains(joined, want) {
			t.Errorf("缺少差异 %q：%s", want, joined)
		}
	}
	if strings.Contains(joined, "password") {
		t.Errorf("脱敏占位不应构成差异（也不应泄漏真实值）: %s", joined)
	}
	// 相同值无差异
	if d := ValuesDiff(deployed, map[string]any{
		"port": float64(8080), "gone": "x",
		"db": map[string]any{"host": "10.0.0.1", "password": chart.RedactedValue},
	}); len(d) != 0 {
		t.Fatalf("相同 values 应无差异: %v", d)
	}
}

// TestReadMarkerPlay marker 读取 play 只读且需要 become（marker 0600）。
func TestReadMarkerPlay(t *testing.T) {
	p := ReadMarkerPlay("app", "/var/lib/wdp/app/release.json", "web*")
	if !p.Become {
		t.Error("读取 0600 marker 需要 become")
	}
	if p.Hosts != "web*" {
		t.Errorf("hosts 模式应透传: %q", p.Hosts)
	}
	if len(p.Tasks) != 1 || p.Tasks[0].Module != "shell" {
		t.Fatalf("应是一条 shell 任务: %+v", p.Tasks)
	}
	// 引用路径必须被引号包裹（防注入），且失败时输出哨兵
	ff := p.Tasks[0].FreeForm
	if !strings.Contains(ff, "'/var/lib/wdp/app/release.json'") {
		t.Errorf("marker 路径应经 shell 引号: %q", ff)
	}
	if !strings.Contains(ff, "__MISSING__") {
		t.Errorf("缺失 marker 应输出哨兵: %q", ff)
	}
	if p.Tasks[0].ChangedWhen != "{{ false }}" {
		t.Errorf("读取任务不应产生变更: %q", p.Tasks[0].ChangedWhen)
	}
}
