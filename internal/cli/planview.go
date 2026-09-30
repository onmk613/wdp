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
	"wdp/internal/i18n"
	"wdp/internal/plan"
)

// planShowHelp 返回 `wdp plan show` 的长帮助（调用时求值）。
func planShowHelp() string {
	return i18n.T(`Inspect the content of a compiled plan

Prints the plan summary (chart/phase/PlanID/host count/file count) and the global values, then lists the
effective values and task list of every (play, host) shard; tasks are grouped as pre/tasks/post/handler,
with block/rescue/always indented and rollback capability annotated
--host shows only one host's shards (a host has several under a multi-play phase)

Examples:
wdp plan show plan.json
wdp plan show plan.json --host web1
`, `查看已编译计划的内容

打印计划概要（chart/相位/PlanID/主机数/文件数）与全局 values，再逐主机列出
每个 (play, host) 分片的生效 values 与任务清单；任务按 pre/tasks/post/handler
分组，block/rescue/always 缩进呈现并标注回滚能力
--host 只看某台主机的分片（多 play 相位下同一主机会有多条）

示例：
wdp plan show plan.json
wdp plan show plan.json --host web1
`)
}

// newPlanShowCmd 构造 `wdp plan show`。
func newPlanShowCmd() *cobra.Command {
	var host string
	cmd := &cobra.Command{
		Use: "show <plan.json>",
		Short: i18n.T("inspect a compiled plan (per-host values and task lists)",
			"查看已编译计划的内容（逐主机 values 与任务清单）"),
		Long: planShowHelp(),
		Args: cobra.ExactArgs(1),
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

// printPlanTask 打印计划任务行（block/rescue/always 缩进递归）。chart 引用
// 附带 hosts 过滤与 values_from 覆盖——它们改变实际执行范围，评审必须可见。
func printPlanTask(out io.Writer, t *plan.ResolvedTask, prefix, indent string) {
	fmt.Fprintf(out, "%s%s#%d %s (%s", indent, prefix, t.Idx, t.Label, t.Module)
	if t.Rollback == "full" || t.Rollback == "partial" {
		fmt.Fprintf(out, ", rollback:%s", t.Rollback)
	}
	if t.ChartRef != "" {
		if t.ChartHosts != "" {
			fmt.Fprintf(out, ", hosts:%s", t.ChartHosts)
		}
		if n := len(t.ChartValuesFrom); n > 0 {
			fmt.Fprintf(out, ", values_from:%s", strings.Join(t.ChartValuesFrom, ","))
		}
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

// planDiffHelp 返回 `wdp plan diff` 的长帮助（调用时求值）。
func planDiffHelp() string {
	return i18n.T(`Compare two execution plans and answer "what will this change do more or less than last time"

Diff dimensions: chart name/version, phase, global values (field-level), host set additions/removals,
per-host task list additions/removals (pre/tasks/post/handler all included)
Tasks are aligned by label+module (+ group prefix); reordering is not a difference — review cares about
"what will run", not display order; idx is display-only and never used for alignment: a single insertion
shifts the idx of every later task, so aligning by idx would mark all unchanged tasks as differences
(drowning real differences in cascading noise)
Prints plans are equivalent when the two are fully equal

Examples:
wdp plan diff plan-old.json plan-new.json
`, `对比两份执行计划，回答"这次变更与上次相比会多做/少做什么"

差异维度：chart 名/版本、相位、全局 values（字段级）、主机集合增删、
逐主机任务清单增删（pre/tasks/post/handler 全计入）
任务按 label+module（+组前缀）对齐，顺序变化不算差异——评审关心"会执行什么"，不是展示顺序；
idx 只用于展示不参与对齐：清单中单点插入会平移后续任务的 idx，按 idx 对齐会把
未变更任务整体判为差异（级联噪声淹没有效差异）
完全等价时输出 plans are equivalent

示例：
wdp plan diff plan-old.json plan-new.json
`)
}

// newPlanDiffCmd 构造 `wdp plan diff`。
func newPlanDiffCmd() *cobra.Command {
	return &cobra.Command{
		Use: "diff <plan-a.json> <plan-b.json>",
		Short: i18n.T("task-level and values-level diff of two plans",
			"对比两份计划的任务级与 values 级差异"),
		Long: planDiffHelp(),
		Args: cobra.ExactArgs(2),
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
			// 逐主机任务差异（按 label+module 对齐；顺序变化不算差异，
			// 增删才算——评审关心的是"会执行什么"，不是展示顺序）
			common := intersection(hostsA, hostsB)
			sort.Strings(common)
			for _, h := range common {
				ta, tb := taskLines(a, h), taskLines(b, h)
				equal := len(ta) == len(tb)
				for i := 0; equal && i < len(ta); i++ {
					equal = ta[i].key == tb[i].key
				}
				if equal {
					continue
				}
				fmt.Fprintf(out, "host %s tasks:\n", h)
				// 多重集比对（按 key 计数配对）：同名同模块任务可重复
				// 出现，纯集合会丢"删了一份重复任务"这类差异
				cntA, cntB := map[string]int{}, map[string]int{}
				for _, l := range ta {
					cntA[l.key]++
				}
				for _, l := range tb {
					cntB[l.key]++
				}
				for _, l := range ta {
					if cntB[l.key] == 0 {
						fmt.Fprintf(out, "  - %s\n", l.display)
						diffs++
						continue
					}
					cntB[l.key]--
				}
				for _, l := range tb {
					if cntA[l.key] == 0 {
						fmt.Fprintf(out, "  + %s\n", l.display)
						diffs++
						continue
					}
					cntA[l.key]--
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

// taskLine 是 diff 的一个任务行：key 是对齐身份（组前缀 + label +
// module + 影响执行范围的 chart hosts/values_from），display 是展示文本
// （额外含 idx）。idx 不进身份：单点插入平移后续任务的 idx 时，按 idx
// 对齐会把未变更任务整体判成差异。
type taskLine struct {
	key     string
	display string
}

// taskLines 展开一台主机的全部任务行（pre/tasks/post/handler 与
// block/rescue/always 递归，携带组前缀），供 diff 按行比对。
func taskLines(p *plan.Plan, host string) []taskLine {
	var out []taskLine
	var walk func(ts []*plan.ResolvedTask, tag string)
	walk = func(ts []*plan.ResolvedTask, tag string) {
		for _, t := range ts {
			suffix := ""
			if t.ChartHosts != "" {
				suffix += fmt.Sprintf(" hosts:%s", t.ChartHosts)
			}
			if len(t.ChartValuesFrom) > 0 {
				suffix += fmt.Sprintf(" values_from:%s", strings.Join(t.ChartValuesFrom, ","))
			}
			out = append(out, taskLine{
				key:     fmt.Sprintf("%s%s (%s)%s", tag, t.Label, t.Module, suffix),
				display: fmt.Sprintf("%s#%d %s (%s)%s", tag, t.Idx, t.Label, t.Module, suffix),
			})
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
