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
	"wdp/internal/i18n"
	"wdp/internal/inventory"
	"wdp/internal/model"
	"wdp/internal/plan"
	"wdp/internal/release"
)

// applyHelp 返回 `wdp apply` 的长帮助（调用时求值）。
func applyHelp() string {
	return i18n.T(`Execute an approved plan produced by wdp plan

Connection metadata comes from the plan itself (frozen at compile time); no chart directory or inventory
file needs to exist on the executing machine
Reversibility confirmation is evaluated per phase from the chart tree embedded in the plan: Destructive
phases still require confirmation, -y skips it (recommended for CI)
Execution control: --limit narrows hosts, --tags/--skip-tags filter tasks
Dry run: --check is zero-risk read-only probing, --diff a content-level before/after comparison (same semantics as run)
--chart-dir supplies large payload files referenced by the plan (packages artifacts) from local disk; --fact-cache persists facts across runs
Deployment records are written per the phase declarations (review with wdp release show)

Autonomous mode (--autonomous, agent channel only): plan shards are submitted to the target agents,
which converge locally and journal the result; the controller may disconnect at any time (disconnect-tolerant).
Shards are grouped by their via relay root, so the jump-host agent becomes the local controller for that
network segment; agents too old to expose /plan fall back to direct controller execution
--detach returns as soon as the plan is submitted, poll later with wdp apply status; --resume continues from
each agent's journal (tasks already ok/changed are not redone; a changed plan refuses to resume);
--become-password-env passes the sudo password (prefer passwordless sudo)
--rerun forces re-convergence of a finished (done/cancelled) agent run of the same plan: by default an
idempotent hit on the old run is not re-executed (failed runs need no flag to retry); use it to
re-converge drifted hosts

Examples:
wdp apply plan.json -y                        # execute directly from the controller
wdp apply plan.json --check --diff            # dry run against real hosts
wdp apply plan.json --autonomous --detach -y  # submit and return
`, `执行 wdp plan 产出并经批准的执行计划

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
--rerun 对同一计划已完成（done/cancelled）的 agent run 强制重新收敛：默认幂等命中
旧 run 不重跑（已失败 run 无须此标志即可重试），对漂移主机重新收敛时使用

示例：
wdp apply plan.json -y                        # 控制端直接执行
wdp apply plan.json --check --diff            # 连真机预演
wdp apply plan.json --autonomous --detach -y  # 提交即返回
`)
}

// newApplyCmd 构造 `wdp apply`。
func newApplyCmd() *cobra.Command {
	opts := applyOptions{}
	cmd := &cobra.Command{
		Use: "apply <plan.json>",
		Short: i18n.T("execute an approved plan produced by `wdp plan`",
			"执行 wdp plan 产出并经批准的执行计划"),
		Long: applyHelp(),
		Args: cobra.ExactArgs(1),
		// 位置参数是本地 plan 路径，但有子命令（status）时 cobra 默认
		// NoFileComp，本地路径无法补全——回落文件补全，子命令仍可补全
		ValidArgsFunction: completePathArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runApply(cmd.Context(), args[0], opts)
		},
	}
	f := cmd.Flags()
	f.StringVar(&opts.limit, "limit", "", i18n.T(
		"further limit hosts (group/host/!exclude)",
		"进一步收窄主机（组/主机/!排除）"))
	f.StringSliceVarP(&opts.tags, "tags", "t", nil, "run only tasks with these tags (comma-separated)")
	f.StringSliceVar(&opts.skipTags, "skip-tags", nil, "skip tasks with these tags")
	f.BoolVar(&opts.check, "check", false, "check mode: dry-run without applying changes")
	f.BoolVar(&opts.diff, "diff", false, "diff mode: content-level diff with --check")
	f.BoolVar(&opts.failFast, "fail-fast", false, i18n.T(
		"abort remaining batches and plays as soon as any host fails (in-flight hosts finish first)",
		"任一主机失败即中止后续批次与 play（在途主机先跑完）"))
	f.BoolVarP(&opts.yes, "yes", "y", false, "skip confirmation of irreversible operations (recommended for CI)")
	f.StringVar(&opts.factCache, "fact-cache", "", i18n.T(
		"persist setup/set_fact facts to this JSON file across runs",
		"把 setup/set_fact 的 facts 跨运行持久化到此 JSON 文件"))
	f.StringVar(&opts.chartDir, "chart-dir", "", i18n.T(
		"chart directory supplying large payload files referenced by the plan (offline mode)",
		"本地 chart 目录，补齐计划引用的大文件（离线模式）"))
	f.BoolVar(&opts.autonomous, "autonomous", false, i18n.T(
		"submit the plan to target agents for autonomous execution (disconnect-tolerant; agent channel only)",
		"把计划提交给目标 agent 自治执行（断连容忍；仅 agent 通道）"))
	f.BoolVar(&opts.detach, "detach", false, i18n.T(
		"with --autonomous: return as soon as agents accept the plan (poll later with the 'apply status' subcommand)",
		"配合 --autonomous：agent 接受计划即返回（事后用 apply status 回查）"))
	f.BoolVar(&opts.resume, "resume", false, i18n.T(
		"with --autonomous: resume from each agent's journal (skips tasks already ok/changed)",
		"配合 --autonomous：从各 agent 的 journal 断点续跑（已 ok/changed 的任务不重做）"))
	f.StringVar(&opts.becomePasswordEnv, "become-password-env", "", i18n.T(
		"env var holding the sudo password delivered with autonomous plans (prefer passwordless sudo)",
		"携带 sudo 密码的环境变量名，随自治计划下发（优先免密 sudo）"))
	f.BoolVar(&opts.rerun, "rerun", false, i18n.T(
		"with --autonomous: re-submit a finished (done/cancelled) run of the same plan; failed runs are always retriable",
		"配合 --autonomous：对同一计划已完成（done/cancelled）的 run 强制重新提交；已失败的 run 无须此标志即可重试"))

	cmd.AddCommand(newApplyStatusCmd())
	return cmd
}

// applyStatusHelp 返回 `wdp apply status` 的长帮助（调用时求值）。
func applyStatusHelp() string {
	return i18n.T(`Poll or review the progress of an autonomous (--autonomous) execution

Reads the tail of each agent's journal and streams a summary of that run's task states; after a
disconnect it recomputes to the same conclusion as the agent's local journal
<run-id-prefix> supports prefix matching; --limit filters agents further

Examples:
wdp apply status plan.json a1b2c3
`, `轮询/回看自治执行（--autonomous）的进度

读取各 agent 的 journal 尾部，流式汇总该 run 的任务状态；断连恢复后重算，
结论与 agent 本地 journal 一致
<run-id-prefix> 支持前缀匹配；--limit 进一步筛选 agent

示例：
wdp apply status plan.json a1b2c3
`)
}

// newApplyStatusCmd 构造 `wdp apply status`：轮询/回看自治执行进度。
func newApplyStatusCmd() *cobra.Command {
	opts := applyOptions{}
	cmd := &cobra.Command{
		Use: "status <plan.json> <run-id-prefix>",
		Short: i18n.T("poll autonomous execution progress of a run (journal tail)",
			"轮询/回看某次自治执行的进度（journal 尾部）"),
		Long: applyStatusHelp(),
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runApplyStatus(cmd.Context(), args[0], args[1], opts)
		},
	}
	cmd.Flags().StringVar(&opts.limit, "limit", "", i18n.T(
		"further limit agents (group/host/!exclude)",
		"进一步筛选 agent（组/主机/!排除）"))
	return cmd
}

type applyOptions struct {
	limit     string
	tags      []string
	skipTags  []string
	check     bool
	diff      bool
	failFast  bool
	yes       bool
	factCache string
	chartDir  string

	autonomous        bool
	detach            bool
	resume            bool
	rerun             bool
	becomePasswordEnv string
}

// runApply 执行计划。
func runApply(ctx context.Context, path string, opts applyOptions) error {
	opts.check = normalizeDiffFlag(opts.diff, opts.check)
	if err := validateApplyFlags(opts); err != nil {
		return err
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
	inv := inventory.FromHosts(planHosts(p))
	allowed, err := applyLimitSet(inv, opts.limit)
	if err != nil {
		return err
	}

	// 自治执行：分片提交给目标 agent（异步），本控制端只做进度聚合
	if opts.autonomous {
		failed, aerr := runApplyAutonomous(ctx, p, opts, allowed)
		if aerr != nil {
			return aerr
		}
		if failed {
			return errPlayFailed
		}
		return nil
	}

	ex, failed, werr := executePlanDirect(ctx, inv, p, opts)
	return recordPlanRelease(path, p, spec, ex, failed, werr, opts.check)
}

// validateApplyFlags 校验自治专用 flag 的上下文。
// 自治专用 flag 脱离 --autonomous 时此前被静默丢弃（漏写 --autonomous 的
// 用户会以为 sudo 密码已下发、以为已异步返回）：显式报错而不是假装生效。
func validateApplyFlags(opts applyOptions) error {
	if opts.autonomous {
		return nil
	}
	if opts.detach {
		return fmt.Errorf("--detach requires --autonomous (it controls when autonomous submission returns)")
	}
	if opts.resume {
		return fmt.Errorf("--resume requires --autonomous (it resumes from each agent's journal)")
	}
	if opts.becomePasswordEnv != "" {
		return fmt.Errorf("--become-password-env requires --autonomous (the direct path takes the password from the inventory (become_password / become_password_env))")
	}
	if opts.rerun {
		return fmt.Errorf("--rerun requires --autonomous (it overrides the agent-side idempotency of plan submissions)")
	}
	return nil
}

// applyLimitSet 把 --limit 收窄为允许的主机集（nil = 全部）；命中不到
// 任何计划主机时报错。
func applyLimitSet(inv *inventory.Inventory, limit string) (map[string]bool, error) {
	var allowed map[string]bool // --limit 允许的主机集（nil = 全部）
	if limit == "" {
		return allowed, nil
	}
	limited, lerr := inv.Select(limit)
	if lerr != nil {
		return nil, lerr
	}
	if len(limited) == 0 {
		return nil, fmt.Errorf("--limit %s matched no plan hosts", limit)
	}
	allowed = make(map[string]bool, len(limited))
	for _, h := range limited {
		allowed[h.Name] = true
	}
	return allowed, nil
}

// executePlanDirect 组装报告器/连接池/执行器并直连执行计划（含信号处理
// 与执行收尾）。返回执行器供部署记录读取统计。
func executePlanDirect(ctx context.Context, inv *inventory.Inventory, p *plan.Plan, opts applyOptions) (*executor.Executor, bool, error) {
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
		FailFast:         opts.failFast,
		TaskTimeout:      config.Current().Run.TaskTimeout,
		WdpVersion:       Version,
		FactCachePath:    opts.factCache,
		PayloadDir:       opts.chartDir,
		MaxDownloadBytes: maxDownloadBytes(),
		MaxUploadBytes:   maxUploadBytes(),
	})

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	failed := ex.RunPlan(ctx, p)
	conns.CloseAll()
	werr := finish()
	return ex, failed, werr
}

// recordPlanRelease 落部署记录（chart 版本 + values 快照 + 结果统计）。
// 预演与不留痕相位（未声明 release/record 的相位）不产生真实部署，不写
// 审计记录（否则与真实部署无法区分）
func recordPlanRelease(path string, p *plan.Plan, spec chart.PhaseSpec, ex *executor.Executor, failed bool, werr error, check bool) error {
	if check || !spec.Records() {
		return finishPlay(werr, failed)
	}
	rec := &release.Record{
		Playbook: path,
		Phase:    p.Phase,
		Chart:    p.Chart,
		Version:  p.Version,
		// 与 marker 同一脱敏口径：sensitive_values 不因落在控制端就明文落盘
		Values: chart.RedactValues(p.Meta.SensitiveValues, p.Values),
		Stats:  ex.LastStats(),
		Failed: failed,
		Hosts:  p.Host(),
	}
	return saveReleaseRecord(rec, werr, failed)
}

// planHosts 把计划主机清单还原为连接模型（连接元数据编译期固化在计划
// 里，取各主机首个分片；Host() 已按出现序去重，多 play 同名主机共用
// 首片的连接）。apply 直连执行、自治回退执行与 apply status 的 --limit
// 收窄三处共用同一还原口径。
func planHosts(p *plan.Plan) []*model.Host {
	names := p.Host()
	hosts := make([]*model.Host, 0, len(names))
	for _, name := range names {
		hosts = append(hosts, p.HostPlansOf(name)[0].Conn.Host(name))
	}
	return hosts
}

// planPhaseSpec 从计划快照的 chart.yaml 声明合成相位属性（与
// chart.PhaseSpecFor 共用 chart.MergePhaseSpec，规则不漂移）。
func planPhaseSpec(p *plan.Plan) chart.PhaseSpec {
	spec := chart.DefaultPhaseSpec(p.Phase)
	if declared, ok := p.Meta.Phases[p.Phase]; ok {
		// 计划快照的镜像 PhaseSpec → chart.PhaseSpec（合并规则单一实现在
		// chart.MergePhaseSpec，镜像只承载数据）
		spec = chart.MergePhaseSpec(spec, chart.PhaseSpec{
			Release:      declared.Release,
			Record:       declared.Record,
			ClearsMarker: declared.ClearsMarker,
			ValuesFrom:   chart.ValuesFrom(declared.ValuesFrom),
		})
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
