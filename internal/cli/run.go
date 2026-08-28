package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"wdp/internal/chart"
	"wdp/internal/config"
	"wdp/internal/conn"
	"wdp/internal/executor"
	"wdp/internal/fmtutil"
	"wdp/internal/inventory"
	"wdp/internal/model"
	"wdp/internal/module"
	"wdp/internal/playbook"
	"wdp/internal/release"
	"wdp/internal/report"

	_ "wdp/internal/conn/agentc"
	_ "wdp/internal/conn/local"
	_ "wdp/internal/conn/push"
	_ "wdp/internal/conn/sshc"
)

// newRunCmd 构造 `wdp run`。
func newRunCmd() *cobra.Command {
	opts := runOptions{}
	cmd := &cobra.Command{
		Use:   "run <playbook.yaml|chart-dir|chart.tgz>",
		Short: "run a playbook or chart",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runTarget(cmd.Context(), args[0], opts)
		},
	}
	f := cmd.Flags()
	f.StringVar(&opts.limit, "limit", "", "further limit hosts (group/host/!exclude)")
	f.StringSliceVar(&opts.hostsInline, "hosts", nil,
		"inline host specs instead of an inventory file (IP/host[:port], ranges like 10.8.2.101-104); every play targets these hosts")
	f.StringSliceVarP(&opts.tags, "tags", "t", nil, "run only tasks with these tags (comma-separated)")
	f.StringSliceVar(&opts.skipTags, "skip-tags", nil, "skip tasks with these tags")
	f.BoolVar(&opts.listHosts, "list-hosts", false, "list hosts that would run, then exit")
	f.StringVar(&opts.startAtTask, "start-at-task", "", "start execution at the given task")
	f.BoolVar(&opts.check, "check", false, "check mode: dry-run without applying changes")
	f.BoolVar(&opts.diff, "diff", false, "diff mode: content-level diff with --check (copy/template/file)")
	f.StringVar(&opts.phase, "phase", "deploy", "chart lifecycle phase: deploy | uninstall | status")
	f.BoolVarP(&opts.yes, "yes", "y", false, "skip confirmation of irreversible operations (recommended for CI)")
	f.StringVar(&opts.factCache, "fact-cache", "",
		"persist setup/set_fact facts to this JSON file across runs (loaded at start, saved atomically at end)")
	chartValueFlags(cmd, &opts.valuesFiles, &opts.setArgs)
	return cmd
}

// chartValueFlags 声明 chart 公共 flag（-f/--values/--set）。
func chartValueFlags(cmd *cobra.Command, valuesFiles, setArgs *[]string) {
	cmd.Flags().StringArrayVarP(valuesFiles, "values-file", "f", nil,
		"chart values override files (repeatable, deep-merged in order, like Helm)")
	cmd.Flags().StringArrayVar(setArgs, "set", nil,
		"chart values dot-path overrides (--set a.b[0]=v, repeatable)",
	)
}

// loadInventories 加载全部 -i 清单
func loadInventories() (*inventory.Inventory, error) {
	paths := gInventories
	if len(paths) == 0 {
		paths = []string{"inventory.yaml"}
	}
	return inventory.LoadMergeWithConfig(paths, config.Current())
}

// connDefaults 从 wdp.cfg 归一出连接层默认值（组合根显式注入，
// 连接层自身不依赖 config 包）。归一化职责划分：SSH 用户/超时在 config
// 取值器归一（inventory 烘焙 host 字段共用）；agent 类默认值注入原始值、
// 由 conn.Defaults 的 OrDefault 系列归一（conn 层是唯一消费方）。
func connDefaults() *conn.Defaults {
	c := config.Current()
	return &conn.Defaults{
		SSHUser:             c.SSHUser(),
		SSHConnectTimeout:   c.SSHConnectTimeout(),
		AgentPort:           c.Agent.Port,
		AgentCertRotateMin:  c.AgentCertRotateMin(),
		PushCADir:           c.Agent.PushCADir,
		AgentIdleTimeoutMin: c.AgentIdleTimeoutMin(),
		PushBinary:          c.Agent.PushBinary,
	}
}

// maxDownloadBytes 归一 get_url 下载上限（--max-download-mb > wdp.cfg > 内置默认；
// flag 覆盖已在 PersistentPreRunE 写入 config，0 表示用模块内置默认）。
func maxDownloadBytes() int64 {
	if mb := config.Current().Transfer.MaxDownloadMB; mb > 0 {
		return int64(mb) << 20
	}
	return 0
}

type runOptions struct {
	limit       string
	tags        []string
	skipTags    []string
	listHosts   bool
	startAtTask string
	check       bool
	diff        bool
	phase       string
	yes         bool
	factCache   string
	hostsInline []string
	valuesFiles []string
	setArgs     []string
}

// runTarget 加载目标（chart 或裸 playbook）并执行，返回错误由 cobra 呈现。
func runTarget(ctx context.Context, target string, opts runOptions) error {
	if opts.diff && !opts.check {
		opts.check = true // --diff 基于 check 只读对比，自动启用预演
	}
	// 全局墙钟超时从进入本函数起计时：覆盖 chart 解包、inventory 加载与
	// 交互确认（此前只罩 executor 阶段，长解包/人工确认不消耗超时预算）
	if cfgTimeout := config.Current().Run.Timeout; cfgTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(cfgTimeout)*time.Second)
		defer cancel()
	}
	// 主机来源：--hosts 内联表达式（不写 inventory 文件）或 -i 清单。
	inline := len(opts.hostsInline) > 0
	var inv *inventory.Inventory
	if inline {
		// 只与"用户显式 -i"互斥；gInventories 可能是 PersistentPreRunE
		// 回填的 config 默认值，不代表用户指定了清单
		if gInventoryExplicit {
			return fmt.Errorf("--hosts and -i are mutually exclusive (inline specs replace the inventory file)")
		}
		hs, err := inventory.HostsFromSpecs(opts.hostsInline, connDefaults())
		if err != nil {
			return err
		}
		if len(hs) == 0 {
			return fmt.Errorf("--hosts resolved to no hosts")
		}
		inv = inventory.FromHosts(hs)
	} else {
		var err error
		inv, err = loadInventories()
		if err != nil {
			return err
		}
	}

	eopts := executor.Options{
		Forks:            config.Current().Forks(),
		Limit:            opts.limit,
		Tags:             opts.tags,
		SkipTags:         opts.skipTags,
		ListHosts:        opts.listHosts,
		StartAtTask:      opts.startAtTask,
		CheckMode:        opts.check,
		DiffMode:         opts.diff,
		TaskTimeout:      config.Current().Run.TaskTimeout,
		WdpVersion:       Version,
		FactCachePath:    opts.factCache,
		MaxDownloadBytes: maxDownloadBytes(),
	}
	var plays []*model.Play

	if chart.IsChartPath(target) {
		ch, values, eng, err := chart.OpenWithLimits(target, opts.valuesFiles, opts.setArgs, chart.Limits{MaxExtractBytes: config.Current().MaxExtractBytes()})
		if err != nil {
			return err
		}
		defer ch.Close()

		// 生命周期相位：选择对应 play；deploy 相位校验 required 配置项
		plays, err = ch.PhasePlays(opts.phase)
		if err != nil {
			return err
		}
		if opts.phase == "" || opts.phase == "deploy" {
			if err := ch.ValidateRequired(values); err != nil {
				return err
			}
			// 可逆性评估 + 不可逆操作确认（可 --yes 跳过；非交互环境警告放行）
			if !opts.listHosts && !opts.check {
				if err := confirmReversibility(ch, opts.yes); err != nil {
					return err
				}
			}
		}
		eopts.Chart = ch
		eopts.Values = values
		eopts.Engine = eng
		eopts.BaseDir = ch.Dir
		eopts.Phase = opts.phase

		// 同名碰撞预检：被 values 遮蔽的 inventory 变量告警（静默失效是真坑），
		// inventory_override 白名单生效的键打信息。--list-hosts 不执行任务，跳过。
		if !opts.listHosts {
			if hosts := inv.SelectPlays(plays, opts.limit); len(hosts) > 0 {
				reportValueCollisions(os.Stderr, eopts.Values, hosts, eopts.Chart.Meta.InventoryOverride)
			}
		}
	} else {
		var err error
		plays, err = playbook.Load(target)
		if err != nil {
			return err
		}
		eopts.BaseDir = filepath.Dir(target)
	}

	// 内联主机模式：play 的 hosts 模式指向命名组，而内联清单只有 all——
	// 全部 play 一律作用于全部内联主机（多组编排请使用 inventory 文件）。
	if inline {
		for _, p := range plays {
			p.Hosts = "all"
		}
	}

	rep, finish := buildReporter()
	conns := conn.NewManagerWithDefaults(connDefaults())
	conns.SetConnectConcurrency(2 * config.Current().Forks())
	ex := executor.New(inv, conns, rep, eopts)

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	failed := ex.Run(ctx, plays)
	conns.CloseAll()
	finish()

	// 部署记录（chart 版本 + values 快照 + 结果统计）。
	// 预演/列主机/只读相位不产生真实部署，不写审计记录（否则与真实部署无法区分）
	if opts.check || opts.listHosts || opts.phase == "status" {
		if failed {
			return errPlayFailed
		}
		return nil
	}
	rec := &release.Record{
		Playbook:  target,
		ValuesRef: append(append([]string{}, opts.valuesFiles...), opts.setArgs...),
		Stats:     ex.LastStats(),
		Failed:    failed,
	}
	if eopts.Chart != nil {
		rec.Chart, rec.Version, rec.Values = eopts.Chart.Meta.Name, eopts.Chart.Meta.Version, eopts.Values
	}
	// 记录实际作用的主机范围：与 executor 一致取全部 play 的并集并应用
	// --limit，避免 --limit web1 时审计记录虚报整个 play 的主机清单
	for _, h := range inv.SelectPlays(plays, eopts.Limit) {
		rec.Hosts = append(rec.Hosts, h.Name)
	}
	if id, err := release.Save(rec); err == nil {
		fmt.Fprintf(os.Stderr, "[release] %s\n", id)
	}

	if failed {
		return errPlayFailed
	}
	return nil
}

// errPlayFailed 标记存在失败（退出码 1，不打印重复错误）。
var errPlayFailed = fmt.Errorf("execution finished with failed hosts")

// confirmReversibility 打印 chart 可逆性评估（着色遵循全局颜色开关，
// 与 buildReporter 同一决策：config 允许 && stderr 为终端且 NO_COLOR 未设）。
func confirmReversibility(ch *chart.Chart, yes bool) error {
	rep := ch.Analyze()
	p := fmtutil.New()
	p.SetWriter(os.Stderr)
	p.SetColor(config.Current().Color() && fmtutil.ColorAuto(os.Stderr))

	p.Print(fmtutil.BoldCyan, "==> chart assessment")
	p.Printf(fmtutil.Bold, " [%s %s]\n", ch.Meta.Name, ch.Meta.Version)
	for _, row := range rep.Rows() {
		p.Printf(fmtutil.None, "    %-20s %s", row.Label, p.Sprint(rowColor(row.Label), fmt.Sprintf("%3d", row.Count)))
		if row.Note != "" {
			p.Printf(fmtutil.None, "  %s", p.Sprint(fmtutil.Dim, "("+row.Note+")"))
		}
		p.Print(fmtutil.None, "\n")
	}
	for _, e := range rep.Examples {
		p.Printf(fmtutil.Yellow, "      - %s\n", e)
	}
	lcClr := fmtutil.None
	switch {
	case rep.HasUninstall && rep.AutoRollback:
		lcClr = fmtutil.Green
	case !rep.HasUninstall && !rep.AutoRollback:
		lcClr = fmtutil.Yellow // 既不可卸载也无自动回滚，提示风险
	}
	p.Printf(fmtutil.None, "    %s %s\n", p.Sprint(fmtutil.Dim, "lifecycle:"), p.Sprint(lcClr, rep.LifecycleNote()))

	if rep.Irreversible == 0 || yes {
		return nil
	}
	if !fmtutil.IsTerminal(os.Stdout) {
		p.Printf(fmtutil.Yellow, "==> warning: %d irreversible operation(s) in a non-interactive environment, continuing (suppress with --yes)\n",
			rep.Irreversible)
		return nil
	}
	p.Printf(fmtutil.None, "==> %s, continue deploying? [Y/n] ",
		p.Sprint(fmtutil.BoldRed, "irreversible operations detected"))
	line, err := readLine(os.Stdin)
	if err != nil {
		return nil // 读失败按默认继续
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "", "y", "yes":
		return nil
	default:
		return fmt.Errorf("deployment cancelled by user (irreversible-operation confirmation declined)")
	}
}

// rowColor 评估分类行的语义色：可逆绿 / 部分可逆黄 / 只读弱化 / 不可逆加粗红。
func rowColor(label string) fmtutil.Color {
	switch label {
	case "reversible":
		return fmtutil.Green
	case "partially reversible":
		return fmtutil.Yellow
	case "read-only":
		return fmtutil.Dim
	default:
		return fmtutil.BoldRed
	}
}

func readLine(r io.Reader) (string, error) {
	buf := make([]byte, 0, 64)
	one := make([]byte, 1)
	for {
		n, err := r.Read(one)
		if n > 0 {
			if one[0] == '\n' {
				return string(buf), nil
			}
			if one[0] != '\r' {
				buf = append(buf, one[0])
			}
		}
		if err != nil {
			if len(buf) > 0 {
				return string(buf), nil
			}
			return "", err
		}
	}
}

// buildReporter 按全局 --output 构造 reporter；json 模式返回最终文档输出函数。
func buildReporter() (report.Reporter, func()) {
	if gOutput == "json" {
		j := report.NewJSONReporter(os.Stdout)
		return j, j.Finish
	}
	level := gVerbosity
	if gQuiet {
		level = -1
	}
	rep := report.NewConsole(os.Stdout, config.Current().Color() && fmtutil.ColorAuto(os.Stdout), level)
	return rep, func() {}
}

// newAdhocCmd 构造 `wdp adhoc`。
func newAdhocCmd() *cobra.Command {
	var (
		mod, argStr, format string
		become, check, diff bool
	)
	cmd := &cobra.Command{
		Use:   "adhoc -m shell -a 'uptime' <host-pattern>",
		Short: "one-off single-module execution",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			pattern := args[0]
			if _, ok := module.Get(mod); !ok {
				return fmt.Errorf("unknown module %q (run `wdp template module` for the list)", mod)
			}
			inv, err := loadInventories()
			if err != nil {
				return err
			}
			free, margs := parseAdhocArgs(argStr)
			if diff && !check {
				check = true // --diff 基于 check 只读对比
			}
			play := &model.Play{
				Hosts: pattern, Become: become,
				Tasks: []*model.Task{{Module: mod, FreeForm: free, Args: margs, Become: &become}},
			}
			rep, finish := buildReporter()
			if format != "" {
				// --format：逐主机模板化输出（脚本管道友好），静默其余呈现
				rep = report.NewFormatter(os.Stdout, format)
				finish = func() {}
			}
			conns := conn.NewManagerWithDefaults(connDefaults())
			conns.SetConnectConcurrency(2 * config.Current().Forks())
			ex := executor.New(inv, conns, rep, executor.Options{
				Forks: config.Current().Forks(), TaskTimeout: config.Current().Run.TaskTimeout,
				CheckMode: check, DiffMode: diff,
				MaxDownloadBytes: maxDownloadBytes(),
			})
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			failed := ex.Run(ctx, []*model.Play{play})
			conns.CloseAll()
			finish()
			if failed {
				return errPlayFailed
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVarP(&mod, "module", "m", "shell", "module name")
	f.StringVarP(&argStr, "args", "a", "", "module args (free-form or k=v list)")
	f.BoolVarP(&become, "become", "b", false, "escalate privileges")
	f.StringVar(&format, "format", "",
		"format per-host output with a Go template, e.g. '{{.host}}: {{.stdout}}' (fields: .stdout/.stderr/.rc/.changed/.failed/.msg)",
	)
	f.BoolVar(&check, "check", false, "check mode: dry-run without applying changes")
	f.BoolVar(&diff, "diff", false, "diff mode: content-level diff with --check")
	return cmd
}

// parseAdhocArgs 解析 adhoc 参数：含 = 的 token 视为 k=v，其余拼接 free-form。
func parseAdhocArgs(s string) (string, map[string]any) {
	args := map[string]any{}
	var free []string
	for tok := range strings.FieldsSeq(s) {
		if k, v, ok := strings.Cut(tok, "="); ok && k != "" {
			args[k] = v
			continue
		}
		free = append(free, tok)
	}
	return strings.Join(free, " "), args
}

// outPrinter 返回绑定命令输出流的着色 printer（颜色遵循 --no-color、
// 终端检测与 NO_COLOR 约定），供列表类命令渲染 fmtutil 表格。
func outPrinter(cmd *cobra.Command) *fmtutil.Printer {
	p := fmtutil.New()
	p.SetWriter(cmd.OutOrStdout())
	if !config.Current().Color() {
		p.SetColor(false)
	}
	// 未显式 --no-color 时保持自动模式（终端检测 + NO_COLOR）
	return p
}
