package playbook

// 裸 playbook 的 chart 引用预扫描：`wdp run` 在启动期从 playbook 同级
// charts/ 目录加载被引用 chart（缺引用/坏 chart 立即失败，而不是首个主机
// 执行到该任务时才报错），并聚合各 chart 的 _helpers.tpl 构建渲染引擎。

import (
	"wdp/internal/model"
)

// CollectChartRefs 收集 plays 中全部 chart 引用名（含 block/rescue/always
// 嵌套与 handler），去重且顺序稳定（同名引用只留首个出现位置）。
func CollectChartRefs(plays []*model.Play) []string {
	seen := map[string]bool{}
	var out []string
	add := func(t *model.Task) {
		if t.ChartRef != "" && !seen[t.ChartRef] {
			seen[t.ChartRef] = true
			out = append(out, t.ChartRef)
		}
	}
	var walk func(tasks []*model.Task)
	walk = func(tasks []*model.Task) {
		for _, t := range tasks {
			add(t)
			if t.Module == "block" {
				walk(t.Block)
				walk(t.Rescue)
				walk(t.Always)
			}
		}
	}
	for _, p := range plays {
		walk(p.Tasks)
		walk(p.Handlers)
	}
	return out
}
