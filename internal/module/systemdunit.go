package module

import (
	"fmt"
	"os"
	"strings"

	"wdp/internal/render"
	"wdp/internal/shellquote"
)

func init() {
	Register(&SystemdUnitModule{})
}

// SystemdUnitModule 部署 systemd unit 文件并按需 daemon-reload、启停与自启管理。
// 文件部分复用 putFile（校验和幂等 + 备份 + 回滚登记）；
// 服务部分镜像 service 模块逻辑，复用 isActive/isEnabled 探测。
// daemon-reload 与启停属系统状态变更，不登记回滚（unit 文件本身可回滚）。
type SystemdUnitModule struct{}

// Name 模块名。
func (m *SystemdUnitModule) Name() string { return "systemd_unit" }

// Desc 模块说明。
func (m *SystemdUnitModule) Desc() string {
	return "deploy systemd unit files and manage service state"
}

// Params 参数文档。
func (m *SystemdUnitModule) Params() []ParamDoc {
	return []ParamDoc{
		{Name: "name", Type: "string", Desc: "unit file name (basename, e.g. myapp.service)"},
		{Name: "content", Type: "string", Desc: "unit file literal content (mutually exclusive with src, not templated; omit both for state-only management)"},
		{Name: "src", Type: "string", Desc: "local unit template path (rendered by the engine when the content contains {{)"},
		{Name: "dest_dir", Type: "string", Default: "/etc/systemd/system", Desc: "unit deploy directory"},
		{Name: "state", Type: "string", Desc: "started/stopped/restarted/reloaded (optional)"},
		{Name: "enabled", Type: "bool", Desc: "enable on boot (optional)"},
		{Name: "daemon_reload", Type: "bool", Default: "true", Desc: "run systemctl daemon-reload after unit file changes"},
	}
}

// Example 示例任务。
func (m *SystemdUnitModule) Example() string {
	return `# deploy and start the service (auto daemon-reload on content change)
- name: deploy the myapp service
  become: true
  systemd_unit:
    name: myapp.service
    content: |
      [Unit]
      Description=MyApp

      [Service]
      ExecStart=/opt/myapp/bin/myapp

      [Install]
      WantedBy=multi-user.target
    state: started
    enabled: true

# template-rendered deploy (src containing {{ .env }} etc. is rendered automatically)
- name: deploy the rendered unit
  become: true
  systemd_unit:
    name: worker.service
    src: templates/worker.service
    dest_dir: /etc/systemd/system
    state: restarted`
}

// Run 执行 unit 部署与服务管理。
func (m *SystemdUnitModule) Run(rc *RunContext, args map[string]any, _ string) *Result {
	name, ok := argStr(args, "name")
	if !ok || name == "" {
		return Fail("%s", "systemd_unit requires a name parameter (unit file basename, e.g. myapp.service)")
	}
	if strings.Contains(name, "/") {
		return Fail("%s", "name must be a basename (no /); use dest_dir for the directory")
	}
	content, hasContent := argStr(args, "content")
	src, hasSrc := argStr(args, "src")
	if hasContent && hasSrc {
		return Fail("%s", "content and src are mutually exclusive")
	}
	if hasContent && content == "" {
		return Fail("%s", "content must not be empty (an empty rendered unit file would break systemd)")
	}
	if hasSrc && src == "" {
		return Fail("%s", "src must not be empty")
	}
	destDir, _ := argStr(args, "dest_dir")
	if destDir == "" {
		destDir = "/etc/systemd/system"
	}
	state, hasState := argStr(args, "state")
	if hasState {
		switch state {
		case "started", "stopped", "restarted", "reloaded":
		default:
			return Fail("unsupported state %q (options: started/stopped/restarted/reloaded)", state)
		}
	}
	enabled, hasEnabled := argBool(args, "enabled")
	// 纯状态管理（无 content/src）必须至少给出 state 或 enabled，
	// 否则任务无事可做（历史上这里误判成 content/src 互斥，杀伤 handler 场景）
	if !hasContent && !hasSrc && !hasState && !hasEnabled {
		return Fail("%s", "nothing to manage: provide content/src to deploy a unit, or state/enabled to manage service state")
	}
	daemonReload, hasDR := argBool(args, "daemon_reload")
	if !hasDR {
		daemonReload = true
	}

	// 组装 unit 内容：content 字面量；src 本地文件（含 {{ 时渲染）；
	// 两者都缺省 = 纯状态管理（不部署文件，handler/卸载场景），data 为 nil
	var data []byte
	if hasContent {
		data = []byte(content)
	} else if hasSrc {
		raw, err := os.ReadFile(resolveLocal(rc, src))
		if err != nil {
			return Fail("failed to read unit template: %v", err)
		}
		text := string(raw)
		if strings.Contains(text, "{{") {
			eng := rc.Engine
			if eng == nil {
				eng = render.DefaultEngine()
			}
			rendered, err := eng.Render(text, rc.Vars)
			if err != nil {
				return Fail("unit template rendering failed: %v", err)
			}
			text = rendered
		}
		data = []byte(text)
	}
	deployFile := data != nil

	if out, bad := rc.exec("command -v systemctl >/dev/null 2>&1"); bad != nil {
		return bad
	} else if out.Code != 0 {
		return Fail("%s", "target machine does not have systemctl installed (V1 only supports systemd)")
	}

	dest := strings.TrimSuffix(destDir, "/") + "/" + name

	// check 模式：文件部分由 putFile 预估，服务部分只探测返回预估
	if rc.CheckMode {
		var fileChanged bool
		var fileRes *Result
		if deployFile {
			fileChanged, fileRes = putFile(rc, data, dest, 0o644, false, true, "", "")
			if fileRes != nil && fileRes.Failed {
				return fileRes
			}
		}
		if fileRes == nil {
			fileRes = &Result{}
		}
		svcWould, svcActions, svcDiff, bad := estimateUnitState(rc, name, state, hasState, enabled, hasEnabled)
		if bad != nil {
			return bad
		}
		would := fileChanged || svcWould
		var actions []string
		if fileChanged {
			actions = append(actions, "unit file would be written"+boolTo(daemonReload && fileChanged, " and daemon-reload", ""))
		}
		actions = append(actions, svcActions...)
		msg := fmt.Sprintf("[check] %s no change", name)
		if would {
			msg = fmt.Sprintf("[check] %s: %s", name, joinWords(actions))
		}
		var diffLines []string
		if fileRes.Diff != "" {
			diffLines = append(diffLines, fileRes.Diff)
		}
		diffLines = append(diffLines, svcDiff...)
		return &Result{Changed: would, Msg: msg, Diff: joinLines(diffLines)}
	}

	// 文件落盘：幂等 + 备份 + 回滚登记全由 putFile 承担（纯状态管理跳过）
	var fileChanged bool
	var res *Result
	if deployFile {
		fileChanged, res = putFile(rc, data, dest, 0o644, false, true, "", "")
		if res != nil {
			return res
		}
	}
	changed := false
	var actions []string
	if fileChanged {
		changed = true
		actions = append(actions, "wrote unit file")
		if daemonReload {
			if out, bad := rc.exec("systemctl daemon-reload"); bad != nil {
				return bad
			} else if out.Code != 0 {
				return Fail("systemctl daemon-reload failed: %s", firstLine(out.Stderr))
			}
			actions = append(actions, "daemon-reload done")
		}
	}

	// 服务状态与自启（镜像 service 模块）
	svcChanged, svcActions, bad := applyUnitState(rc, name, state, hasState, enabled, hasEnabled)
	if bad != nil {
		return bad
	}
	if svcChanged {
		changed = true
		actions = append(actions, svcActions...)
	}
	msg := fmt.Sprintf("%s no change", name)
	if changed {
		msg = fmt.Sprintf("%s: %s", name, joinWords(actions))
	}
	return &Result{Changed: changed, Msg: msg}
}

// estimateUnitState check 模式预估 state/enabled 变化，
// 返回（是否变更、动作描述、diff 行、失败）。
func estimateUnitState(rc *RunContext, name string, state string, hasState bool, enabled, hasEnabled bool) (bool, []string, []string, *Result) {
	would := false
	var actions, diff []string
	if hasState {
		active, bad := isActive(rc, name)
		if bad != nil {
			return false, nil, nil, bad
		}
		if (state == "started" && !active) || (state == "stopped" && active) ||
			state == "restarted" || state == "reloaded" {
			would = true
			actions = append(actions, "would "+verbFor(state))
			diff = append(diff,
				fmt.Sprintf("- %s: %s", name, boolTo(active, "active", "inactive")),
				fmt.Sprintf("+ %s: %s", name, wantState(state)))
		}
	}
	if hasEnabled {
		enabledNow, bad := isEnabled(rc, name)
		if bad != nil {
			return false, nil, nil, bad
		}
		if enabledNow != enabled {
			would = true
			actions = append(actions, "would adjust autostart")
			diff = append(diff,
				fmt.Sprintf("- %s: %s", name, boolTo(enabledNow, "enabled", "disabled")),
				fmt.Sprintf("+ %s: %s", name, boolTo(enabled, "enabled", "disabled")))
		}
	}
	return would, actions, diff, nil
}

// applyUnitState 实际执行 state/enabled 变更（镜像 service 模块逻辑），
// 返回（是否变更、动作描述、失败）。
func applyUnitState(rc *RunContext, name string, state string, hasState bool, enabled, hasEnabled bool) (bool, []string, *Result) {
	changed := false
	var actions []string
	if hasState {
		active, bad := isActive(rc, name)
		if bad != nil {
			return false, nil, bad
		}
		verb := ""
		switch state {
		case "started":
			if !active {
				verb = "start"
			}
		case "stopped":
			if active {
				verb = "stop"
			}
		case "restarted":
			verb = "restart"
		case "reloaded":
			verb = "reload"
			if !active {
				verb = "start" // 未运行时 reload 等价启动
			}
		}
		if verb != "" {
			if out, bad := rc.exec(fmt.Sprintf("systemctl %s %s", verb, shellquote.Quote(name))); bad != nil {
				return false, nil, bad
			} else if out.Code != 0 {
				return false, nil, Fail("systemctl %s %s failed: %s", verb, name, firstLine(out.Stderr))
			}
			changed = true
			actions = append(actions, verb)
		}
	}
	if hasEnabled {
		enabledNow, bad := isEnabled(rc, name)
		if bad != nil {
			return false, nil, bad
		}
		if enabledNow != enabled {
			verb := "disable"
			if enabled {
				verb = "enable"
			}
			if out, bad := rc.exec(fmt.Sprintf("systemctl %s %s", verb, shellquote.Quote(name))); bad != nil {
				return false, nil, bad
			} else if out.Code != 0 {
				return false, nil, Fail("systemctl %s %s failed: %s", verb, name, firstLine(out.Stderr))
			}
			changed = true
			actions = append(actions, verb)
		}
	}
	return changed, actions, nil
}
