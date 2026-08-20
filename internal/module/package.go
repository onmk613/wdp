package module

import (
	"fmt"
	"strings"

	"wdp/internal/i18n"
	"wdp/internal/shellquote"
)

func init() {
	Register(&PackageModule{})
}

// PackageModule 跨发行版的包管理（自动识别 apt/dnf/yum/apk/zypper）。
type PackageModule struct{}

// Name 模块名。
func (m *PackageModule) Name() string { return "package" }

// Desc 模块说明。
func (m *PackageModule) Desc() string {
	return i18n.T("install/remove packages (auto-detects the package manager)", "安装/卸载软件包（自动识别包管理器）")
}

// Run 执行包操作。
func (m *PackageModule) Run(rc *RunContext, args map[string]any, free string) *Result {
	names, ok := argStrList(args, "name")
	if !ok || len(names) == 0 {
		return Fail("package 需要 name 参数")
	}
	state, _ := argStr(args, "state")
	switch state {
	case "", "present", "latest", "absent":
	default:
		return Fail("不支持的 state %q（可选: present/latest/absent）", state)
	}

	mgr, bad := detectPkgManager(rc)
	if bad != nil {
		return bad
	}

	// check 模式：只探测当前安装状态，返回变更预估（--diff 列出逐包增删；
	// latest 会真实查询可用升级，与实跑判定一致）
	if rc.CheckMode {
		would := false
		var logs []string
		var diffLines []string
		for _, name := range names {
			installed, bad := mgr.installed(rc, name)
			if bad != nil {
				return bad
			}
			switch {
			case state == "absent" && installed:
				would = true
				logs = append(logs, name+" 将卸载")
				diffLines = append(diffLines, "- "+name)
			case state != "absent" && !installed:
				would = true
				logs = append(logs, name+" 将安装")
				diffLines = append(diffLines, "+ "+name)
			case state == "latest" && installed:
				up, bad := mgr.upgradable(rc, name)
				if bad != nil {
					return bad
				}
				if up {
					would = true
					logs = append(logs, name+" 将升级")
					diffLines = append(diffLines, "~ "+name)
				} else {
					logs = append(logs, name+" 已是最新")
				}
			default:
				logs = append(logs, name+" 已是目标状态")
			}
		}
		return &Result{Changed: would, Msg: "[check] " + strings.Join(logs, "; "), Diff: strings.Join(diffLines, "\n")}
	}

	changed := false
	var logs []string
	for _, name := range names {
		installed, bad := mgr.installed(rc, name)
		if bad != nil {
			return bad
		}
		switch {
		case state == "absent" && installed:
			if bad := mgr.remove(rc, name); bad != nil {
				return bad
			}
			changed = true
			logs = append(logs, name+" 已卸载")
		case state != "absent" && !installed:
			if bad := mgr.install(rc, name); bad != nil {
				return bad
			}
			changed = true
			logs = append(logs, name+" 已安装")
		case state == "latest" && installed:
			// 幂等：先探测是否存在可用升级，无升级则跳过（不再每次无脑 upgrade）
			up, bad := mgr.upgradable(rc, name)
			if bad != nil {
				return bad
			}
			if !up {
				logs = append(logs, name+" 已是最新")
				continue
			}
			if bad := mgr.upgrade(rc, name); bad != nil {
				return bad
			}
			changed = true
			logs = append(logs, name+" 已升级")
		default:
			logs = append(logs, name+" 已是目标状态")
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
	for _, line := range strings.Split(out.Stdout, "\n") {
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
		return nil, Fail("无法识别包管理器（os id=%s like=%s）", id, like)
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
	verb := "remove"
	switch p.kind {
	case "apk":
		verb = "del"
	case "zypper":
		verb = "remove"
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
	return Fail("未知包管理器 %s", p.kind)
}

// upgradable 探测包是否存在可用升级（state: latest 的幂等判定依据）。
// 探测失败（如 dnf 源暂时不可用）返回 error，不静默跳过。
func (p *pkgManager) upgradable(rc *RunContext, name string) (bool, *Result) {
	q := shellquote.Quote(name)
	var script string
	switch p.kind {
	case "apt":
		// 模拟升级并解析 "N upgraded" 汇总行：N>0 即存在可用升级。
		// "already the newest version" 时无该行，awk 默认 0 → 无升级。
		script = fmt.Sprintf("DEBIAN_FRONTEND=noninteractive apt-get -s install --only-upgrade %s 2>/dev/null | awk '/^[0-9]+ upgraded/{ if ($1+0 > 0) exit 1 }'", q)
	case "dnf", "yum":
		// check-update 语义：返回 100 = 有可用更新，0 = 无
		script = fmt.Sprintf("%s check-update %s >/dev/null 2>&1; rc=$?; [ $rc -eq 100 ] || [ $rc -eq 0 ] || exit $rc", p.kind, q)
	case "apk":
		// apk version -l '<' 仅列出存在可用升级的包
		script = fmt.Sprintf("apk version -l '<' %s 2>/dev/null | grep -q .", q)
	case "zypper":
		// list-updates 输出表头后存在包行即存在升级
		script = fmt.Sprintf("zypper --non-interactive list-updates %s 2>/dev/null | tail -n +5 | grep -q .", q)
	default:
		return false, Fail("未知包管理器 %s", p.kind)
	}
	out, bad := rc.exec(script)
	if bad != nil {
		return false, bad
	}
	switch p.kind {
	case "dnf", "yum":
		// 脚本保证 rc ∈ {0,100}（其它值已提前 exit 原码）
		return out.Code == 100, nil
	case "apt":
		// awk 发现 >0 行时 exit 1；apt 自身网络错误为其它非零码，不误判
		return out.Code == 1, nil
	default:
		// grep 探测：0 = 有升级，1 = 无升级
		return out.Code == 0, nil
	}
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
		return Fail("包操作失败 rc=%d: %s", out.Code, firstLine(out.Stderr))
	}
	return nil
}

// Params 参数文档。
func (m *PackageModule) Params() []ParamDoc {
	return []ParamDoc{
		{Name: "name", Type: "list", Desc: "包名（字符串空白分割或列表，必需）"},
		{Name: "state", Type: "string", Default: "present", Desc: "present/latest/absent（自动识别 apt/dnf/yum/apk/zypper）"},
	}
}

// Example 示例任务。
func (m *PackageModule) Example() string {
	return `- name: 安装依赖
  package:
    name: [curl, jq]
    state: present
`
}
