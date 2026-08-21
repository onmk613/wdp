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
	"wdp/internal/connection"
	"wdp/internal/executor"
	"wdp/internal/fmtutil"
	"wdp/internal/i18n"
	"wdp/internal/inventory"
	"wdp/internal/model"
	"wdp/internal/module"
	"wdp/internal/playbook"
	"wdp/internal/release"
	"wdp/internal/report"
)

// loadInventories 加载全部 -i 清单（未指定时用内置默认 inventory.yaml）。
func loadInventories() (*inventory.Inventory, error) {
	paths := gInventories
	if len(paths) == 0 {
		paths = []string{"inventory.yaml"}
	}
	return inventory.LoadMerge(paths)
}

// chartValueFlags 声明 chart 公共 flag（-f/--values/--set）。
func chartValueFlags(cmd *cobra.Command, valuesFiles, setArgs *[]string) {
	cmd.Flags().StringArrayVarP(valuesFiles, "values-file", "f", nil,
		i18n.T("chart values override files (repeatable, deep-merged in order, like Helm)",
			"chart values 覆盖文件（可多次，按序深合并，同 Helm）"))
	cmd.Flags().StringArrayVar(setArgs, "set", nil,
		i18n.T("chart values dot-path overrides (--set a.b[0]=v, repeatable)",
			"chart values 点路径覆盖（--set a.b[0]=v，可多次）"))
}

// newRunCmd 构造 `wdp run`。
func newRunCmd() *cobra.Command {
	var (
		limit, tags, skipTags, startAt string
		listHosts, check, diff, yes    bool
		phase                          string
		valuesFiles, setArgs           []string
	)
	cmd := &cobra.Command{
		Use:   "run <playbook.yaml|chart目录|chart.tgz>",
		Short: i18n.T("run a playbook or chart", "执行 playbook 或 chart"),
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runTarget(cmd.Context(), args[0], runOptions{
				limit:       limit,
				tags:        splitCSV(tags),
				skipTags:    splitCSV(skipTags),
				listHosts:   listHosts,
				startAtTask: startAt,
				check:       check,
				diff:        diff,
				phase:       phase,
				yes:         yes,
				valuesFiles: valuesFiles,
				setArgs:     setArgs,
			})
		},
	}
	f := cmd.Flags()
	f.StringVar(&limit, "limit", "", i18n.T("further limit hosts (group/host/!exclude)", "进一步限制主机（组/主机/!排除）"))
	f.StringVarP(&tags, "tags", "t", "", i18n.T("run only tasks with these tags (comma-separated)", "仅执行带这些 tag 的任务（逗号分隔）"))
	f.StringVar(&skipTags, "skip-tags", "", i18n.T("skip tasks with these tags", "跳过带这些 tag 的任务"))
	f.BoolVar(&listHosts, "list-hosts", false, i18n.T("list hosts that would run, then exit", "仅列出将执行的主机"))
	f.StringVar(&startAt, "start-at-task", "", i18n.T("start execution at the given task", "从指定任务开始执行"))
	f.BoolVar(&check, "check", false, i18n.T("check mode: dry-run without applying changes", "check 模式：预演变更不实际执行"))
	f.BoolVar(&diff, "diff", false, i18n.T("diff mode: content-level diff with --check (copy/template/file)", "diff 模式：配合 --check 输出内容级差异（copy/template/file）"))
	f.StringVar(&phase, "phase", "deploy", i18n.T("chart lifecycle phase: deploy | uninstall | status", "chart 生命周期相位：deploy | uninstall | status"))
	f.BoolVarP(&yes, "yes", "y", false, i18n.T("skip confirmation of irreversible operations (recommended for CI)", "跳过不可逆操作确认提示（CI 建议）"))
	chartValueFlags(cmd, &valuesFiles, &setArgs)
	return cmd
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
	valuesFiles []string
	setArgs     []string
}

// runTarget 加载目标（chart 或裸 playbook）并执行，返回错误由 cobra 呈现。
func runTarget(ctx context.Context, target string, opts runOptions) error {
	if opts.diff && !opts.check {
		opts.check = true // --diff 基于 check 只读对比，自动启用预演
	}
	inv, err := loadInventories()
	if err != nil {
		return err
	}

	eopts := executor.Options{
		Forks:       gForks,
		Limit:       opts.limit,
		Tags:        opts.tags,
		SkipTags:    opts.skipTags,
		ListHosts:   opts.listHosts,
		StartAtTask: opts.startAtTask,
		CheckMode:   opts.check,
		DiffMode:    opts.diff,
		TaskTimeout: gTaskTimeout,
		WdpVersion:  Version,
	}
	var plays []*model.Play

	if chart.IsChartPath(target) {
		ch, values, eng, err := chart.Open(target, opts.valuesFiles, opts.setArgs)
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
	} else {
		plays, err = playbook.Load(target)
		if err != nil {
			return err
		}
		eopts.BaseDir = filepath.Dir(target)
	}

	rep, finish := buildReporter()
	conns := connection.NewManager()
	conns.SetConnectConcurrency(2 * gForks)
	ex := executor.New(inv, conns, rep, eopts)

	if gTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(gTimeout)*time.Second)
		defer cancel()
	}
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	failed := ex.Run(ctx, plays)
	conns.CloseAll()
	finish()

	// 部署记录（chart 版本 + values 快照 + 结果统计）
	rec := &release.Record{
		Playbook:  target,
		ValuesRef: append(append([]string{}, opts.valuesFiles...), opts.setArgs...),
		Stats:     ex.LastStats(),
		Failed:    failed,
	}
	if eopts.Chart != nil {
		rec.Chart, rec.Version, rec.Values = eopts.Chart.Meta.Name, eopts.Chart.Meta.Version, eopts.Values
	}
	if hosts, err := inv.Select(eoptsHosts(plays)); err == nil {
		for _, h := range hosts {
			rec.Hosts = append(rec.Hosts, h.Name)
		}
	}
	if id, err := release.Save(rec); err == nil {
		fmt.Fprintf(os.Stderr, "[release] %s\n", id)
	}

	if failed {
		return errPlayFailed
	}
	return nil
}

// eoptsHosts 提取 play 的 hosts 模式（部署记录用，取第一个 play）。
func eoptsHosts(plays []*model.Play) string {
	if len(plays) == 0 {
		return ""
	}
	return plays[0].Hosts
}

// errPlayFailed 标记存在失败（退出码 1，不打印重复错误）。
var errPlayFailed = fmt.Errorf("执行完成，存在失败主机")

// confirmReversibility 打印 chart 可逆性评估；存在不可逆操作时交互确认。
// 行为：--yes 直接放行；TTY 下逐次询问（回车=继续）；非交互环境打印警告后放行（CI 不应挂起）。
func confirmReversibility(ch *chart.Chart, yes bool) error {
	rep := ch.Analyze()
	fmt.Fprintf(os.Stderr, "==> 应用包评估 [%s %s]: %s\n", ch.Meta.Name, ch.Meta.Version, rep.Summary())
	if rep.Irreversible == 0 || yes {
		return nil
	}
	if !fmtutil.IsTerminal(os.Stdout) {
		fmt.Fprintf(os.Stderr, "==> 警告: 含 %d 个不可逆操作且当前非交互环境，继续执行（--yes 可抑制本警告）\n",
			rep.Irreversible)
		return nil
	}
	fmt.Fprintf(os.Stderr, "==> 存在不可逆操作，继续部署? [Y/n] ")
	line, err := readLine(os.Stdin)
	if err != nil {
		return nil // 读失败按默认继续
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "", "y", "yes":
		return nil
	default:
		return fmt.Errorf("用户取消部署（不可逆操作确认被拒绝）")
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
	rep := report.NewConsole(os.Stdout, !gNoColor && fmtutil.ColorAuto(os.Stdout), level)
	return rep, func() {}
}

// newAdhocCmd 构造 `wdp adhoc`。
func newAdhocCmd() *cobra.Command {
	var (
		mod, argStr, format string
		become, check, diff bool
	)
	cmd := &cobra.Command{
		Use:   "adhoc -m shell -a 'uptime' <主机模式>",
		Short: i18n.T("one-off single-module execution", "单模块临时执行"),
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			pattern := args[0]
			if _, ok := module.Get(mod); !ok {
				return fmt.Errorf("未知模块 %q（wdp modules 查看列表）", mod)
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
			conns := connection.NewManager()
			conns.SetConnectConcurrency(2 * gForks)
			ex := executor.New(inv, conns, rep, executor.Options{
				Forks: gForks, TaskTimeout: gTaskTimeout,
				CheckMode: check, DiffMode: diff,
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
	f.StringVarP(&mod, "module", "m", "shell", i18n.T("module name", "模块名"))
	f.StringVarP(&argStr, "args", "a", "", i18n.T("module args (free-form or k=v list)", "模块参数（free-form 或 k=v 列表）"))
	f.BoolVarP(&become, "become", "b", false, i18n.T("escalate privileges", "提权执行"))
	f.StringVar(&format, "format", "",
		i18n.T("format per-host output with a Go template, e.g. '{{.host}}: {{.stdout}}' (fields: .stdout/.stderr/.rc/.changed/.failed/.msg)",
			"按 Go 模板格式化每主机输出（如 '{{.host}}: {{.stdout}}'；可用 .stdout/.stderr/.rc/.changed/.failed/.msg）"))
	f.BoolVar(&check, "check", false, i18n.T("check mode: dry-run without applying changes", "check 模式：预演变更不实际执行"))
	f.BoolVar(&diff, "diff", false, i18n.T("diff mode: content-level diff with --check", "diff 模式：配合 --check 输出内容级差异"))
	return cmd
}

// parseAdhocArgs 解析 adhoc 参数：含 = 的 token 视为 k=v，其余拼接 free-form。
func parseAdhocArgs(s string) (string, map[string]any) {
	args := map[string]any{}
	var free []string
	for _, tok := range strings.Fields(s) {
		if k, v, ok := strings.Cut(tok, "="); ok && k != "" {
			args[k] = v
			continue
		}
		free = append(free, tok)
	}
	return strings.Join(free, " "), args
}

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	for _, t := range strings.Split(s, ",") {
		if t = strings.TrimSpace(t); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// outPrinter 返回绑定命令输出流的着色 printer（颜色遵循 --no-color、
// 终端检测与 NO_COLOR 约定），供列表类命令渲染 fmtutil 表格。
func outPrinter(cmd *cobra.Command) *fmtutil.Printer {
	p := fmtutil.New()
	p.SetWriter(cmd.OutOrStdout())
	if gNoColor {
		p.SetColor(false)
	}
	// 未显式 --no-color 时保持自动模式（终端检测 + NO_COLOR）
	return p
}
