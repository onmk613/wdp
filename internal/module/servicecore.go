package module

// service 与 systemd_unit 共用的服务状态收敛内核：期望态 → 计划 → 应用。
// 决策（svcStatePlan/svcEnablePlan）与执行（svcExec）收敛于此，check 预估
// 与实跑共用同一实现，防多份拷贝口径漂移；探测时机与结果文案保留在各模块。

import (
	"fmt"

	"wdp/internal/shellquote"
)

// svcStatePlan 由期望 state 与当前 is-active 探测值推导 state 动作：
// started→未运行才 start；stopped→运行中才 stop；restarted→恒 restart；
// reloaded→运行中 reload、未运行 start（reload 语义上等价收敛到运行态）。
// 返回 systemctl 动词（空 = state 已收敛，无动作）与动作后的预期运行态。
func svcStatePlan(state string, active bool) (verb string, wouldActive bool) {
	switch state {
	case "started":
		if !active {
			return "start", true
		}
	case "stopped":
		if active {
			return "stop", false
		}
	case "restarted":
		return "restart", true
	case "reloaded":
		if active {
			return "reload", true
		}
		return "start", true
	}
	return "", active
}

// svcEnablePlan 由期望 enabled 与当前 is-enabled 探测值推导自启动作
// （enable/disable，空 = 已收敛，无动作）。
func svcEnablePlan(enabled, enabledNow bool) string {
	if enabled == enabledNow {
		return ""
	}
	if enabled {
		return "enable"
	}
	return "disable"
}

// svcExec 执行一条 systemctl 变更命令（两模块共用执行与失败报错口径）。
func svcExec(rc *RunContext, name, verb string) *Result {
	out, bad := rc.exec(fmt.Sprintf("systemctl %s %s", verb, shellquote.Quote(name)))
	if bad != nil {
		return bad
	}
	if out.Code != 0 {
		return Fail("systemctl %s %s failed: %s", verb, name, firstLine(out.Stderr))
	}
	return nil
}
