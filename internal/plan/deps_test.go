package plan_test

// plan 契约包依赖白名单对账（分层方案 P3）：plan.json 是控制端与 agent
// 之间的冻结契约，本包的依赖面就是契约的实现面——必须收敛到 model
// 一个内部包。chart/playbook（编译侧，internal/planbuild）、executor、
// render 等一旦被引入，契约面就随实现演进而漂移，agent 侧"解析计划只需
// 本包"的保证随之失效。同源思路：cmd/wdp-agent/deps_test.go 的档位边界。

import (
	"os/exec"
	"strings"
	"testing"
)

// TestPlanContractDependencies plan 包的 import 闭包只允许 model。
func TestPlanContractDependencies(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go 工具链不可用，跳过依赖白名单对账")
	}
	out, err := exec.Command("go", "list", "-deps", ".").Output()
	if err != nil {
		t.Fatalf("go list -deps: %v", err)
	}
	allowed := map[string]bool{
		"wdp/internal/model": true,
		"wdp/internal/plan":  true, // 自身
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if !strings.HasPrefix(line, "wdp/internal/") {
			continue
		}
		if !allowed[line] {
			t.Errorf("plan 契约包引入了 %s——契约面被实现依赖污染（编译逻辑属 internal/planbuild；新增模型字段请评估是否需要 SchemaVer 演进）", line)
		}
	}
}
