package module

import (
	"fmt"
	"slices"
	"strings"
)

func init() {
	Register(&SystemdUnitModule{})
}

// SystemdUnitModule 部署 systemd unit 文件并按需 daemon-reload、启停与自启管理。
// 文件部分复用 putFile（校验和幂等 + 备份 + 回滚登记）；
// 服务部分走 service 模块共用的收敛内核（servicecore.go：期望态 → 计划
// → 应用），探测 isActive/isEnabled 同口径。
// daemon-reload 与启停属系统状态变更，不登记回滚（unit 文件本身可回滚）。
type SystemdUnitModule struct{}

func (m *SystemdUnitModule) Name() string { return "systemd_unit" }

func (m *SystemdUnitModule) Desc() string {
	return "deploy systemd unit files and manage service state"
}

func (m *SystemdUnitModule) Params() []ParamDoc {
	return []ParamDoc{
		{Name: "name", Type: "string", Desc: "unit file name (basename, e.g. myapp.service)"},
		{Name: "content", Type: "string", Desc: "unit file literal content (mutually exclusive with src, not templated; omit both for state-only management)"},
		{Name: "src", Type: "string", Desc: "local unit template path (rendered by the engine when the content contains {{)"},
		{Name: "dest_dir", Type: "string", Default: "/etc/systemd/system", Desc: "unit deploy directory"},
		{Name: "state", Type: "string", Enum: serviceStates, Desc: "service state to enforce (optional)"},
		{Name: "enabled", Type: "bool", Desc: "enable on boot (optional)"},
		{Name: "daemon_reload", Type: "bool", Default: "true", Desc: "run systemctl daemon-reload after unit file changes"},
	}
}

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

// unitReq 是 systemd_unit 解析后的参数。
type unitReq struct {
	name         string
	content      string
	src          string
	hasContent   bool
	hasSrc       bool
	destDir      string
	state        string
	hasState     bool
	enabled      bool
	hasEnabled   bool
	daemonReload bool
}

// parseUnitArgs 解析并校验 systemd_unit 参数（name 必填且为 basename、
// content/src 互斥与空值拒绝、state 合法性、纯状态管理至少给出 state 或
// enabled、daemon_reload 缺省 true）。
func parseUnitArgs(rc *RunContext, args map[string]any) (*unitReq, *Result) {
	name, ok := argStr(args, "name")
	if !ok || name == "" {
		return nil, Fail("systemd_unit requires a name parameter (unit file basename, e.g. myapp.service)")
	}
	if strings.Contains(name, "/") {
		return nil, Fail("name must be a basename (no /); use dest_dir for the directory")
	}
	u := &unitReq{name: name}
	u.content, u.hasContent = argStr(args, "content")
	u.src, u.hasSrc = argStr(args, "src")
	if u.hasContent && u.hasSrc {
		return nil, Fail("content and src are mutually exclusive")
	}
	if u.hasContent && u.content == "" {
		return nil, Fail("content must not be empty (an empty rendered unit file would break systemd)")
	}
	if u.hasSrc && u.src == "" {
		return nil, Fail("src must not be empty")
	}
	u.destDir, _ = argStr(args, "dest_dir")
	if u.destDir == "" {
		u.destDir = "/etc/systemd/system"
	}
	state, hasState := argStr(args, "state")
	if hasState && !slices.Contains(serviceStates, state) {
		return nil, Fail("unsupported state %q (options: %s)", state, strings.Join(serviceStates, "/"))
	}
	u.state, u.hasState = state, hasState
	u.enabled, u.hasEnabled = argBool(args, "enabled")
	// 纯状态管理（无 content/src）必须至少给出 state 或 enabled，
	// 否则任务无事可做（历史上这里误判成 content/src 互斥，杀伤 handler 场景）
	if !u.hasContent && !u.hasSrc && !u.hasState && !u.hasEnabled {
		return nil, Fail("nothing to manage: provide content/src to deploy a unit, or state/enabled to manage service state")
	}
	var hasDR bool
	u.daemonReload, hasDR = argBool(args, "daemon_reload")
	if !hasDR {
		u.daemonReload = true
	}
	return u, nil
}

// unitData 组装 unit 内容：content 字面量；src 本地文件（含 {{ 时渲染）；
// 两者都缺省 = 纯状态管理（不部署文件，handler/卸载场景），返回 nil。
func unitData(rc *RunContext, u *unitReq) ([]byte, *Result) {
	if u.hasContent {
		return []byte(u.content), nil
	}
	if !u.hasSrc {
		return nil, nil
	}
	local, lerr := resolveLocal(rc, u.src)
	if lerr != nil {
		return nil, Fail("%v", lerr)
	}
	raw, err := readLocalSrc(rc, local)
	if err != nil {
		return nil, Fail("failed to read unit template: %v", err)
	}
	text := string(raw)
	if strings.Contains(text, "{{") {
		rendered, err := rc.engine().Render(text, rc.Vars)
		if err != nil {
			return nil, Fail("unit template rendering failed: %v", err)
		}
		text = rendered
	}
	return []byte(text), nil
}

// Run 执行 unit 部署与服务管理：解析 → 组装内容/探测 systemctl →
// check 预估或实跑（骨架与 user 模块一致）。
func (m *SystemdUnitModule) Run(rc *RunContext, args map[string]any, _ string) *Result {
	u, bad := parseUnitArgs(rc, args)
	if bad != nil {
		return bad
	}
	data, bad := unitData(rc, u)
	if bad != nil {
		return bad
	}
	deployFile := data != nil

	if out, bad := rc.exec("command -v systemctl >/dev/null 2>&1"); bad != nil {
		return bad
	} else if out.Code != 0 {
		return Fail("target machine does not have systemctl installed (V1 only supports systemd)")
	}

	dest := strings.TrimSuffix(u.destDir, "/") + "/" + u.name

	if rc.CheckMode {
		return unitCheck(rc, u, data, deployFile, dest)
	}
	return unitApply(rc, u, data, deployFile, dest)
}

// unitCheck 是 check 模式分支：文件部分由 putFile 预估，服务部分由
// estimateUnitState 只探测返回预估；--diff 聚合文件与服务两类 diff。
func unitCheck(rc *RunContext, u *unitReq, data []byte, deployFile bool, dest string) *Result {
	var fileChanged bool
	var fileRes *Result
	if deployFile {
		fileChanged, fileRes = putFile(rc, putFileOpts{data: data, dest: dest, mode: modePtr(0o644)})
		if fileRes != nil && fileRes.Failed {
			return fileRes
		}
	}
	if fileRes == nil {
		fileRes = &Result{}
	}
	svcWould, svcActions, svcDiff, bad := estimateUnitState(rc, u.name, u.state, u.hasState, u.enabled, u.hasEnabled)
	if bad != nil {
		return bad
	}
	would := fileChanged || svcWould
	var actions []string
	if fileChanged {
		actions = append(actions, "unit file would be written"+boolTo(u.daemonReload && fileChanged, " and daemon-reload", ""))
	}
	actions = append(actions, svcActions...)
	msg := fmt.Sprintf("[check] %s no change", u.name)
	if would {
		msg = fmt.Sprintf("[check] %s: %s", u.name, strings.Join(actions, ", "))
	}
	res := &Result{Changed: would, Msg: msg}
	if rc.DiffMode { // 与其他模块一致：Diff 仅在 --diff 下填充
		var diffLines []string
		if fileRes.Diff != "" {
			diffLines = append(diffLines, fileRes.Diff)
		}
		diffLines = append(diffLines, svcDiff...)
		res.Diff = strings.Join(diffLines, "\n")
	}
	return res
}

// unitApply 是实跑分支：文件落盘（幂等 + 备份 + 回滚登记全由 putFile 承担，
// 纯状态管理跳过）+ 按需 daemon-reload + 服务状态与自启收敛。
func unitApply(rc *RunContext, u *unitReq, data []byte, deployFile bool, dest string) *Result {
	var fileChanged bool
	if deployFile {
		changed, res := putFile(rc, putFileOpts{data: data, dest: dest, mode: modePtr(0o644)})
		if res != nil {
			return res
		}
		fileChanged = changed
	}
	changed := false
	var actions []string
	if fileChanged {
		changed = true
		actions = append(actions, "wrote unit file")
		if u.daemonReload {
			if out, bad := rc.exec("systemctl daemon-reload"); bad != nil {
				return bad
			} else if out.Code != 0 {
				return Fail("systemctl daemon-reload failed: %s", firstLine(out.Stderr))
			}
			actions = append(actions, "daemon-reload done")
		}
	}

	// 服务状态与自启（svcStatePlan/svcEnablePlan 内核，与 service 模块同口径）
	svcChanged, svcActions, bad := applyUnitState(rc, u.name, u.state, u.hasState, u.enabled, u.hasEnabled)
	if bad != nil {
		return bad
	}
	if svcChanged {
		changed = true
		actions = append(actions, svcActions...)
	}
	msg := fmt.Sprintf("%s no change", u.name)
	if changed {
		msg = fmt.Sprintf("%s: %s", u.name, strings.Join(actions, ", "))
	}
	return &Result{Changed: changed, Msg: msg}
}

// unitSvcPlan 是 systemd_unit 服务部分（state/enabled）的动作计划：动词
// 为空串表示该项已收敛、无需动作。check 预估与实跑共用同一份计划
// （现状探测与动词推导只做一次），预估照计划展示、实跑照计划执行，
// 平行的两份实现就此合一。
type unitSvcPlan struct {
	stateVerb  string // state 动作的 systemctl 子命令（svcStatePlan 推导）
	active     bool   // 探测到的当前运行态（check diff 展示用）
	enableVerb string // 自启动作的 systemctl 子命令（svcEnablePlan 推导）
	enabledNow bool   // 探测到的当前自启态（check diff 展示用）
}

// planUnitState 探测服务现状并推导动作计划（estimateUnitState 与
// applyUnitState 共用；决策内核 svcStatePlan/svcEnablePlan 与 service 模块
// 同源）。注意：is-enabled 探测先于 state 动作执行——start/stop/restart
// 不改自启 symlink，探测结论不因前序动作漂移。
func planUnitState(rc *RunContext, name, state string, hasState bool, enabled, hasEnabled bool) (*unitSvcPlan, *Result) {
	p := &unitSvcPlan{}
	if hasState {
		active, bad := isActive(rc, name)
		if bad != nil {
			return nil, bad
		}
		p.active = active
		p.stateVerb, _ = svcStatePlan(state, active)
	}
	if hasEnabled {
		enabledNow, bad := isEnabled(rc, name)
		if bad != nil {
			return nil, bad
		}
		p.enabledNow = enabledNow
		p.enableVerb = svcEnablePlan(enabled, enabledNow)
	}
	return p, nil
}

// estimateUnitState check 模式预估 state/enabled 变化，
// 返回（是否变更、动作描述、diff 行、失败）。变更判定与动词推导走
// planUnitState 共用计划，与实跑（applyUnitState）、service 模块保持同一决策。
func estimateUnitState(rc *RunContext, name string, state string, hasState bool, enabled, hasEnabled bool) (bool, []string, []string, *Result) {
	p, bad := planUnitState(rc, name, state, hasState, enabled, hasEnabled)
	if bad != nil {
		return false, nil, nil, bad
	}
	would := false
	var actions, diff []string
	if p.stateVerb != "" {
		would = true
		actions = append(actions, "would "+verbFor(state))
		diff = append(diff,
			fmt.Sprintf("- %s: %s", name, boolTo(p.active, "active", "inactive")),
			fmt.Sprintf("+ %s: %s", name, wantState(state)))
	}
	if p.enableVerb != "" {
		would = true
		actions = append(actions, "would adjust autostart")
		diff = append(diff,
			fmt.Sprintf("- %s: %s", name, boolTo(p.enabledNow, "enabled", "disabled")),
			fmt.Sprintf("+ %s: %s", name, boolTo(enabled, "enabled", "disabled")))
	}
	return would, actions, diff, nil
}

// applyUnitState 实际执行 state/enabled 变更，返回（是否变更、动作描述、
// 失败）。动词推导与执行基于 planUnitState 共用计划 + svcExec 内核，
// 与 service 模块、check 预估（estimateUnitState）保持同一决策。
func applyUnitState(rc *RunContext, name string, state string, hasState bool, enabled, hasEnabled bool) (bool, []string, *Result) {
	p, bad := planUnitState(rc, name, state, hasState, enabled, hasEnabled)
	if bad != nil {
		return false, nil, bad
	}
	changed := false
	var actions []string
	if p.stateVerb != "" {
		if bad := svcExec(rc, name, p.stateVerb); bad != nil {
			return false, nil, bad
		}
		changed = true
		actions = append(actions, p.stateVerb)
	}
	if p.enableVerb != "" {
		if bad := svcExec(rc, name, p.enableVerb); bad != nil {
			return false, nil, bad
		}
		changed = true
		actions = append(actions, p.enableVerb)
	}
	return changed, actions, nil
}
