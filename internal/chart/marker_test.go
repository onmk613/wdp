package chart

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// marker v2：记录 resolved values；敏感键脱敏且不参与摘要；v1 兼容解析。

func TestMarkerV2ContainsValues(t *testing.T) {
	c, err := Load(writeLifecycleChart(t))
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]any{"app": map[string]any{"port": 8080}, "data_dir": "/srv/app"}
	b := c.MarkerContent("0.4.0", values, "deploy")
	mk, err := ParseMarker(b)
	if err != nil {
		t.Fatal(err)
	}
	if mk.Schema() != MarkerSchemaV2 {
		t.Fatalf("marker schema: %d", mk.Schema())
	}
	if mk.Values["data_dir"] != "/srv/app" {
		t.Fatalf("marker v2 应记录 resolved values: %+v", mk.Values)
	}
	if mk.ValuesSHA != ValuesDigest(values) {
		t.Fatal("无敏感键时摘要应等于全量摘要")
	}
}

// TestMarkerV1Detected：旧版 marker（无 marker_schema 字段）按 v1 识别，
// Values 为空——非部署相位据此拒绝还原而不是静默用默认值。
func TestMarkerV1Detected(t *testing.T) {
	v1 := `{"chart":"myapp","version":"1.0.0","phase":"deploy","deployed_at":"2026-01-01T00:00:00Z","values_sha256":"abc123","wdp_version":"0.3.0"}`
	mk, err := ParseMarker([]byte(v1))
	if err != nil {
		t.Fatal(err)
	}
	if mk.Schema() != MarkerSchemaV1 {
		t.Fatalf("缺失 marker_schema 应按 v1: %+v", mk)
	}
	if len(mk.Values) != 0 {
		t.Fatalf("v1 marker 无 resolved values: %+v", mk.Values)
	}
}

// TestMarkerSensitiveRedaction：sensitive_values 白名单键以 <redacted> 落盘，
// 且不参与摘要（真实敏感值变化不构成 drift 信号）。
func TestMarkerSensitiveRedaction(t *testing.T) {
	dir := writeLifecycleChart(t)
	if err := os.WriteFile(filepath.Join(dir, "chart.yaml"), []byte(`
name: myapp
version: 1.0.0
required: [app.port, db.host]
marker_dir: /tmp/wdp-test-marker
sensitive_values: [db.password]
`), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]any{
		"db": map[string]any{"host": "10.0.0.1", "password": "s3cret"},
	}
	b := string(c.MarkerContent("0.4.0", values, "deploy"))
	if strings.Contains(b, "s3cret") {
		t.Fatalf("敏感值泄漏进 marker: %s", b)
	}
	if !strings.Contains(b, RedactedValue) {
		t.Fatalf("敏感键应落盘占位: %s", b)
	}
	// 摘要剔除敏感键：真实密码变化不影响摘要
	other := map[string]any{
		"db": map[string]any{"host": "10.0.0.1", "password": "different"},
	}
	if c.ValuesDigestOf(values) != c.ValuesDigestOf(other) {
		t.Fatal("敏感键应不参与摘要")
	}
	// 剔除路径本身可用（drift 口径共用）
	stripped := c.StripSensitive(values)
	db := stripped["db"].(map[string]any)
	if _, exists := db["password"]; exists {
		t.Fatalf("StripSensitive 应剔除敏感键: %+v", stripped)
	}
	// RedactValues：控制端审计记录 / plan 快照共用同一脱敏口径
	red := c.RedactValues(values)
	if red["db"].(map[string]any)["password"] != RedactedValue {
		t.Fatalf("RedactValues 应替换敏感键: %+v", red)
	}
	if values["db"].(map[string]any)["password"] != "s3cret" {
		t.Fatal("RedactValues 不应修改入参")
	}
	// 无敏感声明时也返回深拷贝（不共享嵌套引用）
	plain, err := Load(writeLifecycleChart(t))
	if err != nil {
		t.Fatal(err)
	}
	cp := plain.RedactValues(values)
	cp["db"].(map[string]any)["password"] = "mutated"
	if values["db"].(map[string]any)["password"] != "s3cret" {
		t.Fatal("RedactValues 应返回深拷贝")
	}
}

// TestMarkerRoundTrip：MarkerContent → ParseMarker 往返字段一致。
func TestMarkerRoundTrip(t *testing.T) {
	c, err := Load(writeLifecycleChart(t))
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]any{"k": "v"}
	mk, err := ParseMarker(c.MarkerContent("1.2.3", values, "update"))
	if err != nil {
		t.Fatal(err)
	}
	if mk.Chart != "myapp" || mk.Version != "1.0.0" || mk.Phase != "update" || mk.WdpVersion != "1.2.3" {
		t.Fatalf("roundtrip: %+v", mk)
	}
	var raw map[string]any
	if err := json.Unmarshal(c.MarkerContent("1.2.3", values, "update"), &raw); err != nil {
		t.Fatal(err)
	}
	if raw["marker_schema"].(float64) != 2 {
		t.Fatalf("marker_schema 应显式为 2: %v", raw)
	}
}
