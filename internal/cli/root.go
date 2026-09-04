package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"wdp/internal/config"
)

// cmd --version Printf
var (
	Version   = "0.0.1"
	Commit    = "none"
	BuildDate = "unknown"
	GoVersion = "unknown"
)

func version() string {
	return fmt.Sprintf("%s \ngolang %s \ncommit %s\nbuilt %s", Version, GoVersion, Commit, BuildDate)
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
		Short:         "wdp — an automation & deployment tool",
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
	pf.StringArrayVarP(&gInventories, "inventory", "i", nil, "inventory file path (repeatable, later merges over earlier; default inventory.yaml)")
	pf.IntVar(&flForks, "forks", 5, "host concurrency")
	pf.IntVar(&flTimeout, "timeout", 0, "global timeout in seconds, 0 = unlimited")
	pf.IntVar(&flTaskTimeout, "task-timeout", 0, "default task timeout in seconds, 0 = unlimited; overridable per task")
	pf.CountVarP(&gVerbosity, "verbose", "v", "verbosity (repeatable): -v per-host / -vv full stdout/stderr & loop items / -vvv debug")
	pf.BoolVarP(&gQuiet, "quiet", "q", false, "quiet: only failed hosts and RECAP")
	pf.BoolVar(&flNoColor, "no-color", false, "disable colored output")
	pf.StringVar(&gOutput, "output", "console", "output format: console | json (machine-readable, for CI/CD)")
	pf.Int64Var(&flMaxDown, "max-download-mb", 0, "get_url download body size limit in MiB (0 = follow wdp.cfg [transfer], default 2048)")

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

	addCommandGroups(root,
		commandGroup{"deploy", "Deployment", []*cobra.Command{
			newRunCmd(),
			newPlanCmd(),
			newApplyCmd(),
			newAdhocCmd(),
		}},
		commandGroup{"chart", "Package", []*cobra.Command{
			newSchemaCmd(),
			newModuleCmd(),
			newRenderCmd(),
			newLintCmd(),
			newPackageCmd(),
		}},
		commandGroup{"security", "Security", []*cobra.Command{
			newCACmd(),
			newScanSshCmd(),
		}},
		commandGroup{"agent", "Agent", []*cobra.Command{
			newAgentCmd(),
			newAgentCtlCmd(),
		}},
		commandGroup{"ops", "Operations", []*cobra.Command{
			newDriftCmd(),
			newReleaseCmd(),
			newInventoryCmd(),
		}},
	)

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
