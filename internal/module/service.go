package module

import (
	"fmt"

	"wdp/internal/i18n"
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
	return i18n.T("manage systemd service state and boot enablement", "管理 systemd 服务状态与开机自启")
}

// Run 管理服务状态与自启（is-active/is-enabled 漂移探测，幂等：
// 仅状态漂移才动作；restarted/reloaded 恒动作；reloaded 对未运行服务等价 start）。
func (m *ServiceModule) Run(rc *RunContext, args map[string]any, free string) *Result {
	name, ok := argStr(args, "name")
	if !ok || name == "" {
		return Fail("service 需要 name 参数")
	}
	state, hasState := argStr(args, "state")
	switch state {
	case "", "started", "stopped", "restarted", "reloaded":
	default:
		return Fail("不支持的 state %q（可选: started/stopped/restarted/reloaded）", state)
	}
	enabled, hasEnabled := argBool(args, "enabled")
	if !hasState && !hasEnabled {
		return Fail("service 需要 state 与 enabled 至少一项")
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
		verbs, logs = append(verbs, "start"), append(logs, "启动")
		wouldActive = true
	case state == "stopped" && active:
		verbs, logs = append(verbs, "stop"), append(logs, "停止")
		wouldActive = false
	case state == "restarted":
		verbs, logs = append(verbs, "restart"), append(logs, "重启")
		wouldActive = true
	case state == "reloaded":
		if active {
			verbs, logs = append(verbs, "reload"), append(logs, "重载")
		} else {
			// 未运行服务 reload 等价 start（收敛到运行态）
			verbs, logs = append(verbs, "start"), append(logs, "启动")
		}
		wouldActive = true
	}
	switch {
	case hasEnabled && enabled && !enabledNow:
		verbs, logs = append(verbs, "enable"), append(logs, "启用自启")
		wouldEnabled = true
	case hasEnabled && !enabled && enabledNow:
		verbs, logs = append(verbs, "disable"), append(logs, "禁用自启")
		wouldEnabled = false
	}

	if rc.CheckMode {
		res := &Result{Changed: len(verbs) > 0}
		if res.Changed {
			if hasState {
				res.Msg = fmt.Sprintf("[check] 将%s %s（%s）", verbFor(state), name, joinCN(logs))
			} else {
				res.Msg = fmt.Sprintf("[check] %s（%s）", name, joinCN(logs))
			}
		} else {
			res.Msg = fmt.Sprintf("[check] %s 已是目标状态", name)
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
		return &Result{Msg: fmt.Sprintf("%s 已是目标状态", name)}
	}
	for _, v := range verbs {
		out, bad := rc.exec(fmt.Sprintf("systemctl %s %s", v, shellquote.Quote(name)))
		if bad != nil {
			return bad
		}
		if out.Code != 0 {
			return Fail("systemctl %s %s 失败: %s", v, name, firstLine(out.Stderr))
		}
	}
	return &Result{Changed: true, Msg: fmt.Sprintf("%s %s", name, joinCN(logs))}
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
		return "启动"
	case "stopped":
		return "停止"
	case "restarted":
		return "重启"
	case "reloaded":
		return "重载"
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
	s := ""
	for i, it := range items {
		if i > 0 {
			s += "\n"
		}
		s += it
	}
	return s
}

func joinCN(items []string) string {
	s := ""
	for i, it := range items {
		if i > 0 {
			s += "、"
		}
		s += it
	}
	return s
}

// Params 参数文档。
func (m *ServiceModule) Params() []ParamDoc {
	return []ParamDoc{
		{Name: "name", Type: "string", Desc: "systemd 单元名（必需）"},
		{Name: "state", Type: "string", Desc: "started/stopped/restarted/reloaded"},
		{Name: "enabled", Type: "bool", Desc: "开机自启"},
	}
}

// Example 示例任务。
func (m *ServiceModule) Example() string {
	return `- name: 启动并自启
  service:
    name: nginx
    state: started
    enabled: true
`
}
