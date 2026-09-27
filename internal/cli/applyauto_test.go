package cli

// §7.7 端到端：`wdp apply --autonomous` 把 plan 分片提交给目标 agent，
// agent 在本地完成收敛（selfexec 路径）——控制端提交后即可断开。
// 覆盖：agent 通道提交、进度轮询、非 agent 通道显式报错（§7.5）。

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"wdp/internal/agent"
)

// startAgentForTest 起回环 agent（复用 agent 包的测试装配语义）。
func startAgentForTest(t *testing.T) string {
	t.Helper()
	s := agent.New("")
	s.SetRunsDir(filepath.Join(t.TempDir(), "runs"))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = s.Serve(ln) }()
	base := "http://" + ln.Addr().String()
	t.Cleanup(func() {
		resp, err := http.Post(base+"/shutdown", "", nil)
		if err == nil {
			resp.Body.Close()
		}
	})
	return base
}

// TestApplyAutonomousFullLoop agent 通道自治全链路：plan 编译 → 分片提交 →
// agent 本地收敛（真实执行 shell 任务）→ 轮询到终态。
func TestApplyAutonomousFullLoop(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	base := startAgentForTest(t)
	target := filepath.Join(t.TempDir(), "autonomous.flag")

	// chart：shell 任务写 marker 文件
	chartDir := filepath.Join(t.TempDir(), "probe")
	write := func(rel, content string) {
		p := filepath.Join(chartDir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("chart.yaml", "name: probe\nversion: 0.1.0\n")
	write("values.yaml", "target: "+target+"\n")
	write("deploy.yaml", "- hosts: all\n  tasks:\n    - shell: 'echo autonomous > {{ .target }}'\n")

	// inventory：目标主机走 agent 通道（指向测试 agent）
	invPath := filepath.Join(t.TempDir(), "inv.yaml")
	if err := os.WriteFile(invPath, []byte("all:\n  hosts:\n    self: {conn: agent, agent_url: '"+base+"'}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	old, oldExplicit := gInventories, gInventoryExplicit
	gInventories, gInventoryExplicit = []string{invPath}, true
	defer func() { gInventories, gInventoryExplicit = old, oldExplicit }()

	ctx := context.Background()
	planPath := filepath.Join(t.TempDir(), "plan.json")
	if err := runPlanCompile(ctx, chartDir, planPath, planCompileOptions{phase: "deploy"}); err != nil {
		t.Fatalf("plan 编译失败: %v", err)
	}

	// 自治提交 + 轮询（yes 跳过不可逆确认：shell 任务）
	if err := runApply(ctx, planPath, applyOptions{autonomous: true, yes: true}); err != nil {
		t.Fatalf("autonomous apply 失败: %v", err)
	}
	// agent 侧真实执行了任务
	if b, err := os.ReadFile(target); err != nil || string(b) != "autonomous\n" {
		t.Fatalf("任务应经 agent 自治执行: %q %v", string(b), err)
	}
}

// TestApplyAutonomousRequiresAgentChannel §7.5：非 agent 通道显式报错并
// 说明原因，不静默降级。
func TestApplyAutonomousRequiresAgentChannel(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	chartDir, _, restore := planApplyEnv(t)
	defer restore()
	ctx := context.Background()
	planPath := filepath.Join(t.TempDir(), "plan.json")
	if err := runPlanCompile(ctx, chartDir, planPath, planCompileOptions{phase: "deploy"}); err != nil {
		t.Fatalf("plan 编译失败: %v", err)
	}
	err := runApply(ctx, planPath, applyOptions{autonomous: true, yes: true})
	if err == nil {
		t.Fatal("非 agent 通道的自治请求应报错")
	}
	want := "requires the agent channel"
	if !contains(err.Error(), want) {
		t.Fatalf("错误应说明原因（%q）: %v", want, err)
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }

// TestApplyAutonomousRerunFlagRequiresAutonomous --rerun 脱离 --autonomous
// 显式报错（与 --detach/--resume 同口径：不静默丢 flag）。
func TestApplyAutonomousRerunFlagRequiresAutonomous(t *testing.T) {
	err := runApply(context.Background(), "plan.json", applyOptions{rerun: true})
	if err == nil || !contains(err.Error(), "--rerun requires --autonomous") {
		t.Fatalf("应显式报错: %v", err)
	}
}

// TestApplyAutonomousRerunForce --rerun 透传全链路：done 的 run 默认幂等
// 不重跑；--rerun 清 agent 侧幂等键重新收敛（同一 chart 对漂移主机重新收敛）。
func TestApplyAutonomousRerunForce(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	base := startAgentForTest(t)
	target := filepath.Join(t.TempDir(), "drift.flag")

	chartDir := filepath.Join(t.TempDir(), "probe")
	write := func(rel, content string) {
		p := filepath.Join(chartDir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("chart.yaml", "name: probe\nversion: 0.1.0\n")
	write("values.yaml", "target: "+target+"\n")
	write("deploy.yaml", "- hosts: all\n  tasks:\n    - shell: 'echo autonomous > {{ .target }}'\n")

	invPath := filepath.Join(t.TempDir(), "inv.yaml")
	if err := os.WriteFile(invPath, []byte("all:\n  hosts:\n    self: {conn: agent, agent_url: '"+base+"'}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	old, oldExplicit := gInventories, gInventoryExplicit
	gInventories, gInventoryExplicit = []string{invPath}, true
	defer func() { gInventories, gInventoryExplicit = old, oldExplicit }()

	ctx := context.Background()
	planPath := filepath.Join(t.TempDir(), "plan.json")
	if err := runPlanCompile(ctx, chartDir, planPath, planCompileOptions{phase: "deploy"}); err != nil {
		t.Fatalf("plan 编译失败: %v", err)
	}

	opts := applyOptions{autonomous: true, yes: true}
	if err := runApply(ctx, planPath, opts); err != nil {
		t.Fatalf("首次自治提交失败: %v", err)
	}
	// 模拟漂移：删掉收敛产物
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	// 非 --rerun 重提：幂等命中旧 done run，不重新收敛
	if err := runApply(ctx, planPath, opts); err != nil {
		t.Fatalf("幂等重提不应报错: %v", err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatal("非 --rerun 重提应幂等跳过（marker 不应重建）")
	}
	// --rerun 重提：agent 清幂等键重新收敛
	if err := runApply(ctx, planPath, applyOptions{autonomous: true, yes: true, rerun: true}); err != nil {
		t.Fatalf("--rerun 重提失败: %v", err)
	}
	if b, err := os.ReadFile(target); err != nil || string(b) != "autonomous\n" {
		t.Fatalf("--rerun 应重新收敛: %q %v", string(b), err)
	}
}
