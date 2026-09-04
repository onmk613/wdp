package chart

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"wdp/internal/model"
)

// writeLifecycleChart 写出含 required/uninstall/status 的 chart。
func writeLifecycleChart(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	must := func(rel, content string) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	must("chart.yaml", `
name: myapp
version: 1.0.0
required: [app.port, db.host]
marker_dir: /tmp/wdp-test-marker
`)
	must("values.yaml", "app: {name: demo}\n")
	must("deploy.yaml", `
- hosts: all
  tasks:
    - copy: {content: x, dest: /etc/app.conf}
    - file: {path: /data, state: directory}
    - shell: 'echo hi'
    - setup:
    - package: {name: nginx, state: present}
`)
	must("uninstall.yaml", `
- hosts: all
  tasks:
    - file: {path: /etc/app.conf, state: absent}
`)
	must("status.yaml", `
- hosts: all
  tasks:
    - shell: 'cat /etc/app.conf'
`)
	return dir
}

func TestLifecycleLoad(t *testing.T) {
	c, err := Load(writeLifecycleChart(t))
	if err != nil {
		t.Fatal(err)
	}
	if c.Phases["uninstall"] == nil || len(c.Phases["uninstall"]) != 1 {
		t.Fatal("uninstall.yaml 未加载")
	}
	if c.Phases["status"] == nil || len(c.Phases["status"]) != 1 {
		t.Fatal("status.yaml 未加载")
	}
	if len(c.Meta.Required) != 2 {
		t.Fatalf("required: %v", c.Meta.Required)
	}
	if c.MarkerDir() != "/tmp/wdp-test-marker" {
		t.Fatal("marker_dir 未生效")
	}
	if c.MarkerPath() != "/tmp/wdp-test-marker/myapp/release.json" {
		t.Fatalf("marker path: %s", c.MarkerPath())
	}
	if !c.MarkerEnabled() {
		t.Fatal("marker 应默认启用")
	}
}

func TestValidateRequired(t *testing.T) {
	c, err := Load(writeLifecycleChart(t))
	if err != nil {
		t.Fatal(err)
	}
	// 缺 db.host
	values := map[string]any{"app": map[string]any{"port": 8080}}
	err = c.ValidateRequired(values)
	if err == nil || !strings.Contains(err.Error(), "db.host") {
		t.Fatalf("应报缺 db.host: %v", err)
	}
	// 补齐后通过
	values["db"] = map[string]any{"host": "10.0.0.1"}
	if err := c.ValidateRequired(values); err != nil {
		t.Fatal(err)
	}
	// 中间段非 map
	if err := c.ValidateRequired(map[string]any{"db": "scalar"}); err == nil {
		t.Fatal("中间段非 map 应报错")
	}
}

func TestAnalyze(t *testing.T) {
	c, err := Load(writeLifecycleChart(t))
	if err != nil {
		t.Fatal(err)
	}
	r := c.Analyze("deploy")
	// copy/file 2 可逆 + setup 1 只读 + shell/package 2 不可逆
	if r.Reversible != 2 || r.ReadOnly != 1 || r.Irreversible != 2 {
		t.Fatalf("统计异常: %+v", r)
	}
	if !r.HasUninstall || !r.HasStatus {
		t.Fatal("生命周期能力未识别")
	}
	if r.AutoRollback {
		t.Fatal("未配置 auto_rollback")
	}
	var irr int
	for _, row := range r.Rows() {
		if row.Label == "irreversible" {
			irr = row.Count
		}
	}
	if irr != 2 || !strings.Contains(r.LifecycleNote(), "uninstallable") {
		t.Fatalf("摘要行/生命周期: %+v %s", r.Rows(), r.LifecycleNote())
	}
}

// TestAnalyzePartialReversible 回归：unarchive 曾因硬编码名单遗漏被误判为
// 完全不可逆。现在按模块自声明能力归类为"部分可逆"。
func TestAnalyzePartialReversible(t *testing.T) {
	c := &Chart{Deploy: []*model.Play{{Tasks: []*model.Task{
		{Module: "unarchive"},
		{Module: "shell", FreeForm: "x"},
	}}}}
	r := c.Analyze("deploy")
	if r.Partial != 1 || r.Irreversible != 1 {
		t.Fatalf("unarchive 应计为部分可逆: %+v", r)
	}
	var partial int
	var note string
	for _, row := range r.Rows() {
		if row.Label == "partially reversible" {
			partial, note = row.Count, row.Note
		}
	}
	if partial != 1 || !strings.Contains(note, "overwritten files are not restored") {
		t.Fatalf("摘要应说明部分可逆边界: %+v", r.Rows())
	}
}

func TestMarkerContent(t *testing.T) {
	c, err := Load(writeLifecycleChart(t))
	if err != nil {
		t.Fatal(err)
	}
	b := string(c.MarkerContent("0.3.0", map[string]any{"a": 1}, "update"))
	for _, want := range []string{`"chart": "myapp"`, `"version": "1.0.0"`, `"phase": "update"`, `"wdp_version": "0.3.0"`} {
		if !strings.Contains(b, want) {
			t.Fatalf("marker 缺 %s: %s", want, b)
		}
	}
	// 空相位归一化为 deploy
	if b := string(c.MarkerContent("0.3.0", nil, "")); !strings.Contains(b, `"phase": "deploy"`) {
		t.Fatalf("空相位应按 deploy: %s", b)
	}
	if len(ValuesDigest(map[string]any{"a": 1})) != 12 {
		t.Fatal("摘要长度应 12")
	}
	// no_marker 禁用
	c.Meta.NoMarker = true
	if c.MarkerEnabled() {
		t.Fatal("no_marker 应禁用")
	}
}

// TestAutoRollbackDetection 验证 Analyze 识别策略回滚。
func TestAutoRollbackDetection(t *testing.T) {
	c := &Chart{Deploy: []*model.Play{{
		Strategy: &model.Strategy{Type: "canary", AutoRollback: true},
	}}}
	if !c.Analyze("deploy").AutoRollback {
		t.Fatal("未识别 auto_rollback")
	}
}
