package planbuild

// §6.4 golden 快照：docker 与 node-exporter 两个示例 chart 的 plan 编译
// 结果固化为 golden 文件，防后续改动无意提交语义漂移。有意变更时：
//
//	UPDATE_GOLDEN=1 go test ./internal/plan/ -run TestGoldenExamples
//
// 示例 tgz 是测试夹具，存放于 testdata/（examples/ 目录已不纳入版本
// 控制，fresh checkout 不存在——夹具必须随仓库走，golden 门禁才成立）。

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

func TestGoldenExamples(t *testing.T) {
	inv, err := inventory.Parse([]byte(goldenInv))
	if err != nil {
		t.Fatal(err)
	}
	for _, example := range []string{"docker", "node-exporter"} {
		t.Run(example, func(t *testing.T) {
			// 示例以打包产物形态存放（testdata/<name>-<version>.tgz）：
			// 与「下载制品可直接 wdp apply/再导入」的闭环一致，golden 走
			// 同一条加载路径
			matches, err := filepath.Glob(filepath.Join("testdata", example+"-*.tgz"))
			if err != nil || len(matches) == 0 {
				t.Fatalf("no packaged example for %s: %v", example, err)
			}
			target := matches[0]
			p, err := Compile(target, inv, nil, nil, CompileOptions{WdpVersion: "golden"})
			if err != nil {
				t.Fatalf("compile %s: %v", example, err)
			}
			// 环境无关归一化：packages/ 制品缓存是否存在于本地不影响 golden
			// 语义（examples/*/packages 是 .gitignore 的下载产物，全新 clone
			// 必然不存在）。payload 采集机制由 TestCompilePayloads 单独覆盖。
			p.Payloads = nil
			p.FillID()
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
