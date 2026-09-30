package chart

// 模块集提取（分层方案 P2）：应用版本创建时把「每个相位会用到哪些
// 内置模块」随制品落库，执行受理期与目标主机 agent 上报的模块集对账
// （agent /info）——缺模块在受理期报错，而不是执行到一半才失败。
//
// 只收集注册表内的内置模块名：chart 引用由执行器展开为子任务（在
// 递归中单独统计）；chart 本地脚本模块（modules/<名> 可执行文件）由
// chart 分发后经 shell 执行，均不需要 agent 侧能力。

import (
	"slices"

	"wdp/internal/model"
	"wdp/internal/module"
)

// ModuleNames 返回全部相位各自会执行到的内置模块名（去重、按相位分组；
// 相位 = PhaseNames() 的全集）。子 chart 引用沿实际任务树展开，环引用
// 按 MaxChartDepth 拦截（与 reversibility.Analyze 同口径）。
func (c *Chart) ModuleNames() map[string][]string {
	out := map[string][]string{}
	for _, phase := range c.PhaseNames() {
		out[phase] = c.moduleNamesFor(phase)
	}
	return out
}

// moduleNamesFor 收集单个相位的模块名（nil 错误容忍：坏相位由 lint/
// 执行侧报错，此处按空集处理）。
func (c *Chart) moduleNamesFor(phase string) []string {
	seen := map[string]bool{}
	type refKey struct {
		ch    *Chart
		phase string
	}
	normEntry := func(e string) string {
		if e == "" {
			return "deploy"
		}
		return e
	}
	visited := map[refKey]bool{{c, normEntry(phase)}: true}
	var walkPlays func(ch *Chart, plays []*model.Play, depth int)
	var walkTask func(ch *Chart, t *model.Task, depth int)
	walkTask = func(ch *Chart, t *model.Task, depth int) {
		if t.Block != nil {
			for _, sub := range append(append([]*model.Task{}, t.Block...), append(t.Rescue, t.Always...)...) {
				walkTask(ch, sub, depth)
			}
			return
		}
		if t.ChartRef != "" {
			sub, err := ch.ResolveSub(t.ChartRef)
			if err != nil {
				return // 引用错误由 lint/执行侧报
			}
			key := refKey{sub, normEntry(t.TasksFrom)}
			if visited[key] {
				return
			}
			visited[key] = true
			if subPlay, err := sub.EntryPlay(t.TasksFrom); err == nil {
				walkPlays(sub, []*model.Play{subPlay}, depth+1)
			}
			return
		}
		if _, ok := module.Get(t.Module); ok {
			seen[t.Module] = true
		}
	}
	walkPlays = func(ch *Chart, plays []*model.Play, depth int) {
		if depth > MaxChartDepth {
			return
		}
		for _, p := range plays {
			for _, t := range append(append([]*model.Task{}, p.Tasks...), p.Handlers...) {
				walkTask(ch, t, depth)
			}
		}
	}
	plays, err := c.PhasePlays(phase)
	if err != nil {
		return nil
	}
	walkPlays(c, plays, 0)
	out := make([]string, 0, len(seen))
	for m := range seen {
		out = append(out, m)
	}
	slices.Sort(out)
	return out
}
