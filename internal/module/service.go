package module

import (
	"fmt"
	"slices"
	"strings"

	"wdp/internal/shellquote"
)

func init() {
	Register(&ServiceModule{})
}

// serviceStates 是 service/systemd_unit 共用的服务 state 值域：解析
// 校验与 Params 文档共用同一变量（文档长在实现旁边）。
var serviceStates = []string{"started", "stopped", "restarted", "reloaded"}

// ServiceModule systemd 服务状态与自启管理。
type ServiceModule struct{}

func (m *ServiceModule) Name() string { return "service" }

func (m *ServiceModule) Desc() string {
	return "manage systemd service state and boot enablement"
}

// Run 管理服务状态与自启（is-active/is-enabled 漂移探测，幂等：
// 仅状态漂移才动作；restarted/reloaded 恒动作；reloaded 对未运行服务等价 start）。
func (m *ServiceModule) Run(rc *RunContext, args map[string]any, _ string) *Result {
	name, ok := argStr(args, "name")
	if !ok || name == "" {
		return Fail("service requires a name parameter")
	}
	state, hasState := argStr(args, "state")
	if state != "" && !slices.Contains(serviceStates, state) {
		return Fail("unsupported state %q (options: %s)", state, strings.Join(serviceStates, "/"))
	}
	enabled, hasEnabled := argBool(args, "enabled")
	if !hasState && !hasEnabled {
		return Fail("service requires at least one of state or enabled")
	}

	active, bad := isActive(rc, name)
	if bad != nil {
		return bad
	}
	enabledNow, bad := isEnabled(rc, name)
	if bad != nil {
		return bad
	}

	// 动作计划：期望态 → systemctl 子命令序列（service/systemd_unit 共用
	// 内核 svcStatePlan/svcEnablePlan，见 servicecore.go）。state 动作在前、
	// 自启动作在后；logs 是面向用户的动作描述（自启带 autostart 后缀）。
	stateVerb, wouldActive := svcStatePlan(state, active)
	enableVerb := ""
	wouldEnabled := enabledNow
	if hasEnabled {
		if v := svcEnablePlan(enabled, enabledNow); v != "" {
			enableVerb = v
			wouldEnabled = enabled
		}
	}
	var verbs, logs []string
	if stateVerb != "" {
		verbs, logs = append(verbs, stateVerb), append(logs, stateVerb)
	}
	if enableVerb != "" {
		verbs, logs = append(verbs, enableVerb), append(logs, enableVerb+" autostart")
	}

	if rc.CheckMode {
		res := &Result{Changed: len(verbs) > 0}
		if res.Changed {
			if hasState {
				res.Msg = fmt.Sprintf("[check] would %s %s (%s)", verbFor(state), name, strings.Join(logs, ", "))
			} else {
				res.Msg = fmt.Sprintf("[check] %s (%s)", name, strings.Join(logs, ", "))
			}
		} else {
			res.Msg = fmt.Sprintf("[check] %s is already in the target state", name)
		}
		if rc.DiffMode && len(verbs) > 0 {
			var lines []string
			if hasState {
				lines = append(lines,
					fmt.Sprintf("- state: %s", boolTo(active, "active", "inactive")),
					fmt.Sprintf("+ state: %s", boolTo(wouldActive, "active", "inactive")))
			}
			if hasEnabled {
				lines = append(lines,
					fmt.Sprintf("- enabled: %s", boolTo(enabledNow, "enabled", "disabled")),
					fmt.Sprintf("+ enabled: %s", boolTo(wouldEnabled, "enabled", "disabled")))
			}
			res.Diff = strings.Join(lines, "\n")
		}
		return res
	}

	if len(verbs) == 0 {
		return &Result{Msg: fmt.Sprintf("%s is already in the target state", name)}
	}
	for _, v := range verbs {
		if bad := svcExec(rc, name, v); bad != nil {
			return bad
		}
	}
	return &Result{Changed: true, Msg: fmt.Sprintf("%s %s", name, strings.Join(logs, ", "))}
}

func isActive(rc *RunContext, name string) (bool, *Result) {
	out, bad := rc.exec(fmt.Sprintf("systemctl is-active %s >/dev/null 2>&1", shellquote.Quote(name)))
	if bad != nil {
		return false, bad
	}
	return out.Code == 0, nil
}

func isEnabled(rc *RunContext, name string) (bool, *Result) {
	out, bad := rc.exec(fmt.Sprintf("systemctl is-enabled %s 2>/dev/null", shellquote.Quote(name)))
	if bad != nil {
		return false, bad
	}
	return out.Code == 0, nil
}

// verbFor 将 state 映射为动作描述。
func verbFor(state string) string {
	switch state {
	case "started":
		return "start"
	case "stopped":
		return "stop"
	case "restarted":
		return "restart"
	case "reloaded":
		return "reload"
	}
	return state
}

func boolTo(b bool, yes, no string) string {
	if b {
		return yes
	}
	return no
}

// wantState 将服务 state 映射为 systemctl 目标态（systemd_unit 复用）。
func wantState(state string) string {
	switch state {
	case "started":
		return "active"
	case "stopped":
		return "inactive"
	case "restarted", "reloaded":
		return state
	}
	return state
}

func (m *ServiceModule) Params() []ParamDoc {
	return []ParamDoc{
		{Name: "name", Type: "string", Desc: "systemd unit name (required)"},
		{Name: "state", Type: "string", Enum: serviceStates, Desc: "target service state"},
		{Name: "enabled", Type: "bool", Desc: "enable on boot"},
	}
}

func (m *ServiceModule) Example() string {
	return `- name: start and enable on boot
  service:
    name: nginx
    state: started
    enabled: true
`
}
