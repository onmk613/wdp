package cli

// `wdp apply`：执行已批准的执行计划（`wdp plan` 的产物）。连接元数据来自
// 计划本身（编译期固化），不依赖 chart 目录或 inventory 文件在现场存在。
//
//	wdp plan ./chart -i inv.yaml -o plan.json   # 编译（离线）
//	# … 评审 plan.json …
//	wdp apply plan.json                          # 执行

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"wdp/internal/chart"
	"wdp/internal/config"
	"wdp/internal/conn"
	"wdp/internal/executor"
	"wdp/internal/inventory"
	"wdp/internal/model"
	"wdp/internal/plan"
	"wdp/internal/release"
)

const applyHelp = `
执行 wdp plan 产出并经批准的执行计划

连接元数据来自计划本身（编译期固化），执行现场不需要 chart 目录或 inventory 文件存在
可逆性确认从计划内嵌的 chart 树按相位评估：Destructive 相位仍需确认，-y 跳过（CI 推荐）
执行控制：--limit 收窄主机、--tags/--skip-tags 筛任务
预演：--check 零风险只读探测、--diff 内容级前后对照（语义与 run 相同）
--chart-dir 本地补齐计划引用的大文件（packages 制品）；--fact-cache 跨运行持久化 facts
按相位声明落部署记录（wdp release show 回看）

自治模式（--autonomous，仅 agent 通道）：计划分片提交给目标 agent，本地收敛并落
journal，控制端可随时断开（断连容忍）；按 via 中继根分组提交，跳板机 agent 即该
网段本地控制端；旧版 agent 无 /plan 端点时自动回退控制端直接执行
--detach 提交即返回，事后 wdp apply status 回查；--resume 从 journal 断点续跑
（已 ok/changed 的任务不重做；计划变更则拒绝续跑）；--become-password-env 传递 sudo 密码
（优先免密 sudo）

示例：
wdp apply plan.json -y                        # 控制端直接执行
wdp apply plan.json --check --diff            # 连真机预演
wdp apply plan.json --autonomous --detach -y  # 提交即返回
`

// newApplyCmd 构造 `wdp apply`。
func newApplyCmd() *cobra.Command {
	opts := applyOptions{}
	cmd := &cobra.Command{
		Use:   "apply <plan.json>",
		Short: "execute an approved plan produced by `wdp plan`",
		Long:  applyHelp,
		Args:  cobra.ExactArgs(1),
		// 位置参数是本地 plan 路径，但有子命令（status）时 cobra 默认
		// NoFileComp，本地路径无法补全——回落文件补全，子命令仍可补全
		ValidArgsFunction: completePathArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runApply(cmd.Context(), args[0], opts)
		},
	}
	f := cmd.Flags()
	f.StringVar(&opts.limit, "limit", "", "further limit hosts (group/host/!exclude)")
	f.StringSliceVarP(&opts.tags, "tags", "t", nil, "run only tasks with these tags (comma-separated)")
	f.StringSliceVar(&opts.skipTags, "skip-tags", nil, "skip tasks with these tags")
	f.BoolVar(&opts.check, "check", false, "check mode: dry-run without applying changes")
	f.BoolVar(&opts.diff, "diff", false, "diff mode: content-level diff with --check")
	f.BoolVarP(&opts.yes, "yes", "y", false, "skip confirmation of irreversible operations (recommended for CI)")
	f.StringVar(&opts.factCache, "fact-cache", "",
		"persist setup/set_fact facts to this JSON file across runs")
	f.StringVar(&opts.chartDir, "chart-dir", "",
		"chart directory supplying large payload files referenced by the plan (offline mode)")
	f.BoolVar(&opts.autonomous, "autonomous", false,
		"submit the plan to target agents for autonomous execution (disconnect-tolerant; agent channel only)")
	f.BoolVar(&opts.detach, "detach", false,
		"with --autonomous: return as soon as agents accept the plan (poll later with `apply status`)")
	f.BoolVar(&opts.resume, "resume", false,
		"with --autonomous: resume from each agent's journal (skips tasks already ok/changed)")
	f.StringVar(&opts.becomePasswordEnv, "become-password-env", "",
		"env var holding the sudo password delivered with autonomous plans (prefer passwordless sudo)")

	cmd.AddCommand(newApplyStatusCmd())
	return cmd
}

const applyStatusHelp = `
轮询/回看自治执行（--autonomous）的进度

读取各 agent 的 journal 尾部，流式汇总该 run 的任务状态；断连恢复后重算，
结论与 agent 本地 journal 一致
<run-id-prefix> 支持前缀匹配；--limit 进一步筛选 agent

示例：
wdp apply status plan.json a1b2c3
`

// newApplyStatusCmd 构造 `wdp apply status`：轮询/回看自治执行进度。
func newApplyStatusCmd() *cobra.Command {
	opts := applyOptions{}
	cmd := &cobra.Command{
		Use:   "status <plan.json> <run-id-prefix>",
		Short: "poll autonomous execution progress of a run (journal tail)",
		Long:  applyStatusHelp,
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runApplyStatus(cmd.Context(), args[0], args[1], opts)
		},
	}
	cmd.Flags().StringVar(&opts.limit, "limit", "", "further limit agents (group/host/!exclude)")
	return cmd
}

type applyOptions struct {
	limit     string
	tags      []string
	skipTags  []string
	check     bool
	diff      bool
	yes       bool
	factCache string
	chartDir  string

	autonomous        bool
	detach            bool
	resume            bool
	becomePasswordEnv string
}

// runApply 执行计划。
func runApply(ctx context.Context, path string, opts applyOptions) error {
	if opts.diff && !opts.check {
		opts.check = true
	}
	if cfgTimeout := config.Current().Run.Timeout; cfgTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(cfgTimeout)*time.Second)
		defer cancel()
	}
	p, err := plan.Load(path)
	if err != nil {
		return err
	}
	spec := planPhaseSpec(p)

	// 可逆性确认：从计划内嵌的 chart 树按相位评估（与 run 同门控；Destructive
	// 相位必经确认）。--check 不落地不需要。
	if !opts.check && spec.Destructive() {
		if cerr := confirmPlanReversibility(p, opts.yes); cerr != nil {
			return cerr
		}
	}

	// 合成 inventory（连接元数据来自计划；RunPlan 会整体接管执行器状态）
	var hosts []*model.Host
	seen := map[string]bool{}
	for _, name := range p.Host() {
		if seen[name] {
			continue
		}
		seen[name] = true
		hp := p.HostPlansOf(name)[0]
		hosts = append(hosts, hp.Conn.Host(name))
	}
	inv := inventory.FromHosts(hosts)
	if opts.limit != "" {
		limited, lerr := inv.Select(opts.limit)
		if lerr != nil {
			return lerr
		}
		if len(limited) == 0 {
			return fmt.Errorf("--limit %s matched no plan hosts", opts.limit)
		}
	}

	// 自治执行：分片提交给目标 agent（异步），本控制端只做进度聚合
	if opts.autonomous {
		failed, aerr := runApplyAutonomous(ctx, p, opts)
		if aerr != nil {
			return aerr
		}
		if failed {
			return errPlayFailed
		}
		return nil
	}

	rep, finish := buildReporter()
	conns := conn.NewManagerWithDefaults(connDefaults())
	conns.SetConnectConcurrency(2 * config.Current().Forks())
	ex := executor.New(inv, conns, rep, executor.Options{
		Forks:            config.Current().Forks(),
		Limit:            opts.limit,
		Tags:             opts.tags,
		SkipTags:         opts.skipTags,
		CheckMode:        opts.check,
		DiffMode:         opts.diff,
		TaskTimeout:      config.Current().Run.TaskTimeout,
		WdpVersion:       Version,
		FactCachePath:    opts.factCache,
		PayloadDir:       opts.chartDir,
		MaxDownloadBytes: maxDownloadBytes(),
	})

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	failed := ex.RunPlan(ctx, p)
	conns.CloseAll()
	finish()

	if opts.check || !spec.Records() {
		if failed {
			return errPlayFailed
		}
		return nil
	}
	rec := &release.Record{
		Playbook: path,
		Phase:    p.Phase,
		Chart:    p.Chart,
		Version:  p.Version,
		Values:   p.Values,
		Stats:    ex.LastStats(),
		Failed:   failed,
		Hosts:    p.Host(),
	}
	if id, serr := release.Save(rec); serr == nil {
		fmt.Fprintf(os.Stderr, "[release] %s\n", id)
	}
	if failed {
		return errPlayFailed
	}
	return nil
}

// planPhaseSpec 从计划快照的 chart.yaml 声明合成相位属性（与
// chart.PhaseSpecFor 同合并规则）。
func planPhaseSpec(p *plan.Plan) chart.PhaseSpec {
	spec := chart.DefaultPhaseSpec(p.Phase)
	if declared, ok := p.Meta.Phases[p.Phase]; ok {
		spec.Release = spec.Release || declared.Release
		spec.Record = spec.Record || declared.Record || declared.Release
		spec.ClearsMarker = spec.ClearsMarker || declared.ClearsMarker
	}
	return spec
}

// confirmPlanReversibility 用计划内嵌的 chart 树做相位可逆性确认（计划与
// chart 在同一相位上的任务树一致——编译期由同一 play 清单产出）。
func confirmPlanReversibility(p *plan.Plan, yes bool) error {
	tmp, err := os.MkdirTemp("", "wdp-plan-confirm-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	root, err := p.Materialize(tmp)
	if err != nil {
		return err
	}
	ch, err := chart.LoadWithLimits(root, chart.Limits{})
	if err != nil {
		return fmt.Errorf("plan chart tree invalid: %w", err)
	}
	defer ch.Close()
	return confirmReversibility(ch, p.Phase, yes)
}
