package cli

// §5.5 端到端验收（local 连接真实执行）：uninstall 的 values 默认从主机
// marker 还原实际部署入参——修复 §1.1 "卸载静默空转并报告成功"。
//
//	deploy --set data_dir=/REAL → 创建 /REAL，marker v2 记录 values
//	uninstall（不带 --set）        → /REAL 被删除（此前：作用在默认路径上
//	                                 静默幂等成功，/REAL 原封不动）

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeProbeChart 构造验收 chart（deploy 建目录 / uninstall 删目录），
// 返回 (chartDir, 默认值路径, 实际部署路径, marker路径)。
func writeProbeChart(t *testing.T) (chartDir, defaultDir, realDir, markerPath string) {
	t.Helper()
	base := t.TempDir()
	defaultDir = filepath.Join(base, "DEFAULT")
	realDir = filepath.Join(base, "REAL")
	markerDir := filepath.Join(base, "markers")
	chartDir = filepath.Join(base, "probe")

	write := func(rel, content string) {
		p := filepath.Join(chartDir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("chart.yaml", "name: probe\nversion: 0.1.0\nmarker_dir: "+markerDir+"\n")
	write("values.yaml", "data_dir: "+defaultDir+"\n")
	write("values.schema.json", `{
  "type": "object",
  "properties": {"data_dir": {"type": "string"}},
  "required": ["data_dir"],
  "additionalProperties": false
}`)
	write("deploy.yaml", "- hosts: all\n  tasks:\n    - file: {path: \"{{ .data_dir }}\", state: directory}\n")
	write("uninstall.yaml", "- hosts: all\n  tasks:\n    - file: {path: \"{{ .data_dir }}\", state: absent}\n")
	return chartDir, defaultDir, realDir, filepath.Join(markerDir, "probe", "release.json")
}

// probeInventory 写 local 连接清单并挂到全局（runTarget 读 gInventories）。
func probeInventory(t *testing.T) func() {
	t.Helper()
	invPath := filepath.Join(t.TempDir(), "inventory.yaml")
	if err := os.WriteFile(invPath, []byte("all:\n  hosts:\n    localhost: {conn: local}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	old, oldExplicit := gInventories, gInventoryExplicit
	gInventories, gInventoryExplicit = []string{invPath}, true
	return func() { gInventories, gInventoryExplicit = old, oldExplicit }
}

// TestUninstallUsesMarkerValues §1.1 验收：卸载不带 --set 时 values 取自
// marker，实际部署的目录被删除，默认值路径从未被触碰。
func TestUninstallUsesMarkerValues(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // 隔离 release 记录落盘
	restore := probeInventory(t)
	defer restore()
	chartDir, defaultDir, realDir, markerPath := writeProbeChart(t)
	ctx := context.Background()

	// deploy：--set 指向 REAL（默认值是另一个从未存在的路径）
	if err := runTarget(ctx, chartDir, runOptions{phase: "deploy", yes: true,
		setArgs: []string{"data_dir=" + realDir}}); err != nil {
		t.Fatalf("deploy 失败: %v", err)
	}
	if fi, err := os.Stat(realDir); err != nil || !fi.IsDir() {
		t.Fatalf("deploy 应创建 %s: %v", realDir, err)
	}
	// marker v2 记录了实际 values
	mk, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatalf("marker 未写入: %v", err)
	}
	if !strings.Contains(string(mk), realDir) || !strings.Contains(string(mk), `"marker_schema": 2`) {
		t.Fatalf("marker v2 应记录 resolved values: %s", mk)
	}
	if fi, _ := os.Stat(markerPath); fi != nil && fi.Mode().Perm() != 0o600 {
		t.Fatalf("marker 权限应 0600: %v", fi.Mode())
	}

	// uninstall：不带 --set——此前静默空转报成功，REAL 原封不动
	if err := runTarget(ctx, chartDir, runOptions{phase: "uninstall", yes: true}); err != nil {
		t.Fatalf("uninstall 失败: %v", err)
	}
	if _, err := os.Stat(realDir); !os.IsNotExist(err) {
		t.Fatalf("§1.1 验收失败：values 应从 marker 还原并删除 %s", realDir)
	}
	if _, err := os.Stat(defaultDir); err == nil {
		t.Fatal("默认值路径不应被创建")
	}
	// 卸载成功后 marker 清除
	if _, err := os.Stat(markerPath); !os.IsNotExist(err) {
		t.Fatalf("uninstall 后 marker 应清除: %v", err)
	}
}

// TestUninstallRejectsInvalidValues §1.2 验收：uninstall 传 schema 非法值
// 被拒绝（此前唯一会删除数据的相位是唯一不校验入参的相位）。
func TestUninstallRejectsInvalidValues(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	restore := probeInventory(t)
	defer restore()
	chartDir, _, realDir, _ := writeProbeChart(t)
	ctx := context.Background()

	if err := runTarget(ctx, chartDir, runOptions{phase: "deploy", yes: true,
		setArgs: []string{"data_dir=" + realDir}}); err != nil {
		t.Fatalf("deploy 失败: %v", err)
	}
	// data_dir=999 在 deploy 会被 schema 拒绝；uninstall 也必须拒绝
	err := runTarget(ctx, chartDir, runOptions{phase: "uninstall", yes: true,
		setArgs: []string{"data_dir=999"}})
	if err == nil || !strings.Contains(err.Error(), "schema") {
		t.Fatalf("uninstall 应拒绝 schema 非法值: %v", err)
	}
	// 拒绝发生在执行前：目录原样
	if _, serr := os.Stat(realDir); serr != nil {
		t.Fatalf("被拒绝的 uninstall 不应执行任务: %v", serr)
	}
}

// TestUninstallV1MarkerRejected：v1 marker（无 resolved values）在卸载时
// 明确报错并提示先 deploy 升级，不静默回退默认值。
func TestUninstallV1MarkerRejected(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	restore := probeInventory(t)
	defer restore()
	chartDir, _, realDir, markerPath := writeProbeChart(t)
	ctx := context.Background()

	if err := runTarget(ctx, chartDir, runOptions{phase: "deploy", yes: true,
		setArgs: []string{"data_dir=" + realDir}}); err != nil {
		t.Fatalf("deploy 失败: %v", err)
	}
	// 篡改为 v1 marker（旧版 wdp 写入的形态：无 marker_schema / values）
	v1 := `{"chart":"probe","version":"0.1.0","phase":"deploy","deployed_at":"2026-01-01T00:00:00Z","values_sha256":"abc","wdp_version":"0.3.0"}`
	if err := os.WriteFile(markerPath, []byte(v1), 0o600); err != nil {
		t.Fatal(err)
	}
	err := runTarget(ctx, chartDir, runOptions{phase: "uninstall", yes: true})
	if err == nil || !strings.Contains(err.Error(), "older wdp") {
		t.Fatalf("v1 marker 应报错并提示升级: %v", err)
	}
	if _, serr := os.Stat(realDir); serr != nil {
		t.Fatalf("被拒绝的 uninstall 不应执行任务: %v", serr)
	}
}

// TestUninstallMissingMarkerRejected：marker 缺失时报错，不静默回退。
func TestUninstallMissingMarkerRejected(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	restore := probeInventory(t)
	defer restore()
	chartDir, _, _, markerPath := writeProbeChart(t)
	ctx := context.Background()

	// 从未 deploy：无 marker
	err := runTarget(ctx, chartDir, runOptions{phase: "uninstall", yes: true})
	if err == nil || !strings.Contains(err.Error(), "no release marker") {
		t.Fatalf("marker 缺失应报错: %v", err)
	}
	_ = markerPath
}
