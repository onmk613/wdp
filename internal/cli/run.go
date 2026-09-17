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
	"wdp/internal/render"
)

const runHelp = `
执行 playbook 或 chart（编译 + 执行一条龙；拆开用 wdp plan + wdp apply）

目标可为裸 playbook（site.yaml）、chart 目录或打包的 chart.tgz
chart 按生命周期相位执行：--phase 选择 chart 根部的 <phase>.yaml（内置 deploy/uninstall/status，可自定义）
主机来源：-i inventory 清单（可重复，后者覆盖前者合并）或 --hosts 内联表达式
（IP/主机[:port]，支持 10.55.2.101-104 区间；每个 play 都以这些主机为目标），两者互斥
连接通道由 inventory 的 conn 决定（ssh/push/agent/local），默认取 wdp.cfg [run].conn

执行控制：--limit 在 play 目标范围内进一步过滤；--tags/--skip-tags 按标签筛选任务；
--start-at-task 从指定任务起跑（断点续跑）；--list-hosts 只列出将执行的主机即退出
预演：--check 零风险只读探测 + 变更预估；--diff 在 check 基础上输出内容级前后对照
-f/--values-file 与 --set 覆盖 chart 默认值（依序深合并，同 Helm）；--fact-cache 跨运行持久化 facts
不可逆相位（Destructive）执行前要求确认，-y 跳过（CI 推荐）
按相位声明落部署记录（wdp release show 回看）与 release marker（wdp drift 巡检）

示例：
wdp run site.yaml -i inv.yaml                      # 裸 playbook
wdp run ./myapp -f envs/prod.yaml                  # chart + 环境 values
wdp run ./myapp --phase uninstall --check          # 卸载预演
wdp run ./myapp --start-at-task "下发配置"          # 从指定任务续跑
`

// newRunCmd 构造 `wdp run`。
func newRunCmd() *cobra.Command {
	opts := runOptions{}
	cmd := &cobra.Command{
		Use:   "run <playbook.yaml|chart-dir|chart.tgz>",
		Short: "run a playbook or chart",
		Long:  runHelp,
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
	f.StringVar(&opts.phase, "phase", "deploy",
		"chart lifecycle phase: any <phase>.yaml at the chart root (built-in: deploy/uninstall/status; custom e.g. update/stop/download)")
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
	inv, inline, err := hostSource(opts.hostsInline)
	if err != nil {
		return err
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
	// spec 决定相位语义（是否部署事件/是否留部署记录）；无 chart 上下文
	// （裸 playbook）按内置缺省判定
	spec := chart.DefaultPhaseSpec(opts.phase)

	if chart.IsChartPath(target) {
		// 相位属性可能来自 chart.yaml phases: 声明（自定义相位声明 release），
		// 它决定 values 来源与校验门控——先加载 chart 本体再分支
		ch, loaded, lerr := loadChartRun(ctx, target, inv, opts, &eopts)
		if lerr != nil {
			return lerr
		}
		defer ch.Close()
		plays, spec = loaded.plays, loaded.spec
	} else {
		plays, err = playbook.Load(target)
		if err != nil {
			return err
		}
		if abs, aerr := filepath.Abs(target); aerr == nil {
			target = abs
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

	// 零主机即失败：--limit 命中不到任何 play 的目标主机时，此前会跑完
	// 空 RECAP 并退出 0，CI 把"什么都没做"当成部署成功。与 apply 的
	// "--limit matched no plan hosts" 同口径。
	if hosts := inv.SelectPlays(plays, eopts.Limit); len(hosts) == 0 {
		return noHostsError(opts.limit, target)
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
	// 预演/列主机/不留痕相位（未声明 release/record 的相位，如 status 与普通
	// 自定义相位）不产生真实部署，不写审计记录（否则与真实部署无法区分）
	if opts.check || opts.listHosts || !spec.Records() {
		if failed {
			return errPlayFailed
		}
		return nil
	}
	phaseLabel := opts.phase
	if phaseLabel == "" {
		phaseLabel = "deploy"
	}
	rec := &release.Record{
		Playbook:  target,
		Phase:     phaseLabel,
		ValuesRef: append(append([]string{}, opts.valuesFiles...), opts.setArgs...),
		Stats:     ex.LastStats(),
		Failed:    failed,
	}
	if eopts.Chart != nil {
		rec.Chart, rec.Version, rec.Values = eopts.Chart.Meta.Name, eopts.Chart.Meta.Version, eopts.Values
		// marker 来源相位的 values 按主机还原（eopts.Values 为 nil）：审计
		// 记录取代表性 values（各主机通常一致，逐主机差异在 marker 里）
		if rec.Values == nil {
			for _, v := range eopts.HostValues {
				rec.Values = v
				break
			}
		}
		// 审计记录与 marker 同一脱敏口径：chart 声明的 sensitive_values
		// 不因"落在控制端"就明文持久化（wdp release show --values 可读）
		rec.Values = eopts.Chart.RedactValues(rec.Values)
	}
	// 记录实际作用的主机范围：与 executor 一致取全部 play 的并集并应用
	// --limit，避免 --limit web1 时审计记录虚报整个 play 的主机清单
	for _, h := range inv.SelectPlays(plays, eopts.Limit) {
		rec.Hosts = append(rec.Hosts, h.Name)
	}
	// 审计记录写失败此前被完全吞掉：磁盘满/权限不足时"部署成功但无记录"
	// 无声发生，回看与回滚依据随之缺失——至少给一条告警。
	if id, err := release.Save(rec); err == nil {
		fmt.Fprintf(os.Stderr, "[release] %s\n", id)
	} else {
		fmt.Fprintf(os.Stderr, "warning: failed to write the deployment record: %v\n", err)
	}

	if failed {
		return errPlayFailed
	}
	return nil
}

// chartRun 是 loadChartRun 的装载结果。
type chartRun struct {
	plays []*model.Play
	spec  chart.PhaseSpec
}

// noHostsError 是"零主机"的失败口径：--limit 命中不到目标主机，或 play 的
// hosts 模式匹配不到任何清单主机。此前这两种情况都会静默跑完并退出 0。
func noHostsError(limit, target string) error {
	if limit != "" {
		return fmt.Errorf("--limit %q matched no hosts of any play in %s (check the pattern against the inventory)", limit, target)
	}
	return fmt.Errorf("no hosts selected for %s (the plays match no inventory host)", target)
}

// loadChartRun 装载 chart 执行目标：生命周期相位 plays、相位属性、values
// 与渲染引擎，并完成可逆性确认与同名碰撞预检；eopts 的 chart 上下文
// （Chart/Values/Engine/BaseDir/Phase/HostValues）在此填充。返回的 chart
// 由调用方在执行完毕后 Close。
func loadChartRun(ctx context.Context, target string, inv *inventory.Inventory, opts runOptions, eopts *executor.Options) (_ *chart.Chart, out chartRun, err error) {
	ch, err := chart.LoadWithLimits(target, chart.Limits{MaxExtractBytes: config.Current().MaxExtractBytes()})
	if err != nil {
		return nil, out, err
	}
	defer func() {
		if err != nil {
			ch.Close()
		}
	}()

	// 生命周期相位：选择对应 play；相位属性按 chart.yaml phases: 声明合并
	plays, err := ch.PhasePlays(opts.phase)
	if err != nil {
		return nil, out, err
	}
	// 零主机即失败（在可逆性确认与 marker 读取之前）：--limit 命中不到目标
	// 主机时不该先弹确认再报错，也不该去读一台都没有的 marker。
	if hosts := inv.SelectPlays(plays, opts.limit); len(hosts) == 0 {
		return nil, out, noHostsError(opts.limit, target)
	}
	spec := ch.PhaseSpecFor(opts.phase)
	eng, err := render.NewEngine(ch.CollectHelpers())
	if err != nil {
		return nil, out, err
	}

	var values map[string]any
	switch spec.EffectiveValuesFrom() {
	case chart.ValuesFromChart:
		// chart 默认 values + -f + --set。部署事件相位（deploy 及声明
		// release 的自定义相位如 update）必须过 required + schema 校验；
		// 普通相位（status/download/自定义）只取默认值，不强制校验——
		// 它们不写 marker 也不删除数据。
		values, err = ch.BuildValues(opts.valuesFiles, opts.setArgs)
		if err != nil {
			return nil, out, err
		}
		if spec.Release {
			if err := ch.ValidateRequired(values); err != nil {
				return nil, out, err
			}
			// schema 校验（required 的强化版：类型/取值/结构），
			// 子 chart 用 SubScope 逐层走查（引用 vars 由 executor 展开期校验）
			if err := ch.ValidateValuesSchema(values); err != nil {
				return nil, out, err
			}
			if err := ch.ValidateSubchartsSchema(values); err != nil {
				return nil, out, err
			}
		}
	case chart.ValuesFromMarker:
		// 卸载类相位：values 从各主机 marker 还原实际部署入参，-f/--set
		// 降级为显式覆盖；marker 缺失/v1 报错，绝不静默回退 values.yaml
		// 默认值。校验强度按 Destructive（清除 marker 的相位用 values 拼
		// 删除路径，与部署同门控）。
		if opts.listHosts {
			break
		}
		hosts := inv.SelectPlays(plays, opts.limit)
		hostValues, rerr := resolveMarkerValues(ctx, ch, hosts, opts.valuesFiles, opts.setArgs, spec.Destructive())
		if rerr != nil {
			return nil, out, rerr
		}
		eopts.HostValues = hostValues
		for _, v := range hostValues { // 碰撞预检的代表性 values（各主机通常一致）
			values = v
			break
		}
	}
	// 可逆性确认按 Destructive 门控（deploy/uninstall 及声明对应属性的
	// 相位）：uninstall 此前因 Release == false 完全跳过确认——唯一会
	// 删除数据的相位恰是唯一不确认的相位
	if !opts.listHosts && !opts.check && spec.Destructive() {
		if err := confirmReversibility(ch, opts.phase, opts.yes); err != nil {
			return nil, out, err
		}
	}
	eopts.Chart = ch
	eopts.Values = values
	eopts.Engine = eng
	eopts.BaseDir = ch.Dir
	eopts.Phase = opts.phase

	// 同名碰撞预检：被 values 遮蔽的 inventory 变量告警（静默失效是真坑），
	// inventory_override 白名单生效的键打信息。--list-hosts 不执行任务，跳过。
	if !opts.listHosts && values != nil {
		if hosts := inv.SelectPlays(plays, opts.limit); len(hosts) > 0 {
			reportValueCollisions(os.Stderr, values, hosts, eopts.Chart.Meta.InventoryOverride)
		}
	}
	return ch, chartRun{plays: plays, spec: spec}, nil
}
