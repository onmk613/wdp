package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"wdp/internal/config"
	"wdp/internal/i18n"

	// ssh / agent / local / push
	_ "wdp/internal/connection/agentconn"
	_ "wdp/internal/connection/localconn"
	_ "wdp/internal/connection/pushagent"
	_ "wdp/internal/connection/sshconn"
)

var (
	Version   = "0.0.1"
	Commit    = "none"
	BuildDate = "unknown"
	GoVersion = "unknown"
)

func version() string {
	return fmt.Sprintf("%s \ngolang %s \ncommit %s\nbuilt %s", Version, GoVersion, Commit, BuildDate)
}

// 全局选项（由 PersistentFlags 绑定；未显式指定时默认值可被 wdp.cfg 覆盖）。
var (
	gConfig      string
	gInventories []string
	gForks       int
	gTimeout     int  // 全局 wall-clock 超时秒数，0 不限
	gTaskTimeout int  // 单任务默认超时秒数，0 不限
	gVerbosity   int  // -v 计数：0 聚合 / 1 逐主机 / 2 全量输出 / 3 调试
	gQuiet       bool // -q：仅异常与 RECAP
	gNoColor     bool
	gOutput      string // console | json
	gLang        string // auto | zh | en
	gMaxDownMB   int64  // get_url 下载响应体上限 MiB，0 = 跟随 wdp.cfg / 内置默认
)

// 命令分组（`wdp --help` 按组分类展示；GroupID 在命令挂载时统一设置）。
// 分组同时对应源码文件布局：run.go（部署）· new.go/chartcmd.go（应用包）·
// ca.go/scan-ssh.go（安全与信任）· agent.go（代理）· ops.go（运维与记录）。
const (
	groupDeploy   = "deploy"
	groupChart    = "chart"
	groupSecurity = "security"
	groupAgent    = "agent"
	groupOps      = "ops"
)

// LangEnv 是语言覆盖环境变量（优先级低于 --lang）。
const LangEnv = "WDP_LANG"

// resolveLangEarly 在构造命令树前解析输出语言。
// cobra 的帮助文案（Short/flag 描述）在命令构造期固化为当前语言字符串，
// 因此必须在 NewRootCmd 之前完成解析；--lang 可出现在任意子命令位置。
// 优先级：os.Args 中的 --lang > WDP_LANG > auto（时区 + locale 自动检测）。
func resolveLangEarly() {
	pref := os.Getenv(LangEnv)
	for i, a := range os.Args {
		if !strings.HasPrefix(a, "--lang") {
			continue
		}
		if v, ok := strings.CutPrefix(a, "--lang="); ok {
			pref = v
			break
		}
		if a == "--lang" && i+1 < len(os.Args) {
			pref = os.Args[i+1]
			break
		}
	}
	i18n.Resolve(pref)
}

// NewRootCmd 构造根命令与子命令树
func NewRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "wdp",
		Short:         "wdp — an automation & deployment tool",
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       version(),
	}

	pf := root.PersistentFlags()
	pf.StringVar(&gConfig, "config", config.DefaultPath,
		i18n.T("config file path (TOML, default "+config.DefaultPath+" in current dir)",
			"配置文件路径（TOML，默认当前目录 "+config.DefaultPath+"）"))
	pf.StringArrayVarP(&gInventories, "inventory", "i", nil,
		i18n.T("inventory file path (repeatable, later merges over earlier; default inventory.yaml)",
			"inventory 文件路径（可多次指定，后者覆盖合并；缺省 inventory.yaml）"))
	pf.IntVar(&gForks, "forks", 5, i18n.T("host concurrency", "并发主机数"))
	pf.IntVar(&gTimeout, "timeout", 0, i18n.T("global timeout in seconds, 0 = unlimited", "全局超时（秒），0 不限"))
	pf.IntVar(&gTaskTimeout, "task-timeout", 0,
		i18n.T("default task timeout in seconds, 0 = unlimited; overridable per task",
			"任务默认超时（秒），0 不限；可被任务级 timeout 覆盖"))
	pf.CountVarP(&gVerbosity, "verbose", "v",
		i18n.T("verbosity (repeatable): -v per-host / -vv full stdout/stderr & loop items / -vvv debug",
			"输出详细级别（可重复）：-v 逐主机 / -vv 全量 stdout/stderr 与 loop 逐项 / -vvv 调试"))
	pf.BoolVarP(&gQuiet, "quiet", "q", false,
		i18n.T("quiet: only failed hosts and RECAP", "静默模式：仅输出异常主机与 RECAP"))
	pf.BoolVar(&gNoColor, "no-color", false, i18n.T("disable colored output", "禁用颜色输出"))
	pf.StringVar(&gOutput, "output", "console",
		i18n.T("output format: console | json (machine-readable, for CI/CD)",
			"输出格式：console | json（机器可读，适配 CI/CD）"))
	pf.StringVar(&gLang, "lang", "auto",
		i18n.T("output language: auto | zh | en (or "+LangEnv+" env; affects help, module docs, messages)",
			"输出语言：auto | zh | en（或环境变量 "+LangEnv+"；作用于帮助、模块文档与提示文案）"))
	pf.Int64Var(&gMaxDownMB, "max-download-mb", 0,
		i18n.T("get_url download body size limit in MiB (0 = follow wdp.cfg [transfer], default 2048)",
			"get_url 下载响应体上限（MiB，0 = 跟随 wdp.cfg [transfer]，默认 2048）"))

	// 子命令 RunE 前统一加载 wdp.cfg，未显式指定的全局 flag 回退配置值
	root.PersistentPreRunE = func(cmd *cobra.Command, args []string) error {
		// 语言已在 resolveLangEarly 解析（env / args 预扫描）；
		// 仅显式 --lang 时重解析，避免 flag 默认值 "auto" 覆盖 WDP_LANG。
		if pf.Changed("lang") {
			i18n.Resolve(gLang)
		}
		// --config 显式指定时文件必须存在；默认路径不存在则静默跳过（保持内置默认）
		if err := config.Load(gConfig, pf.Changed("config")); err != nil {
			return err
		}
		c := config.Current()
		if !pf.Changed("inventory") {
			gInventories = []string{c.InventoryPath()}
		}
		if !pf.Changed("forks") {
			gForks = c.Forks()
		}
		if !pf.Changed("timeout") {
			gTimeout = c.Run.Timeout
		}
		if !pf.Changed("task-timeout") {
			gTaskTimeout = c.Run.TaskTimeout
		}
		if !pf.Changed("verbose") {
			if c.Run.Verbose {
				gVerbosity = 1 // wdp.cfg run.verbose 兼容映射
			}
		}
		if !pf.Changed("no-color") {
			gNoColor = !c.Color()
		}
		if !pf.Changed("max-download-mb") {
			gMaxDownMB = int64(c.Transfer.MaxDownloadMB)
		}
		return nil
	}

	// 命令分组声明（顺序即 --help 中的展示顺序）
	root.AddGroup(
		&cobra.Group{ID: groupDeploy, Title: "Deployment"},
		&cobra.Group{ID: groupChart, Title: "Package"},
		&cobra.Group{ID: groupSecurity, Title: "Security"},
		&cobra.Group{ID: groupAgent, Title: "Agent"},
		&cobra.Group{ID: groupOps, Title: "Operations"},
	)

	// grouped 挂载命令到指定分组
	grouped := func(gid string, c *cobra.Command) *cobra.Command {
		c.GroupID = gid
		return c
	}
	root.AddCommand(
		grouped(groupDeploy, newRunCmd()),
		grouped(groupDeploy, newAdhocCmd()),
		grouped(groupChart, newNewCmd()),
		grouped(groupChart, newTemplateCmd()),
		grouped(groupChart, newLintCmd()),
		grouped(groupChart, newPackageCmd()),
		grouped(groupSecurity, newCACmd()),
		grouped(groupSecurity, newScanSshCmd()),
		grouped(groupAgent, newAgentCmd()),
		grouped(groupOps, newReleaseCmd()),
		grouped(groupOps, newModulesCmd()),
	)
	return root
}

// Execute 执行根命令，返回进程退出码。
func Execute() int {
	resolveLangEarly()
	root := NewRootCmd()
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, i18n.T("error:", "错误:"), err)
		return 1
	}
	return 0
}
