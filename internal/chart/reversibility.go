package chart

import (
	"slices"

	"wdp/internal/model"
	"wdp/internal/module"
)

// Reversibility 是应用包的可逆性评估（部署前提示与确认的依据）。
type Reversibility struct {
	Reversible   int      // 全量可回滚任务数（copy/template/file：快照恢复 + 可 absent 卸载）
	Partial      int      // 部分可回滚任务数（unarchive：仅删除新建目录，覆盖已有文件不恢复）
	ReadOnly     int      // 只读任务数（setup）
	Irreversible int      // 不可逆任务数（shell/script/package/service：无法自动回滚）
	Examples     []string // 不可逆任务示例（标签，最多 5 个）
	HasUninstall bool     // 提供 uninstall.yaml
	HasStatus    bool     // 提供 status.yaml
	AutoRollback bool     // 任一 play 配置 strategy.auto_rollback
}

// Analyze 评估 chart 部署任务的可逆性（含子 chart 递归）。
func (c *Chart) Analyze() *Reversibility {
	r := &Reversibility{
		HasUninstall: len(c.Uninstall) > 0,
		HasStatus:    len(c.Status) > 0,
	}
	var walk func(ch *Chart, prefix string)
	walk = func(ch *Chart, prefix string) {
		for _, p := range ch.Deploy {
			if p.Strategy != nil && p.Strategy.AutoRollback {
				r.AutoRollback = true
			}
			for _, t := range append(append([]*model.Task{}, p.Tasks...), p.Handlers...) {
				r.classify(prefix, t)
			}
		}
		for _, sub := range ch.Subs {
			walk(sub, prefix+sub.Meta.Name+".")
		}
	}
	walk(c, "")
	slices.Sort(r.Examples)
	if len(r.Examples) > 5 {
		r.Examples = r.Examples[:5]
	}
	return r
}

// classify 归类单个任务（block 组递归展开，chart 引用按可逆性最保守估计）。
func (r *Reversibility) classify(prefix string, t *model.Task) {
	if t.Block != nil {
		for _, sub := range append(append([]*model.Task{}, t.Block...), append(t.Rescue, t.Always...)...) {
			r.classify(prefix, sub)
		}
		return
	}
	if t.ChartRef != "" {
		// chart 引用按不可逆计（保守估计，子任务已在递归中单独统计）
		return
	}
	label := prefix + t.Label()
	// 可逆性由模块自声明的 RollbackProvider/ReadOnlyProvider 能力决定
	// （未声明能力的模块保守视为不可逆）
	switch {
	case module.IsReadOnlyModule(t.Module):
		r.ReadOnly++
	case module.RollbackCapabilityOf(t.Module) == module.RollbackFull:
		r.Reversible++
	case module.RollbackCapabilityOf(t.Module) == module.RollbackPartial:
		r.Partial++
	default:
		r.Irreversible++
		r.Examples = append(r.Examples, label+" ("+t.Module+")")
	}
}

// SummaryRow 是评估摘要的一行分类计数（呈现层排版与着色用）。
type SummaryRow struct {
	Label string // 分类标签
	Count int    // 任务数
	Note  string // 可选补充说明
}

// Rows 返回分类计数行（partial 为 0 时省略该行）。
func (r *Reversibility) Rows() []SummaryRow {
	rows := []SummaryRow{
		{Label: "reversible", Count: r.Reversible, Note: "copy/template/file"},
	}
	if r.Partial > 0 {
		rows = append(rows, SummaryRow{Label: "partially reversible", Count: r.Partial,
			Note: "unarchive only removes directories it created, overwritten files are not restored"})
	}
	rows = append(rows,
		SummaryRow{Label: "read-only", Count: r.ReadOnly},
		SummaryRow{Label: "irreversible", Count: r.Irreversible},
	)
	return rows
}

// LifecycleNote 返回生命周期能力说明（卸载 / 运行中自动回滚）。
func (r *Reversibility) LifecycleNote() string {
	switch {
	case r.HasUninstall && r.AutoRollback:
		return "uninstallable (uninstall.yaml) with in-run auto rollback"
	case r.HasUninstall:
		return "uninstallable (uninstall.yaml); auto_rollback not configured (in-run failures are not recovered automatically)"
	case r.AutoRollback:
		return "in-run auto rollback supported; no uninstall.yaml (not uninstallable)"
	default:
		return "not uninstallable, no auto rollback on failure"
	}
}

// Uninstallable 报告该应用包整体是否可卸载。
func (r *Reversibility) Uninstallable() bool { return r.HasUninstall }
