package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"wdp/internal/chart"
	"wdp/internal/config"
	"wdp/internal/conn"
	"wdp/internal/executor"
	"wdp/internal/inventory"
	"wdp/internal/model"
	"wdp/internal/playbook"
	"wdp/internal/release"

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
