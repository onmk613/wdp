package cli

// run 的零主机口径回归：--limit 命中不到任何 play 的目标主机时必须报错
// 退出，而不是跑完空 RECAP 并退出 0（CI 会把"什么都没做"当成部署成功）。

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRunLimitNonIntersectingFails 合法但不相交的组：play 目标是组 a，
// --limit b 应报错而非静默成功。
func TestRunLimitNonIntersectingFails(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	base := t.TempDir()
	chartDir := filepath.Join(base, "chart")
	if err := os.MkdirAll(chartDir, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(rel, content string) {
		if err := os.WriteFile(filepath.Join(chartDir, rel), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("chart.yaml", "name: two\nversion: 0.1.0\nno_marker: true\n")
	write("deploy.yaml", "- hosts: a\n  tasks:\n    - debug: {msg: hi}\n")

	invPath := filepath.Join(base, "inv.yaml")
	if err := os.WriteFile(invPath, []byte("a:\n  hosts:\n    hosta: {conn: local}\nb:\n  hosts:\n    hostb: {conn: local}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	old, oldExplicit := gInventories, gInventoryExplicit
	gInventories, gInventoryExplicit = []string{invPath}, true
	defer func() { gInventories, gInventoryExplicit = old, oldExplicit }()

	err := runTarget(context.Background(), chartDir, runOptions{phase: "deploy", yes: true, limit: "b"})
	if err == nil || !strings.Contains(err.Error(), "matched no hosts") {
		t.Fatalf("--limit 不相交应报错: %v", err)
	}
	// 对照组：--limit a 命中，正常执行
	if err := runTarget(context.Background(), chartDir, runOptions{phase: "deploy", yes: true, limit: "a"}); err != nil {
		t.Fatalf("--limit 命中不应失败: %v", err)
	}
}
