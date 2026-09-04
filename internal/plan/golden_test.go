package plan

// §6.4 golden 快照：examples/docker 与 examples/node-exporter 的 plan 编译
// 结果固化为 golden 文件，防后续改动无意提交语义漂移。有意变更时：
//
//	UPDATE_GOLDEN=1 go test ./internal/plan/ -run TestGoldenExamples

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"wdp/internal/inventory"
)

const goldenInv = `
docker:
  hosts:
    d1: {ansible_host: 10.20.0.11}
    d2: {ansible_host: 10.20.0.12}
`

// repoRoot 定位仓库根（测试工作目录是 internal/plan）。
func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("repo root not found: %v", err)
	}
	return root
}

func TestGoldenExamples(t *testing.T) {
	root := repoRoot(t)
	inv, err := inventory.Parse([]byte(goldenInv))
	if err != nil {
		t.Fatal(err)
	}
	for _, example := range []string{"docker", "node-exporter"} {
		t.Run(example, func(t *testing.T) {
			target := filepath.Join(root, "examples", example)
			p, err := Compile(target, inv, nil, nil, CompileOptions{WdpVersion: "golden"})
			if err != nil {
				t.Fatalf("compile %s: %v", example, err)
			}
			got, err := json.MarshalIndent(p, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			golden := filepath.Join("testdata", "golden-"+example+".json")
			if os.Getenv("UPDATE_GOLDEN") == "1" {
				if err := os.MkdirAll("testdata", 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(golden, append(got, '\n'), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("golden missing (run UPDATE_GOLDEN=1 go test ./internal/plan/ -run TestGoldenExamples): %v", err)
			}
			if string(want) != string(got)+"\n" {
				t.Fatalf("plan for examples/%s drifted from golden (intentional? regenerate with UPDATE_GOLDEN=1)", example)
			}
		})
	}
}
