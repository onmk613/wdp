package module

// servicecore 内核（svcStatePlan/svcEnablePlan/svcExec）的表驱动测试：
// is-active/is-enabled 全组合 → 期望动词；并用 TestServiceCoreModuleParity
// 端到端验证 service 与 systemd_unit 两个模块在同一组合下产生完全相同
// 的 systemctl 变更序列（共用内核，任何一侧分叉在此暴露）。

import (
	"fmt"
	"strings"
	"testing"
)

func TestSvcStatePlan(t *testing.T) {
	cases := []struct {
		state     string
		active    bool
		wantVerb  string
		wantAfter bool
	}{
		{"started", false, "start", true},
		{"started", true, "", true}, // 已运行：收敛，无动作
		{"stopped", true, "stop", false},
		{"stopped", false, "", false}, // 已停止：收敛，无动作
		{"restarted", true, "restart", true},
		{"restarted", false, "restart", true}, // restart 恒动作
		{"reloaded", true, "reload", true},
		{"reloaded", false, "start", true}, // 未运行 reload 等价 start（收敛到运行态）
		{"", true, "", true},               // 未给 state（纯自启管理）不动服务
		{"", false, "", false},
	}
	for _, c := range cases {
		verb, after := svcStatePlan(c.state, c.active)
		if verb != c.wantVerb || after != c.wantAfter {
			t.Errorf("svcStatePlan(%q, active=%v) = (%q, %v), want (%q, %v)",
				c.state, c.active, verb, after, c.wantVerb, c.wantAfter)
		}
	}
}

func TestSvcEnablePlan(t *testing.T) {
	cases := []struct {
		enabled, enabledNow bool
		want                string
	}{
		{true, false, "enable"},
		{false, true, "disable"},
		{true, true, ""},   // 已收敛
		{false, false, ""}, // 已收敛
	}
	for _, c := range cases {
		if got := svcEnablePlan(c.enabled, c.enabledNow); got != c.want {
			t.Errorf("svcEnablePlan(%v, %v) = %q, want %q", c.enabled, c.enabledNow, got, c.want)
		}
	}
}

// runServicePair 在同一初始状态下分别跑 service 模块与 systemd_unit
// （纯状态管理形态），返回两者的结果与各自执行的 systemctl 变更序列。
func runServicePair(t *testing.T, check bool, args map[string]any, active, enabledNow bool) (*Result, []string, *Result, []string) {
	t.Helper()

	rcS, _, shS := newUnitRC(t)
	rcS.CheckMode = check
	shS.active["app.service"] = active
	shS.enabled["app.service"] = enabledNow
	rs := (&ServiceModule{}).Run(rcS, args, "")

	rcU, _, shU := newUnitRC(t)
	rcU.CheckMode = check
	shU.active["app.service"] = active
	shU.enabled["app.service"] = enabledNow
	ru := (&SystemdUnitModule{}).Run(rcU, args, "")

	return rs, shS.runs, ru, shU.runs
}

// TestServiceCoreModuleParity 端到端矩阵：同一 (state, active, enabled,
// enabledNow) 组合下，service 模块与 systemd_unit（纯状态管理形态）必须
// 产生完全相同的 systemctl 变更命令序列与 changed 判定；check 模式下
// 两者的预估 changed 也必须一致（均不含文件部署，systemd_unit 的
// daemon-reload 只在文件变更时出现，此处不涉及）。
func TestServiceCoreModuleParity(t *testing.T) {
	states := []string{"started", "stopped", "restarted", "reloaded", ""}
	for _, state := range states {
		for _, active := range []bool{false, true} {
			for _, enabled := range []bool{false, true} {
				for _, enabledNow := range []bool{false, true} {
					label := fmt.Sprintf("state=%q active=%v enabled=%v enabledNow=%v", state, active, enabled, enabledNow)
					args := map[string]any{"name": "app.service", "enabled": enabled}
					if state != "" {
						args["state"] = state
					}
					for _, check := range []bool{false, true} {
						rs, runsS, ru, runsU := runServicePair(t, check, args, active, enabledNow)
						if rs.Failed {
							t.Fatalf("%s check=%v: service 失败: %s", label, check, rs.Msg)
						}
						if ru.Failed {
							t.Fatalf("%s check=%v: systemd_unit 失败: %s", label, check, ru.Msg)
						}
						if rs.Changed != ru.Changed {
							t.Errorf("%s check=%v: changed 不一致 service=%v unit=%v", label, check, rs.Changed, ru.Changed)
						}
						if got, want := strings.Join(runsS, "|"), strings.Join(runsU, "|"); got != want {
							t.Errorf("%s check=%v: systemctl 序列不一致\n service: %s\n   unit: %s", label, check, got, want)
						}
					}
				}
			}
		}
	}
}
