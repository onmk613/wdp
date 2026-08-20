package module

import (
	"fmt"
	"os"
	"strings"

	"wdp/internal/i18n"
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
	return i18n.T("deploy systemd unit files and manage service state", "部署 systemd unit 文件并管理服务状态")
}

// Params 参数文档。
func (m *SystemdUnitModule) Params() []ParamDoc {
	return []ParamDoc{
		{Name: "name", Type: "string", Desc: "unit 文件名（basename，如 myapp.service）"},
		{Name: "content", Type: "string", Desc: "unit 文件字面量内容（与 src 二选一，不渲染模板）"},
		{Name: "src", Type: "string", Desc: "本地 unit 模板路径（内容含 {{ 时经渲染引擎渲染）"},
		{Name: "dest_dir", Type: "string", Default: "/etc/systemd/system", Desc: "unit 部署目录"},
		{Name: "state", Type: "string", Desc: "started/stopped/restarted/reloaded（可选）"},
		{Name: "enabled", Type: "bool", Desc: "是否开机自启（可选）"},
		{Name: "daemon_reload", Type: "bool", Default: "true", Desc: "unit 文件变更后执行 systemctl daemon-reload"},
	}
}

// Example 示例任务。
func (m *SystemdUnitModule) Example() string {
	return `# 部署并启动服务（内容变更自动 daemon-reload）
- name: 部署 myapp 服务
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

# 模板渲染部署（src 含 {{ .env }} 等变量时自动渲染）
- name: 部署渲染后的 unit
  become: true
  systemd_unit:
    name: worker.service
    src: templates/worker.service
    dest_dir: /etc/systemd/system
    state: restarted`
}

// Run 执行 unit 部署与服务管理。
func (m *SystemdUnitModule) Run(rc *RunContext, args map[string]any, free string) *Result {
	name, ok := argStr(args, "name")
	if !ok || name == "" {
		return Fail("systemd_unit 需要 name 参数（unit 文件 basename，如 myapp.service）")
	}
	if strings.Contains(name, "/") {
		return Fail("name 必须是 basename（不含 /），目录用 dest_dir 指定")
	}
	content, hasContent := argStr(args, "content")
	src, hasSrc := argStr(args, "src")
	if hasContent == hasSrc {
		return Fail("content 与 src 必须二选一")
	}
	if hasContent && content == "" {
		return Fail("content 不能为空（模板渲染为空的 unit 文件会导致 systemd 不可用）")
	}
	if hasSrc && src == "" {
		return Fail("src 不能为空")
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
			return Fail("不支持的 state %q（可选: started/stopped/restarted/reloaded）", state)
		}
	}
	enabled, hasEnabled := argBool(args, "enabled")
	daemonReload, hasDR := argBool(args, "daemon_reload")
	if !hasDR {
		daemonReload = true
	}

	// 组装 unit 内容：content 字面量；src 本地文件（含 {{ 时渲染）
	var data []byte
	if hasContent {
		data = []byte(content)
	} else {
		raw, err := os.ReadFile(resolveLocal(rc, src))
		if err != nil {
			return Fail("读取 unit 模板失败: %v", err)
		}
		text := string(raw)
		if strings.Contains(text, "{{") {
			eng := rc.Engine
			if eng == nil {
				eng = render.DefaultEngine()
			}
			rendered, err := eng.Render(text, rc.Vars)
			if err != nil {
				return Fail("unit 模板渲染失败: %v", err)
			}
			text = rendered
		}
		data = []byte(text)
	}

	if out, bad := rc.exec("command -v systemctl >/dev/null 2>&1"); bad != nil {
		return bad
	} else if out.Code != 0 {
		return Fail("目标机未安装 systemctl（V1 仅支持 systemd）")
	}

	dest := strings.TrimSuffix(destDir, "/") + "/" + name

	// check 模式：文件部分由 putFile 预估，服务部分只探测返回预估
	if rc.CheckMode {
		fileChanged, fileRes := putFile(rc, data, dest, 0o644, false, true, "", "")
		if fileRes != nil && fileRes.Failed {
			return fileRes
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
			actions = append(actions, "unit 文件将写入"+boolTo(daemonReload && fileChanged, "并 daemon-reload", ""))
		}
		actions = append(actions, svcActions...)
		msg := fmt.Sprintf("[check] %s 无变化", name)
		if would {
			msg = fmt.Sprintf("[check] %s: %s", name, joinCN(actions))
		}
		var diffLines []string
		if fileRes.Diff != "" {
			diffLines = append(diffLines, fileRes.Diff)
		}
		diffLines = append(diffLines, svcDiff...)
		return &Result{Changed: would, Msg: msg, Diff: joinLines(diffLines)}
	}

	// 文件落盘：幂等 + 备份 + 回滚登记全由 putFile 承担
	fileChanged, res := putFile(rc, data, dest, 0o644, false, true, "", "")
	if res != nil {
		return res
	}
	changed := false
	var actions []string
	if fileChanged {
		changed = true
		actions = append(actions, "已写入 unit 文件")
		if daemonReload {
			if out, bad := rc.exec("systemctl daemon-reload"); bad != nil {
				return bad
			} else if out.Code != 0 {
				return Fail("systemctl daemon-reload 失败: %s", firstLine(out.Stderr))
			}
			actions = append(actions, "已 daemon-reload")
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
	msg := fmt.Sprintf("%s 无变化", name)
	if changed {
		msg = fmt.Sprintf("%s: %s", name, joinCN(actions))
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
			actions = append(actions, "将 "+verbFor(state))
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
			actions = append(actions, "将调整自启")
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
				return false, nil, Fail("systemctl %s %s 失败: %s", verb, name, firstLine(out.Stderr))
			}
			changed = true
			actions = append(actions, "已"+verb)
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
				return false, nil, Fail("systemctl %s %s 失败: %s", verb, name, firstLine(out.Stderr))
			}
			changed = true
			actions = append(actions, "已"+verb)
		}
	}
	return changed, actions, nil
}
