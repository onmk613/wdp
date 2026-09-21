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
	"wdp/internal/plan"
)

const planHelp = `
把 chart + inventory + values 编译为完全解析的执行计划（离线产物）

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
`

// newPlanCmd 构造 `wdp plan`。
func newPlanCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "plan <chart-dir|chart.tgz>",
		Short: "compile a fully-resolved execution plan (offline) for review and `wdp apply`",
		Long:  planHelp,
		Args:  cobra.ExactArgs(1),
		// 位置参数是本地 chart 路径，但有子命令（show/diff）时 cobra
		// 默认 NoFileComp，本地路径无法补全——回落文件补全，子命令仍可补全
		ValidArgsFunction: completePathArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPlanCompile(cmd.Context(), args[0])
		},
	}
	f := cmd.Flags()
	f.StringVarP(&planOut, "output-file", "o", "",
		"write the plan to this file (default: stdout)")
	f.StringVar(&planOpts.limit, "limit", "", "further limit hosts (group/host/!exclude)")
	f.StringVar(&planOpts.phase, "phase", "deploy", "chart lifecycle phase")
	f.StringVar(&planOpts.factCache, "fact-cache", "", "freeze facts from this JSON cache into the plan")
	chartValueFlags(cmd, &planOpts.valuesFiles, &planOpts.setArgs)

	cmd.AddCommand(newPlanShowCmd())
	cmd.AddCommand(newPlanDiffCmd())
	return cmd
}

var (
	planOut  string
	planOpts struct {
		limit       string
		phase       string
		factCache   string
		valuesFiles []string
		setArgs     []string
	}
)

// runPlanCompile 编译计划。部署相位（deploy 及声明 release 的相位）完全
// 离线；非部署相位的 values 从各主机 marker 还原（与 run 同语义）。
func runPlanCompile(ctx context.Context, target string) error {
	inv, err := loadInventories()
	if err != nil {
		return err
	}
	limits := chart.Limits{MaxExtractBytes: config.Current().MaxExtractBytes()}
	opts := plan.CompileOptions{
		Phase:      planOpts.phase,
		Limit:      planOpts.limit,
		WdpVersion: Version,
		FactCache:  planOpts.factCache,
		Limits:     limits,
	}
	// 从 marker 取 values 的相位（uninstall 等）：编译期先行解析（需连接主机）
	probe, err := chart.LoadWithLimits(target, limits)
	if err != nil {
		return err
	}
	spec := probe.PhaseSpecFor(planOpts.phase)
	if spec.EffectiveValuesFrom() == chart.ValuesFromMarker {
		plays, perr := probe.PhasePlays(planOpts.phase)
		if perr != nil {
			probe.Close()
			return perr
		}
		hosts := inv.SelectPlays(plays, planOpts.limit)
		if len(hosts) == 0 {
			probe.Close()
			return noHostsError(planOpts.limit, target)
		}
		if cerr := func() error {
			defer probe.Close()
			hostValues, rerr := resolveMarkerValues(ctx, probe, hosts,
				planOpts.valuesFiles, planOpts.setArgs, spec.Destructive())
			if rerr != nil {
				return rerr
			}
			opts.HostValues = hostValues
			return nil
		}(); cerr != nil {
			return cerr
		}
	} else {
		probe.Close()
	}

	p, err := plan.Compile(target, inv, planOpts.valuesFiles, planOpts.setArgs, opts)
	if err != nil {
		return err
	}
	if planOut != "" {
		if err := p.Write(planOut); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "[plan] %s written (plan_id=%s, %d host plan(s), %d file(s))\n",
			planOut, p.PlanID, len(p.Hosts), len(p.Files))
		return nil
	}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(data))
	return nil
}
