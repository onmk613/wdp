package cli

// §6.4 P1 端到端验证：`wdp plan` → `wdp apply` 与 `wdp run` 在同一 chart 上
// 产生相同的执行结果（本地 local 连接真实执行，比对落盘效果与 marker）。

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"wdp/internal/chart"
)

// planApplyEnv 准备 chart + local inventory，返回编译/执行所需的上下文。
func planApplyEnv(t *testing.T) (chartDir, realDir string, restore func()) {
	t.Helper()
	restoreInv := probeInventory(t)
	chartDir, _, realDir, _ = writeProbeChart(t)
	return chartDir, realDir, restoreInv
}

// withPlanOpts 临时设置 plan 编译参数。
func withPlanOpts(t *testing.T, phase string, sets []string) {
	t.Helper()
	old := planOpts
	planOpts.limit = ""
	planOpts.phase = phase
	planOpts.factCache = ""
	planOpts.valuesFiles = nil
	planOpts.setArgs = sets
	t.Cleanup(func() { planOpts = old })
}

// TestPlanApplyEqualsRun deploy 相位：run 与 plan+apply 产生相同的落盘效果
// 与 marker 内容。
func TestPlanApplyEqualsRun(t *testing.T) {
	ctx := context.Background()

	// 同一 chart、同一 data_dir，先后走两条路径：run → 清场 → plan+apply，
	// 比对落盘效果与 marker（相同入参 ⇒ 相同 values 摘要）
	t.Setenv("HOME", t.TempDir())
	chartDir, realDir, restore := planApplyEnv(t)

	// --- 路径 A：wdp run ---
	if err := runTarget(ctx, chartDir, runOptions{phase: "deploy", yes: true,
		setArgs: []string{"data_dir=" + realDir}}); err != nil {
		t.Fatalf("run 失败: %v", err)
	}
	markerPath := findMarker(t, chartDir)
	var runResult chart.Marker
	if b, err := os.ReadFile(markerPath); err != nil {
		t.Fatalf("run 后 marker 应存在: %v", err)
	} else if err := json.Unmarshal(b, &runResult); err != nil {
		t.Fatal(err)
	}
	// 清场：删除 marker 与目录，两条路径面对相同初始现场
	if err := os.Remove(markerPath); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(realDir); err != nil {
		t.Fatal(err)
	}

	// --- 路径 B：wdp plan → wdp apply ---
	planPath := filepath.Join(t.TempDir(), "plan.json")
	withPlanOpts(t, "deploy", []string{"data_dir=" + realDir})
	planOut = planPath
	defer func() { planOut = "" }()
	if err := runPlanCompile(ctx, chartDir); err != nil {
		t.Fatalf("plan 编译失败: %v", err)
	}
	if _, err := os.Stat(planPath); err != nil {
		t.Fatalf("plan.json 未写出: %v", err)
	}
	if err := runApply(ctx, planPath, applyOptions{yes: true}); err != nil {
		t.Fatalf("apply 失败: %v", err)
	}
	restore()

	// --- 比对：目录、marker values 摘要一致 ---
	if fi, err := os.Stat(realDir); err != nil || !fi.IsDir() {
		t.Fatalf("apply 应创建 %s: %v", realDir, err)
	}
	var applyResult chart.Marker
	if b, err := os.ReadFile(markerPath); err != nil {
		t.Fatalf("apply 后 marker 应存在: %v", err)
	} else if err := json.Unmarshal(b, &applyResult); err != nil {
		t.Fatal(err)
	}
	if applyResult.ValuesSHA != runResult.ValuesSHA {
		t.Fatalf("run 与 apply 的 values 摘要应一致: %s != %s",
			runResult.ValuesSHA, applyResult.ValuesSHA)
	}
	if applyResult.Values["data_dir"] != realDir || runResult.Values["data_dir"] != realDir {
		t.Fatalf("marker values 应记录实际入参: %+v vs %+v", runResult.Values, applyResult.Values)
	}
	if applyResult.Chart != runResult.Chart || applyResult.Phase != runResult.Phase {
		t.Fatalf("marker 元数据不一致: %+v vs %+v", runResult, applyResult)
	}
}

// TestPlanApplyUninstallFromMarker uninstall 相位：plan 编译从 marker 还原
// values，apply 后实际部署目录被删除（§1.1 修复在 plan/apply 路径同样成立）。
func TestPlanApplyUninstallFromMarker(t *testing.T) {
	ctx := context.Background()
	t.Setenv("HOME", t.TempDir())
	chartDir, realDir, restore := planApplyEnv(t)

	if err := runTarget(ctx, chartDir, runOptions{phase: "deploy", yes: true,
		setArgs: []string{"data_dir=" + realDir}}); err != nil {
		t.Fatalf("deploy 失败: %v", err)
	}
	planPath := filepath.Join(t.TempDir(), "uninstall-plan.json")
	withPlanOpts(t, "uninstall", nil)
	planOut = planPath
	defer func() { planOut = "" }()
	// 不带 --set 编译 uninstall 计划：values 从 marker 还原（需读取主机 marker）
	if err := runPlanCompile(ctx, chartDir); err != nil {
		t.Fatalf("uninstall plan 编译失败: %v", err)
	}
	if err := runApply(ctx, planPath, applyOptions{yes: true}); err != nil {
		t.Fatalf("apply uninstall 失败: %v", err)
	}
	restore()
	if _, err := os.Stat(realDir); !os.IsNotExist(err) {
		t.Fatalf("plan+apply uninstall 应删除 %s", realDir)
	}
}

// TestPlanCompileOfflineCLI 断网编译：inventory 指向不可达地址也能产出 plan
// （部署相位完全不连接主机）。
func TestPlanCompileOfflineCLI(t *testing.T) {
	ctx := context.Background()
	chartDir, _, _ := planApplyEnv(t)
	// 改用不可达主机的清单（conn ssh 指向 TEST-NET 地址）
	invDir := t.TempDir()
	invPath := filepath.Join(invDir, "inv.yaml")
	if err := os.WriteFile(invPath, []byte("all:\n  hosts:\n    dead: {ansible_host: 203.0.113.1}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	old, oldExplicit := gInventories, gInventoryExplicit
	gInventories, gInventoryExplicit = []string{invPath}, true
	defer func() { gInventories, gInventoryExplicit = old, oldExplicit }()

	planPath := filepath.Join(invDir, "plan.json")
	withPlanOpts(t, "deploy", nil)
	planOut = planPath
	defer func() { planOut = "" }()
	if err := runPlanCompile(ctx, chartDir); err != nil {
		t.Fatalf("离线编译失败: %v", err)
	}
	if _, err := os.Stat(planPath); err != nil {
		t.Fatalf("plan 未写出: %v", err)
	}

	// 同参数再编译一次：逐字节相同
	planPath2 := filepath.Join(invDir, "plan2.json")
	planOut = planPath2
	if err := runPlanCompile(ctx, chartDir); err != nil {
		t.Fatal(err)
	}
	a, _ := os.ReadFile(planPath)
	b, _ := os.ReadFile(planPath2)
	if string(a) != string(b) {
		t.Fatal("同一 chart + 同一 values 两次编译应逐字节相同")
	}
}

// findMarker 定位 probe chart 的 marker 文件（<base>/markers/probe/release.json）。
func findMarker(t *testing.T, chartDir string) string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(chartDir), "markers", "*", "release.json"))
	if err != nil || len(matches) == 0 {
		t.Fatalf("marker 未找到: %v", err)
	}
	return matches[0]
}
