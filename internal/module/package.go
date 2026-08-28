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

// PackageModule 跨发行版的包管理（自动识别 apt/dnf/yum/apk/zypper）。
type PackageModule struct{}

// Name 模块名。
func (m *PackageModule) Name() string { return "package" }

// Desc 模块说明。
func (m *PackageModule) Desc() string {
	return "install/remove packages (auto-detects the package manager)"
}

// Run 执行包操作。
func (m *PackageModule) Run(rc *RunContext, args map[string]any, _ string) *Result {
	names, ok := argStrList(args, "name")
	if !ok || len(names) == 0 {
		return Fail("%s", "package requires a name parameter")
	}
	state, ok := parseState(args, "present", "present", "latest", "absent")
	if !ok {
		return Fail("unsupported state %q (options: present/latest/absent)", state)
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
				logs = append(logs, name+" will be removed")
				diffLines = append(diffLines, "- "+name)
			case state != "absent" && !installed:
				would = true
				logs = append(logs, name+" will be installed")
				diffLines = append(diffLines, "+ "+name)
			case state == "latest" && installed:
				up, bad := mgr.upgradable(rc, name)
				if bad != nil {
					return bad
				}
				if up {
					would = true
					logs = append(logs, name+" will be upgraded")
					diffLines = append(diffLines, "~ "+name)
				} else {
					logs = append(logs, name+" already latest")
				}
			default:
				logs = append(logs, name+" is already in the target state")
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
			logs = append(logs, name+" removed")
		case state != "absent" && !installed:
			if bad := mgr.install(rc, name); bad != nil {
				return bad
			}
			changed = true
			logs = append(logs, name+" installed")
		case state == "latest" && installed:
			// 幂等：先探测是否存在可用升级，无升级则跳过（不再每次无脑 upgrade）
			up, bad := mgr.upgradable(rc, name)
			if bad != nil {
				return bad
			}
			if !up {
				logs = append(logs, name+" already latest")
				continue
			}
			if bad := mgr.upgrade(rc, name); bad != nil {
				return bad
			}
			changed = true
			logs = append(logs, name+" upgraded")
		default:
			logs = append(logs, name+" is already in the target state")
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
		script = fmt.Sprintf("DEBIAN_FRONTEND=noninteractive apt-get -s install --only-upgrade %s", q)
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

// Params 参数文档。
func (m *PackageModule) Params() []ParamDoc {
	return []ParamDoc{
		{Name: "name", Type: "list", Desc: "package name(s) (whitespace-separated string or list, required)"},
		{Name: "state", Type: "string", Default: "present", Desc: "present/latest/absent (auto-detects apt/dnf/yum/apk/zypper)"},
	}
}

// Example 示例任务。
func (m *PackageModule) Example() string {
	return `- name: install dependencies
  package:
    name: [curl, jq]
    state: present
`
}
