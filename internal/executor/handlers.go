package executor

import (
	"slices"

	"wdp/internal/chart"
	"wdp/internal/model"
)

// mergeSubHandlers 把子 chart 的 handlers 并入父 play（重名告警并忽略）。
func (e *Executor) mergeSubHandlers(p *model.Play) {
	if e.Opts.Chart == nil {
		return
	}
	seen := map[string]bool{}
	for _, h := range p.Handlers {
		seen[h.Name] = true
	}
	var add func(sub *chart.Chart)
	add = func(sub *chart.Chart) {
		// 全相位（deploy + uninstall/status/自定义）：tasks_from 引用的入口
		// 相位里声明的 handler 同样要能被 notify 触发
		for _, subPlay := range sub.AllPlays() {
			for _, h := range subPlay.Handlers {
				if seen[h.Name] {
					e.Rep.PlayMsg("handler %q duplicated, ignoring the same-named handler from subchart %s", h.Name, sub.Meta.Name)
					continue
				}
				seen[h.Name] = true
				p.Handlers = append(p.Handlers, h)
			}
		}
		for _, s := range sub.Subs {
			add(s)
		}
	}
	for _, sub := range e.Opts.Chart.Subs {
		add(sub)
	}
}

// collectNotified 汇总 handler 通知：返回（有序通知列表，主机 → 通知集合）。
// 每台主机独立记录，handler 派发按主机过滤（不全局扇出）。
func collectNotified(runs []*hostRun, handlers []*model.Task) ([]string, map[string]map[string]bool) {
	byHost := map[string]map[string]bool{}
	set := map[string]bool{}
	for _, hr := range runs {
		if !hr.alive {
			continue
		}
		m := map[string]bool{}
		for n := range hr.notified {
			m[n] = true
			set[n] = true
		}
		byHost[hr.host.Name] = m
	}
	var out []string
	for _, h := range handlers {
		if set[h.Name] {
			out = append(out, h.Name)
		}
	}
	slices.Sort(out)
	return out, byHost
}
