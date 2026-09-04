package cli

// `wdp plan show` / `wdp plan diff`：已编译计划（plan.json）的本地查看与
// 对比——不连主机、不改现场，服务于评审环节（编译在 plan.go）。

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"wdp/internal/drift"
	"wdp/internal/plan"
)

const planShowHelp = `
查看已编译计划的内容

打印计划概要（chart/相位/PlanID/主机数/文件数）与全局 values，再逐主机列出
每个 (play, host) 分片的生效 values 与任务清单；任务按 pre/tasks/post/handler
分组，block/rescue/always 缩进呈现并标注回滚能力
--host 只看某台主机的分片（多 play 相位下同一主机会有多条）

示例：
wdp plan show plan.json
wdp plan show plan.json --host web1
`

// newPlanShowCmd 构造 `wdp plan show`。
func newPlanShowCmd() *cobra.Command {
	var host string
	cmd := &cobra.Command{
		Use:   "show <plan.json>",
		Short: "inspect a compiled plan (per-host values and task lists)",
		Long:  planShowHelp,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := plan.Load(args[0])
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "plan %s  chart %s %s  phase %s\n", shortID(p.PlanID), p.Chart, p.Version, p.Phase)
			fmt.Fprintf(out, "wdp %s  %d host plan(s)  %d file(s)\n\n", p.WdpVersion, len(p.Hosts), len(p.Files))
			if vb, err := json.Marshal(p.Values); err == nil {
				fmt.Fprintf(out, "values: %s\n", string(vb))
			}
			for _, hp := range p.Hosts {
				if host != "" && hp.Host != host {
					continue
				}
				name := hp.Play.Name
				if name == "" {
					name = hp.Play.Hosts
				}
				fmt.Fprintf(out, "\nhost %s  (play #%d %s)\n", hp.Host, hp.PlayIdx, name)
				if len(hp.Values) > 0 {
					vb, _ := json.Marshal(hp.Values)
					fmt.Fprintf(out, "  values: %s\n", string(vb))
				}
				groups := []struct {
					label string
					tasks []*plan.ResolvedTask
				}{
					{"pre", hp.Pre}, {"tasks", hp.Tasks}, {"post", hp.Post},
				}
				for _, g := range groups {
					for _, t := range g.tasks {
						printPlanTask(out, t, "  "+g.label+" ", "")
					}
				}
				for _, t := range hp.Handlers {
					printPlanTask(out, t, "  handler ", "")
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&host, "host", "", "only show this host's plan shard")
	return cmd
}

// printPlanTask 打印计划任务行（block/rescue/always 缩进递归）。
func printPlanTask(out io.Writer, t *plan.ResolvedTask, prefix, indent string) {
	fmt.Fprintf(out, "%s%s#%d %s (%s", indent, prefix, t.Idx, t.Label, t.Module)
	if t.Rollback == "full" || t.Rollback == "partial" {
		fmt.Fprintf(out, ", rollback:%s", t.Rollback)
	}
	fmt.Fprintln(out, ")")
	for _, b := range t.Block {
		printPlanTask(out, b, prefix, indent+"    ")
	}
	for _, b := range t.Rescue {
		printPlanTask(out, b, prefix, indent+"    ")
	}
	for _, b := range t.Always {
		printPlanTask(out, b, prefix, indent+"    ")
	}
}

const planDiffHelp = `
对比两份执行计划，回答"这次变更与上次相比会多做/少做什么"

差异维度：chart 名/版本、相位、全局 values（字段级）、主机集合增删、
逐主机任务清单增删（pre/tasks/post/handler 全计入）
任务按 idx+label+module 对齐，顺序变化不算差异——评审关心"会执行什么"，不是展示顺序
完全等价时输出 plans are equivalent

示例：
wdp plan diff plan-old.json plan-new.json
`

// newPlanDiffCmd 构造 `wdp plan diff`。
func newPlanDiffCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "diff <plan-a.json> <plan-b.json>",
		Short: "task-level and values-level diff of two plans",
		Long:  planDiffHelp,
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := plan.Load(args[0])
			if err != nil {
				return err
			}
			b, err := plan.Load(args[1])
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			diffs := 0
			if a.Chart != b.Chart || a.Version != b.Version {
				fmt.Fprintf(out, "chart: %s %s → %s %s\n", a.Chart, a.Version, b.Chart, b.Version)
				diffs++
			}
			if a.Phase != b.Phase {
				fmt.Fprintf(out, "phase: %s → %s\n", a.Phase, b.Phase)
				diffs++
			}
			if vd := drift.ValuesDiff(a.Values, b.Values); len(vd) > 0 {
				fmt.Fprintf(out, "values:\n")
				for _, d := range vd {
					fmt.Fprintf(out, "  %s\n", d)
				}
				diffs += len(vd)
			}
			// 主机集合差异
			hostsA, hostsB := hostSet(a), hostSet(b)
			for _, h := range sortedKeysDiff(hostsA, hostsB) {
				fmt.Fprintf(out, "host %s: only in %s\n", h, args[0])
				diffs++
			}
			for _, h := range sortedKeysDiff(hostsB, hostsA) {
				fmt.Fprintf(out, "host %s: only in %s\n", h, args[1])
				diffs++
			}
			// 逐主机任务差异（按 idx+label+module 对比；顺序变化不算差异，
			// 增删才算——评审关心的是"会执行什么"，不是展示顺序）
			common := intersection(hostsA, hostsB)
			sort.Strings(common)
			for _, h := range common {
				ta, tb := taskLines(a, h), taskLines(b, h)
				if strings.Join(ta, "\n") == strings.Join(tb, "\n") {
					continue
				}
				fmt.Fprintf(out, "host %s tasks:\n", h)
				setA, setB := map[string]bool{}, map[string]bool{}
				for _, l := range ta {
					setA[l] = true
				}
				for _, l := range tb {
					setB[l] = true
				}
				for _, l := range ta {
					if !setB[l] {
						fmt.Fprintf(out, "  - %s\n", l)
						diffs++
					}
				}
				for _, l := range tb {
					if !setA[l] {
						fmt.Fprintf(out, "  + %s\n", l)
						diffs++
					}
				}
			}
			if diffs == 0 {
				fmt.Fprintln(out, "plans are equivalent (task lists and values identical)")
			}
			return nil
		},
	}
}

// hostSet 计划的主机名集合。
func hostSet(p *plan.Plan) map[string]bool {
	out := map[string]bool{}
	for _, h := range p.Host() {
		out[h] = true
	}
	return out
}

// sortedKeysDiff 在 a 中出现而 b 中未出现的键（排序）。
func sortedKeysDiff(a, b map[string]bool) []string {
	var out []string
	for k := range a {
		if !b[k] {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// intersection 两个集合的交集。
func intersection(a, b map[string]bool) []string {
	var out []string
	for k := range a {
		if b[k] {
			out = append(out, k)
		}
	}
	return out
}

// taskLines 展开一台主机的全部任务行（pre/tasks/post/handler 与
// block/rescue/always 递归，携带组前缀），供 diff 按行比对。
func taskLines(p *plan.Plan, host string) []string {
	var out []string
	var walk func(ts []*plan.ResolvedTask, tag string)
	walk = func(ts []*plan.ResolvedTask, tag string) {
		for _, t := range ts {
			out = append(out, fmt.Sprintf("%s#%d %s (%s)", tag, t.Idx, t.Label, t.Module))
			walk(t.Block, tag+"block.")
			walk(t.Rescue, tag+"rescue.")
			walk(t.Always, tag+"always.")
		}
	}
	for _, hp := range p.HostPlansOf(host) {
		walk(hp.Pre, "pre.")
		walk(hp.Tasks, "")
		walk(hp.Post, "post.")
		walk(hp.Handlers, "handler.")
	}
	return out
}
