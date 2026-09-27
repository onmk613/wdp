package module

import (
	"fmt"
	"strconv"
	"strings"

	"wdp/internal/shellquote"
)

func init() {
	Register(&PackageModule{})
}

// packageStates 是 package.state 的合法值域：解析校验与 Params 文档共用
// 同一变量（文档长在实现旁边）。
var packageStates = []string{"present", "latest", "absent"}

// PackageModule 跨发行版的包管理（自动识别 apt/dnf/yum/apk/zypper）。
type PackageModule struct{}

func (m *PackageModule) Name() string { return "package" }

func (m *PackageModule) Desc() string {
	return "install/remove packages (auto-detects the package manager)"
}

// pkgPlan 是单包的动作计划：kind 取 install/remove/upgrade（需要执行）
// 或 latest/none（无需执行；latest 仅用于日志措辞区分"已是最新"）。
type pkgPlan struct {
	name string
	kind string
}

// planPackages 探测逐包安装/升级状态并推导动作计划——check 展示与实跑
// 执行共用同一决策树（此前 check 循环与实跑循环各写一份，口径易漂移）。
// 探测全部只读（installed/upgradable）；实跑先整体计划再依序执行，
// 同批前序安装拉入依赖时，后续包的探测结论是计划时点的快照（对已装包
// 重复执行包管理器命令是幂等 no-op，仅日志措辞不同）。
func planPackages(rc *RunContext, mgr *pkgManager, names []string, state string) ([]pkgPlan, *Result) {
	plans := make([]pkgPlan, 0, len(names))
	for _, name := range names {
		installed, bad := mgr.installed(rc, name)
		if bad != nil {
			return nil, bad
		}
		kind := "none"
		switch {
		case state == "absent" && installed:
			kind = "remove"
		case state != "absent" && !installed:
			kind = "install"
		case state == "latest" && installed:
			// 幂等：先探测是否存在可用升级，无升级不进计划（不再每次无脑 upgrade）
			up, bad := mgr.upgradable(rc, name)
			if bad != nil {
				return nil, bad
			}
			if up {
				kind = "upgrade"
			} else {
				kind = "latest"
			}
		}
		plans = append(plans, pkgPlan{name: name, kind: kind})
	}
	return plans, nil
}

// Run 执行包操作：解析 → 探测包管理器 → 逐包计划（唯一决策树）→
// check 展示或实跑执行（骨架与 user 模块一致）。
func (m *PackageModule) Run(rc *RunContext, args map[string]any, _ string) *Result {
	names, ok := argStrList(args, "name")
	if !ok || len(names) == 0 {
		return Fail("package requires a name parameter")
	}
	state, ok := parseState(args, "present", packageStates...)
	if !ok {
		return Fail("unsupported state %q (options: %s)", state, strings.Join(packageStates, "/"))
	}

	mgr, bad := detectPkgManager(rc)
	if bad != nil {
		return bad
	}

	plans, bad := planPackages(rc, mgr, names, state)
	if bad != nil {
		return bad
	}
	if rc.CheckMode {
		return packageCheck(rc, plans)
	}
	return packageApply(rc, mgr, plans)
}

// packageCheck 是 check 模式分支：按计划展示变更预估（--diff 列出逐包
// 增删；latest 的升级探测在 planPackages 已真实查询，与实跑判定一致）。
func packageCheck(rc *RunContext, plans []pkgPlan) *Result {
	would := false
	var logs []string
	var diffLines []string
	for _, p := range plans {
		switch p.kind {
		case "remove":
			would = true
			logs = append(logs, p.name+" will be removed")
			diffLines = append(diffLines, "- "+p.name)
		case "install":
			would = true
			logs = append(logs, p.name+" will be installed")
			diffLines = append(diffLines, "+ "+p.name)
		case "upgrade":
			would = true
			logs = append(logs, p.name+" will be upgraded")
			diffLines = append(diffLines, "~ "+p.name)
		case "latest":
			logs = append(logs, p.name+" already latest")
		default:
			logs = append(logs, p.name+" is already in the target state")
		}
	}
	res := &Result{Changed: would, Msg: "[check] " + strings.Join(logs, "; ")}
	if rc.DiffMode { // 与其他模块一致：Diff 仅在 --diff 下填充
		res.Diff = strings.Join(diffLines, "\n")
	}
	return res
}

// packageApply 实跑分支：按计划依序执行逐包动作。become 守卫只卡写操作
// （installed/upgradable 是只读探测，已在 planPackages 完成，不受守卫
// 影响），install/remove/upgrade 是系统级变更——非 root 未 become 时
// apt-get/dnf 会以远端晦涩报错失败（甚至源锁死），与 user/group 模块
// 同口径显式拦截（报错文案一致）。
func packageApply(rc *RunContext, mgr *pkgManager, plans []pkgPlan) *Result {
	changed := false
	var logs []string
	for _, p := range plans {
		switch p.kind {
		case "remove":
			if !rc.Become {
				return Fail("removing package %s requires become: true", p.name)
			}
			if bad := mgr.remove(rc, p.name); bad != nil {
				return bad
			}
			changed = true
			logs = append(logs, p.name+" removed")
		case "install":
			if !rc.Become {
				return Fail("installing package %s requires become: true", p.name)
			}
			if bad := mgr.install(rc, p.name); bad != nil {
				return bad
			}
			changed = true
			logs = append(logs, p.name+" installed")
		case "upgrade":
			if !rc.Become {
				return Fail("upgrading package %s requires become: true", p.name)
			}
			if bad := mgr.upgrade(rc, p.name); bad != nil {
				return bad
			}
			changed = true
			logs = append(logs, p.name+" upgraded")
		case "latest":
			logs = append(logs, p.name+" already latest")
		default:
			logs = append(logs, p.name+" is already in the target state")
		}
	}
	return &Result{Changed: changed, Msg: strings.Join(logs, "; ")}
}

// pkgManager 是探测到的目标机包管理器。
type pkgManager struct {
	kind   string // apt | dnf | yum | apk | zypper
	family string // debian | redhat | alpine | suse
}

func detectPkgManager(rc *RunContext) (*pkgManager, *Result) {
	script := `[ -f /etc/os-release ] && . /etc/os-release
echo "id=${ID:-unknown}"
echo "like=${ID_LIKE:-}"`
	out, bad := rc.exec(script)
	if bad != nil {
		return nil, bad
	}
	id, like := "", ""
	for line := range strings.SplitSeq(out.Stdout, "\n") {
		k, v, _ := strings.Cut(line, "=")
		switch strings.TrimSpace(k) {
		case "id":
			id = v
		case "like":
			like = v
		}
	}
	sig := id + " " + like
	contains := func(keys ...string) bool {
		for _, k := range keys {
			if strings.Contains(sig, k) {
				return true
			}
		}
		return false
	}
	switch {
	case contains("debian", "ubuntu"):
		return &pkgManager{kind: "apt", family: "debian"}, nil
	case contains("alpine"):
		return &pkgManager{kind: "apk", family: "alpine"}, nil
	case contains("rhel", "fedora", "centos", "rocky", "alma", "amazon"):
		// 探测 dnf 是否可用，否则回退 yum
		out, bad := rc.exec("command -v dnf >/dev/null 2>&1")
		if bad != nil {
			return nil, bad
		}
		kind := "yum"
		if out.Code == 0 {
			kind = "dnf"
		}
		return &pkgManager{kind: kind, family: "redhat"}, nil
	case contains("suse", "opensuse"):
		return &pkgManager{kind: "zypper", family: "suse"}, nil
	default:
		return nil, Fail("unable to detect the package manager (os id=%s like=%s)", id, like)
	}
}

func (p *pkgManager) installed(rc *RunContext, name string) (bool, *Result) {
	var script string
	switch p.kind {
	case "apt":
		script = fmt.Sprintf(`dpkg-query -W -f='${Status}' %s 2>/dev/null | grep -q 'install ok installed'`, shellquote.Quote(name))
	case "dnf", "yum":
		script = fmt.Sprintf("rpm -q %s >/dev/null 2>&1", shellquote.Quote(name))
	case "apk":
		script = fmt.Sprintf("apk info -e %s >/dev/null 2>&1", shellquote.Quote(name))
	case "zypper":
		script = fmt.Sprintf("rpm -q %s >/dev/null 2>&1", shellquote.Quote(name))
	}
	out, bad := rc.exec(script)
	if bad != nil {
		return false, bad
	}
	return out.Code == 0, nil
}

func (p *pkgManager) install(rc *RunContext, name string) *Result {
	return p.run(rc, p.cmd("install", name))
}

func (p *pkgManager) remove(rc *RunContext, name string) *Result {
	// 各包管理器默认动词均为 remove，仅 apk 用 del
	verb := "remove"
	if p.kind == "apk" {
		verb = "del"
	}
	return p.run(rc, p.cmd(verb, name))
}

func (p *pkgManager) upgrade(rc *RunContext, name string) *Result {
	switch p.kind {
	case "apt":
		return p.run(rc, fmt.Sprintf("DEBIAN_FRONTEND=noninteractive apt-get install -y --only-upgrade %s", shellquote.Quote(name)))
	case "dnf", "yum":
		return p.run(rc, fmt.Sprintf("%s update -y %s", p.kind, shellquote.Quote(name)))
	case "apk":
		return p.run(rc, fmt.Sprintf("apk add --upgrade %s", shellquote.Quote(name)))
	case "zypper":
		return p.run(rc, fmt.Sprintf("zypper update -y %s", shellquote.Quote(name)))
	}
	return Fail("unknown package manager %s", p.kind)
}

// upgradable 探测包是否存在可用升级（state: latest 的幂等判定依据）。
// 探测命令不带管道（sh 管道退出码取最后一段，包管理器自身错误——源不可用、
// 锁冲突——会被 awk/grep 吞成"无升级"，造成假幂等），输出在控制端解析；
// 探测失败返回错误，不静默跳过。
func (p *pkgManager) upgradable(rc *RunContext, name string) (bool, *Result) {
	q := shellquote.Quote(name)
	var script string
	switch p.kind {
	case "apt":
		// 模拟升级输出 "N upgraded, M newly installed" 汇总行，
		// "already the newest version" 时无该行
		script = fmt.Sprintf("LC_ALL=C DEBIAN_FRONTEND=noninteractive apt-get -s install --only-upgrade %s", q) // LC_ALL=C：汇总行解析依赖英文文案
	case "dnf", "yum":
		// check-update 语义：100 = 有可用更新，0 = 无，其它 = 错误
		script = fmt.Sprintf("%s check-update %s", p.kind, q)
	case "apk":
		// -l '<' 由 apk 自身按包名过滤，仅输出存在升级的包行
		script = fmt.Sprintf("apk version -l '<' %s", q)
	case "zypper":
		// list-updates 的位置参数是仓库名而非包名（此前误传包名恒报
		// "无升级"），改为全量列表 + 控制端按包名列精确匹配
		script = "zypper --non-interactive list-updates"
	default:
		return false, Fail("unknown package manager %s", p.kind)
	}
	out, bad := rc.exec(script)
	if bad != nil {
		return false, bad
	}
	probeFail := func() (bool, *Result) {
		return false, Fail("upgrade detection failed rc=%d: %s", out.Code, firstLine(out.Stderr))
	}
	switch p.kind {
	case "apt":
		if out.Code != 0 {
			return probeFail()
		}
		for line := range strings.SplitSeq(out.Stdout, "\n") {
			f := strings.Fields(line)
			if len(f) >= 2 && strings.TrimSuffix(f[1], ",") == "upgraded" {
				n, err := strconv.Atoi(f[0])
				return err == nil && n > 0, nil
			}
		}
		return false, nil
	case "dnf", "yum":
		if out.Code != 0 && out.Code != 100 {
			return probeFail()
		}
		return out.Code == 100, nil
	case "apk":
		if out.Code != 0 {
			return probeFail()
		}
		// apk 已按包名过滤，存在任何输出行即存在升级
		return strings.TrimSpace(out.Stdout) != "", nil
	case "zypper":
		if out.Code != 0 {
			return probeFail()
		}
		// 表格式 "S | Repository | Name | Current | Available | Arch"，
		// Name 列（第 3 列）精确匹配（表头/分隔行不会等于包名）
		for line := range strings.SplitSeq(out.Stdout, "\n") {
			cols := strings.Split(line, "|")
			if len(cols) >= 6 && strings.TrimSpace(cols[2]) == name {
				return true, nil
			}
		}
		return false, nil
	}
	return false, nil
}

func (p *pkgManager) cmd(verb, name string) string {
	switch p.kind {
	case "apt":
		return fmt.Sprintf("DEBIAN_FRONTEND=noninteractive apt-get %s -y %s", verb, shellquote.Quote(name))
	case "apk":
		return fmt.Sprintf("apk %s %s", verb, shellquote.Quote(name))
	case "zypper":
		return fmt.Sprintf("zypper --non-interactive %s %s", verb, shellquote.Quote(name))
	default: // dnf / yum
		return fmt.Sprintf("%s %s -y %s", p.kind, verb, shellquote.Quote(name))
	}
}

func (p *pkgManager) run(rc *RunContext, script string) *Result {
	out, bad := rc.exec(script)
	if bad != nil {
		return bad
	}
	if out.Code != 0 {
		return Fail("package operation failed rc=%d: %s", out.Code, firstLine(out.Stderr))
	}
	return nil
}

func (m *PackageModule) Params() []ParamDoc {
	return []ParamDoc{
		{Name: "name", Type: "list", Desc: "package name(s) (whitespace-separated string or list, required)"},
		{Name: "state", Type: "string", Default: "present", Enum: packageStates, Desc: "desired state (auto-detects apt/dnf/yum/apk/zypper)"},
	}
}

func (m *PackageModule) Example() string {
	return `- name: install dependencies
  package:
    name: [curl, jq]
    state: present
`
}
