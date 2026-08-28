package module

import (
	"fmt"
	"strings"

	"wdp/internal/shellquote"
)

func init() {
	Register(&ServiceModule{})
}

// ServiceModule systemd 服务状态与自启管理。
type ServiceModule struct{}

// Name 模块名。
func (m *ServiceModule) Name() string { return "service" }

// Desc 模块说明。
func (m *ServiceModule) Desc() string {
	return "manage systemd service state and boot enablement"
}

// Run 管理服务状态与自启（is-active/is-enabled 漂移探测，幂等：
// 仅状态漂移才动作；restarted/reloaded 恒动作；reloaded 对未运行服务等价 start）。
func (m *ServiceModule) Run(rc *RunContext, args map[string]any, _ string) *Result {
	name, ok := argStr(args, "name")
	if !ok || name == "" {
		return Fail("%s", "service requires a name parameter")
	}
	state, hasState := argStr(args, "state")
	switch state {
	case "", "started", "stopped", "restarted", "reloaded":
	default:
		return Fail("unsupported state %q (options: started/stopped/restarted/reloaded)", state)
	}
	enabled, hasEnabled := argBool(args, "enabled")
	if !hasState && !hasEnabled {
		return Fail("%s", "service requires at least one of state or enabled")
	}

	active, bad := isActive(rc, name)
	if bad != nil {
		return bad
	}
	enabledNow, bad := isEnabled(rc, name)
	if bad != nil {
		return bad
	}

	// 动作计划：systemctl 子命令 + 动作描述
	var verbs, logs []string
	wouldActive, wouldEnabled := active, enabledNow
	switch {
	case state == "started" && !active:
		verbs, logs = append(verbs, "start"), append(logs, "start")
		wouldActive = true
	case state == "stopped" && active:
		verbs, logs = append(verbs, "stop"), append(logs, "stop")
		wouldActive = false
	case state == "restarted":
		verbs, logs = append(verbs, "restart"), append(logs, "restart")
		wouldActive = true
	case state == "reloaded":
		if active {
			verbs, logs = append(verbs, "reload"), append(logs, "reload")
		} else {
			// 未运行服务 reload 等价 start（收敛到运行态）
			verbs, logs = append(verbs, "start"), append(logs, "start")
		}
		wouldActive = true
	}
	switch {
	case hasEnabled && enabled && !enabledNow:
		verbs, logs = append(verbs, "enable"), append(logs, "enable autostart")
		wouldEnabled = true
	case hasEnabled && !enabled && enabledNow:
		verbs, logs = append(verbs, "disable"), append(logs, "disable autostart")
		wouldEnabled = false
	}

	if rc.CheckMode {
		res := &Result{Changed: len(verbs) > 0}
		if res.Changed {
			if hasState {
				res.Msg = fmt.Sprintf("[check] would %s %s (%s)", verbFor(state), name, joinWords(logs))
			} else {
				res.Msg = fmt.Sprintf("[check] %s (%s)", name, joinWords(logs))
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
			res.Diff = joinLines(lines)
		}
		return res
	}

	if len(verbs) == 0 {
		return &Result{Msg: fmt.Sprintf("%s is already in the target state", name)}
	}
	for _, v := range verbs {
		out, bad := rc.exec(fmt.Sprintf("systemctl %s %s", v, shellquote.Quote(name)))
		if bad != nil {
			return bad
		}
		if out.Code != 0 {
			return Fail("systemctl %s %s failed: %s", v, name, firstLine(out.Stderr))
		}
	}
	return &Result{Changed: true, Msg: fmt.Sprintf("%s %s", name, joinWords(logs))}
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

func joinLines(items []string) string {
	var s strings.Builder
	for i, it := range items {
		if i > 0 {
			s.WriteString("\n")
		}
		s.WriteString(it)
	}
	return s.String()
}

func joinWords(items []string) string {
	sep := ", "
	var s strings.Builder
	for i, it := range items {
		if i > 0 {
			s.WriteString(sep)
		}
		s.WriteString(it)
	}
	return s.String()
}

// Params 参数文档。
func (m *ServiceModule) Params() []ParamDoc {
	return []ParamDoc{
		{Name: "name", Type: "string", Desc: "systemd unit name (required)"},
		{Name: "state", Type: "string", Desc: "started/stopped/restarted/reloaded"},
		{Name: "enabled", Type: "bool", Desc: "enable on boot"},
	}
}

// Example 示例任务。
func (m *ServiceModule) Example() string {
	return `- name: start and enable on boot
  service:
    name: nginx
    state: started
    enabled: true
`
}
