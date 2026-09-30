package agent

// GET /info 能力自描述（分层方案 P2）：模块集从注册表实时派生、build 与
// /health 同源、免认证可达（探测链路在明文回退场景无客户端证书）。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"wdp/internal/plan"
)

func TestHandleInfo(t *testing.T) {
	s := New(":0")
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/info")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/info 应 200, 实际 %d", resp.StatusCode)
	}
	var info struct {
		Build   string   `json:"build"`
		Tier    string   `json:"tier"`
		Proto   int      `json:"proto"`
		PlanSch int      `json:"plan_schema"`
		Modules []string `json:"modules"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		t.Fatal(err)
	}
	if info.Proto != 1 {
		t.Fatalf("proto = %d, want 1", info.Proto)
	}
	if info.Build == "" {
		t.Fatal("build 不应为空（与 /health 同源）")
	}
	if info.PlanSch != plan.SchemaVer {
		t.Fatalf("plan_schema = %d, want %d", info.PlanSch, plan.SchemaVer)
	}
	// 模块集从注册表派生：内置 shell/file 必在，且有序
	if !slices.Contains(info.Modules, "shell") || !slices.Contains(info.Modules, "file") {
		t.Fatalf("模块集应含内置 shell/file: %v", info.Modules)
	}
	if !slices.IsSorted(info.Modules) {
		t.Fatalf("模块集应有序: %v", info.Modules)
	}

	// build 与 /health 同源（升级门控与能力上报不各自为政）
	hresp, err := http.Get(ts.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	defer hresp.Body.Close()
	var health struct {
		Build string `json:"build"`
	}
	if err := json.NewDecoder(hresp.Body).Decode(&health); err != nil {
		t.Fatal(err)
	}
	if info.Build != health.Build {
		t.Fatalf("/info build %q != /health build %q", info.Build, health.Build)
	}
}
