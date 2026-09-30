package cli

import (
	"fmt"
	"os"
	"wdp/internal/buildinfo"

	"github.com/spf13/cobra"

	"wdp/internal/config"
	"wdp/internal/i18n"
	"wdp/internal/i18nhelp"
)

// cmd --version Printf（值经 buildinfo 别名——ldflags 注入点在 buildinfo，
// cli 侧保持既有引用不改动）
var (
	Version   = buildinfo.Version
	Commit    = buildinfo.Commit
	BuildDate = buildinfo.BuildDate
	GoVersion = buildinfo.GoVersion
)

func version() string {
	return fmt.Sprintf("%s\ngolang %s\ncommit %s\nbuilt %s\ntier %s", Version, GoVersion, Commit, BuildDate, buildinfo.Tier)
}

// 命令分组
type commandGroup struct {
	id    string
	title string
	cmd   []*cobra.Command
}

func addCommandGroups(root *cobra.Command, groups ...commandGroup) {
	for _, g := range groups {
		root.AddGroup(&cobra.Group{ID: g.id, Title: g.title})
		for _, c := range g.cmd {
			c.GroupID = g.id
			root.AddCommand(c)
		}
	}
}

var (
	gInventories       []string // -i 可重复；未指定时 PersistentPreRunE 回填 config 默认
	gInventoryExplicit bool     // -i 是否用户显式指定--inventory（和--hosts 互斥判定用）
	gVerbosity         int      // -v 计数：0 聚合 / 1 逐主机 / 2 全量输出 / 3 调试
	gQuiet             bool     // -q：仅异常与 RECAP
	gOutput            string   // console | json
)

// NewRootCmd 构造根命令与子命令树
func NewRootCmd() *cobra.Command {
	// help 按注册序展示（每组内语义排序：如 schema 在 module 前——结构
	// 骨架参考先于具体模块文档），而非字母序
	cobra.EnableCommandSorting = false
	root := &cobra.Command{
		Use:           "wdp",
		Short:         i18n.T("wdp — an automation & deployment tool", "wdp — 自动化与部署工具"),
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       version(),
	}

	// 文件可配置项的 flag 绑定到局部变量，仅在显式指定时覆盖 config
	cfgPath := config.DefaultPath
	var flForks, flTimeout, flTaskTimeout int
	var flMaxDown int64
	var flNoColor bool

	pf := root.PersistentFlags()
	pf.StringVar(&cfgPath, "config", config.DefaultPath, "config file path (TOML, default "+config.DefaultPath+" in current dir)")
	pf.StringArrayVarP(&gInventories, "inventory", "i", nil, i18n.T(
		"inventory file path (repeatable, later merges over earlier; default inventory.yaml)",
		"清单文件路径（可重复，后者覆盖前者合并；默认 inventory.yaml）"))
	pf.IntVar(&flForks, "forks", 5, "host concurrency")
	pf.IntVar(&flTimeout, "timeout", 0, i18n.T(
		"global timeout in seconds, 0 = unlimited",
		"全局超时（秒），0 = 不限制"))
	pf.IntVar(&flTaskTimeout, "task-timeout", 0, i18n.T(
		"default task timeout in seconds, 0 = unlimited; overridable per task",
		"任务默认超时（秒），0 = 不限制；可按任务单独覆盖"))
	pf.CountVarP(&gVerbosity, "verbose", "v", i18n.T(
		"verbosity (repeatable): -v per-host / -vv full stdout/stderr & loop items / -vvv debug",
		"输出详细度（可重复）：-v 逐主机 / -vv 全量 stdout/stderr 与循环项 / -vvv 调试"))
	pf.BoolVarP(&gQuiet, "quiet", "q", false, "quiet: only failed hosts and RECAP")
	pf.BoolVar(&flNoColor, "no-color", false, "disable colored output")
	pf.StringVar(&gOutput, "output", "console", "output format: console | json (machine-readable; applies to run/apply/adhoc/drift, other commands print plain tables)")
	pf.Int64Var(&flMaxDown, "max-download-mb", 0, i18n.T(
		"get_url download body size limit in MiB (0 = follow wdp.cfg [transfer], default 2048)",
		"get_url 下载体积上限（MiB，0 = 跟随 wdp.cfg [transfer]，默认 2048）"))

	// 子命令 RunE 前统一加载 wdp.cfg，再把显式指定的 flag 覆盖进当前配置
	root.PersistentPreRunE = func(cmd *cobra.Command, args []string) error {
		// --output 枚举校验：拼错（如 jons）静默按 console 处理曾让 CI 误读输出
		if gOutput != "console" && gOutput != "json" {
			return fmt.Errorf("invalid --output %q (options: console | json)", gOutput)
		}
		// --config 显式指定时文件必须存在；默认路径不存在则静默跳过（保持内置默认）
		if err := config.Load(cfgPath, pf.Changed("config")); err != nil {
			return err
		}
		c := config.Current()
		if pf.Changed("forks") {
			c.Run.Forks = flForks
		}
		if pf.Changed("timeout") {
			c.Run.Timeout = flTimeout
		}
		if pf.Changed("task-timeout") {
			c.Run.TaskTimeout = flTaskTimeout
		}
		if pf.Changed("no-color") {
			c.Output.Color = new(!flNoColor)
		}
		if pf.Changed("max-download-mb") {
			c.Transfer.MaxDownloadMB = int(flMaxDown)
		}
		if !pf.Changed("inventory") {
			gInventories = []string{c.InventoryPath()}
		}
		if !pf.Changed("verbose") && c.Run.Verbose {
			gVerbosity = 1
		}
		// 显式性记录：--hosts 内联模式只与"用户显式 -i"互斥，
		// 不与 PersistentPreRunE 回填的 config 默认值互斥
		gInventoryExplicit = pf.Changed("inventory")
		return nil
	}

	registerCommandGroups(root)
	i18nhelp.Setup(root)

	return root
}

// Execute 执行根命令，返回进程退出码。
func Execute() int {
	if err := NewRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	return 0
}
