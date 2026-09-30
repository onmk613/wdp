package main_test

// 依赖边界对账（分层方案的防腐烂闸门）：瘦 agent 入口的 import 闭包必须
// 保持收窄——import 拖拽只会「静默多带」，编译照样通过，因此边界必须在
// 这里以 go list 断言硬失败，不靠记性。
//
//   - cmd/wdp-agent 不得依赖：web/store/console/worker（console 域）、
//     conn/sshc（SSH 通道）、cli（全量组合根）、agentops/agentbin（推装，
//     控制端职责）
//
// 控制端侧不再有 tag 变体（原 wdp_no_console / wdp_no_cli 两档已删除）：全量
// 档是唯一的控制端产物，cmd/wdp 拉进 console 域是预期行为，不再断言。
//
// 同源思路：cli/schema_test.go 的主机键文档对账、build.sh 的脏树指纹。

import (
	"os/exec"
	"strings"
	"testing"
)

// forbiddenAgent 是 agent 档禁止出现的内部包（前缀匹配）。
//
// 有意保留的依赖（不是遗漏）：chart/playbook/render 经 executor 进入
// agent——plan 回放期要物化内嵌 chart 树并展开 chart: 引用（执行语义与
// run 路径单一实现，见 executor/planrun.go），template/script 模块在目标
// 机渲染模板（render/sprig）。plan 契约本身的冻结面由 internal/plan 的
// deps_test.go 单独钉住。
var forbiddenAgent = []string{
	"wdp/internal/web",
	"wdp/internal/store",
	"wdp/internal/console",
	"wdp/internal/worker",
	"wdp/internal/conn/sshc",
	"wdp/internal/cli",
	"wdp/internal/agentops",
	"wdp/internal/agentbin",
}

func goListDeps(t *testing.T, args ...string) []string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go 工具链不可用，跳过依赖边界对账")
	}
	out, err := exec.Command("go", append([]string{"list", "-deps"}, args...)...).Output()
	if err != nil {
		t.Fatalf("go list %v: %v", args, err)
	}
	return strings.Split(strings.TrimSpace(string(out)), "\n")
}

func assertNoDeps(t *testing.T, deps []string, forbidden []string, build string) {
	t.Helper()
	for _, d := range deps {
		for _, f := range forbidden {
			if d == f {
				t.Errorf("%s 依赖了 %s——%s 的功能面被拖回，依赖边界已破（分层方案 P1 锁边）", build, f, build)
			}
		}
	}
}

// TestAgentTierDependencyBoundary 瘦 agent 入口的 import 闭包对账。
func TestAgentTierDependencyBoundary(t *testing.T) {
	assertNoDeps(t, goListDeps(t, "."), forbiddenAgent, "agent 档（cmd/wdp-agent）")
}
