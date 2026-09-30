package cli

// `wdp plan`：把 chart + inventory + values 编译为完全解析的执行计划
//（部署相位完全离线，不连接任何主机；非部署相位的 values 来自主机
// marker，需要读取）。plan 是一等产物：可评审（附在工单上）、可 diff
//（任务级差异）、可执行（`wdp apply`）。计划的查看与对比（show/diff）
// 在 planview.go。
//
//	wdp plan ./chart -i inv.yaml -f prod.yaml -o plan.json
//	wdp plan show plan.json [--host 10.20.0.11]
//	wdp plan diff plan-a.json plan-b.json

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"wdp/internal/chart"
	"wdp/internal/config"
	"wdp/internal/i18n"
	"wdp/internal/planbuild"
)

// planHelp 返回 `wdp plan` 的长帮助（调用时求值）。
func planHelp() string {
	return i18n.T(`Compile chart + inventory + values into a fully-resolved execution plan (an offline artifact)

The deploy phase (and any phase declaring release) compiles without connecting to any host; non-deploy
phases take their values from each host's marker, so compilation must connect to read them
Cross-host information (groups/hosts/hostvars) is frozen into literals; when/loop/template rendering stays
on the execution side (it may depend on runtime registers)
Deterministic: the same chart + the same values compile to a byte-identical plan.json; the PlanID is the
content-addressed sha256 — a tampered file is refused at load time
The plan embeds a chart tree snapshot (self-contained: apply does not need the chart/inventory on site);
large files are only referenced by sha256 (supplied locally via --chart-dir at apply time, or distributed
by URL through the artifact module)

Subcommands: show inspects per-host values and task lists; diff compares two plans task-by-task and value-by-value
Common flags: -o writes plan.json (default stdout); --phase picks the phase; --limit narrows hosts;
--fact-cache freezes facts into the plan; -f/--set overrides values

Examples:
wdp plan ./myapp -f envs/prod.yaml -o plan.json     # compile (offline)
wdp plan show plan.json --host web1                 # what will happen on this machine
wdp plan diff plan-a.json plan-b.json               # task-level + values-level differences
`, `把 chart + inventory + values 编译为完全解析的执行计划（离线产物）

deploy 相位（及声明 release 的相位）编译全程不连接任何主机；非部署相位的 values
来自各主机 marker，编译期需连接读取
跨主机信息（groups/hosts/hostvars）固化为字面值；when/loop/模板渲染留给执行侧（可能依赖运行时 register）
确定性：同一 chart + 同一 values 两次编译产出逐字节相同的 plan.json，PlanID 为内容
寻址 sha256——文件被篡改后拒绝加载
plan 内嵌 chart 树快照（自包含，apply 不依赖现场有 chart/inventory）；大文件只记录
sha256 引用（apply 时 --chart-dir 本地补齐，或由 artifact 模块按 URL 分发）

子命令：show 查看逐主机 values 与任务清单；diff 对比两份计划的任务级/值级差异
常用 flag：-o 写出 plan.json（默认 stdout）；--phase 选择相位；--limit 收窄主机；
--fact-cache 冻结 facts 进计划；-f/--set 覆盖 values

示例：
wdp plan ./myapp -f envs/prod.yaml -o plan.json     # 编译（离线）
wdp plan show plan.json --host web1                 # 这台机器会发生什么
wdp plan diff plan-a.json plan-b.json               # 任务级 + values 级差异
`)
}

// newPlanCmd 构造 `wdp plan`。
func newPlanCmd() *cobra.Command {
	// flag 绑定局部变量（与 run/apply 同写法）：包级可变全局会在命令
	// 重复构造/测试间残留旧值
	var out string
	var opts planCompileOptions
	cmd := &cobra.Command{
		Use: "plan <chart-dir|chart.tgz>",
		Short: i18n.T("compile a fully-resolved execution plan (offline) for review and `wdp apply`",
			"把 chart + inventory + values 编译为完全解析的执行计划（离线产物，供评审与 wdp apply 执行）"),
		Long: planHelp(),
		Args: cobra.ExactArgs(1),
		// 位置参数是本地 chart 路径，但有子命令（show/diff）时 cobra
		// 默认 NoFileComp，本地路径无法补全——回落文件补全，子命令仍可补全
		ValidArgsFunction: completePathArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPlanCompile(cmd.Context(), args[0], out, opts)
		},
	}
	f := cmd.Flags()
	f.StringVarP(&out, "output-file", "o", "", i18n.T(
		"write the plan to this file (default: stdout)",
		"把计划写入此文件（默认输出到 stdout）"))
	f.StringVar(&opts.limit, "limit", "", i18n.T(
		"further limit hosts (group/host/!exclude)",
		"进一步收窄主机（组/主机/!排除）"))
	f.StringVar(&opts.phase, "phase", "deploy", i18n.T(
		"chart lifecycle phase",
		"chart 生命周期相位"))
	f.StringVar(&opts.factCache, "fact-cache", "", i18n.T(
		"freeze facts from this JSON cache into the plan",
		"从此 JSON 缓存把 facts 冻结进计划"))
	chartValueFlags(cmd, &opts.valuesFiles, &opts.setArgs)

	cmd.AddCommand(newPlanShowCmd())
	cmd.AddCommand(newPlanDiffCmd())
	return cmd
}

// planCompileOptions 是 `wdp plan` 的编译参数（flag 的落点）。
type planCompileOptions struct {
	limit       string
	phase       string
	factCache   string
	valuesFiles []string
	setArgs     []string
}

// runPlanCompile 编译计划。部署相位（deploy 及声明 release 的相位）完全
// 离线；非部署相位的 values 从各主机 marker 还原（与 run 同语义）。
func runPlanCompile(ctx context.Context, target, out string, opts planCompileOptions) error {
	inv, err := loadInventories()
	if err != nil {
		return err
	}
	limits := chart.Limits{MaxExtractBytes: config.Current().MaxExtractBytes()}
	copts := planbuild.CompileOptions{
		Phase:      opts.phase,
		Limit:      opts.limit,
		WdpVersion: Version,
		FactCache:  opts.factCache,
		Limits:     limits,
	}
	// chart 只加载一次并复用给编译：marker 相位需先加载 chart 解析相位
	// 属性与目标主机（读 marker），此前 probe 与 plan.Compile 内部各加载
	// 一遍——tgz 形态的 LoadWithLimits 含解包，等于同一制品解包两次
	probe, err := chart.LoadWithLimits(target, limits)
	if err != nil {
		return err
	}
	defer probe.Close()
	spec := probe.PhaseSpecFor(opts.phase)
	if spec.EffectiveValuesFrom() == chart.ValuesFromMarker {
		plays, perr := probe.PhasePlays(opts.phase)
		if perr != nil {
			return perr
		}
		hosts := inv.SelectPlays(plays, opts.limit)
		if len(hosts) == 0 {
			return noHostsError(opts.limit, target)
		}
		hostValues, rerr := resolveMarkerValues(ctx, probe, hosts,
			opts.valuesFiles, opts.setArgs, spec.Destructive())
		if rerr != nil {
			return rerr
		}
		copts.HostValues = hostValues
	}

	p, err := planbuild.CompileChart(probe, inv, opts.valuesFiles, opts.setArgs, copts)
	if err != nil {
		return err
	}
	if out != "" {
		if err := p.Write(out); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "[plan] %s written (plan_id=%s, %d host plan(s), %d file(s))\n",
			out, p.PlanID, len(p.Hosts), len(p.Files))
		return nil
	}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(data))
	return nil
}
