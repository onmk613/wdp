package cli

// adhoc 的零主机口径回归：与 run 同源（runlimit_test.go）——模式合法但
// 命中空集时必须报错退出，而不是跑完空 RECAP 并退出 0。

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"wdp/internal/conn/agentc"
	"wdp/internal/model"
)

// TestAdhocZeroHostsFails 模式（未知组）解析失败被 SelectPlays 跳过 →
// 零主机：应报 no hosts selected 而非静默成功。
func TestAdhocZeroHostsFails(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	base := t.TempDir()
	invPath := filepath.Join(base, "inv.yaml")
	if err := os.WriteFile(invPath, []byte("a:\n  hosts:\n    hosta: {conn: local}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	old, oldExplicit := gInventories, gInventoryExplicit
	gInventories, gInventoryExplicit = []string{invPath}, true
	defer func() { gInventories, gInventoryExplicit = old, oldExplicit }()

	err := runAdhoc(context.Background(), "nosuchgroup", adhocOptions{mod: "shell", argStr: "true"})
	if err == nil || !strings.Contains(err.Error(), "no hosts selected") {
		t.Fatalf("零主机应报错（与 run 同口径）: %v", err)
	}
	// 对照组：命中的模式正常执行
	if err := runAdhoc(context.Background(), "a", adhocOptions{mod: "shell", argStr: "true"}); err != nil {
		t.Fatalf("命中主机不应失败: %v", err)
	}
}

// TestPollSubmissionsGivesUpAfterConsecutiveFailures 轮询放弃阈值：agent
// 永久失联时连续失败达 maxPollFails 应计失败返回，而不是无限重试。
func TestPollSubmissionsGivesUpAfterConsecutiveFailures(t *testing.T) {
	old := maxPollFails
	maxPollFails = 2
	defer func() { maxPollFails = old }()

	// 端口 1 上无监听：连接立即被拒（快速失败路径）
	h := &model.Host{Name: "dead", Address: "127.0.0.1", Conn: "agent", AgentURL: "http://127.0.0.1:1"}
	subs := []autonomousSubmission{{
		group:  relayGroup{root: "dead"},
		runID:  "deadrun",
		client: agentc.New(h, connDefaults()),
	}}
	if !pollSubmissions(context.Background(), subs) {
		t.Fatal("连续失败达阈值应计失败返回")
	}
}
