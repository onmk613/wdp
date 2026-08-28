package chart

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

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

// ---- 生命周期相位 ----

// PhasePlays 返回指定生命周期相位对应的 play 清单（deploy | uninstall | status；空串按 deploy）。
// uninstall/status 为可选清单，缺失时返回错误。
func (c *Chart) PhasePlays(phase string) ([]*model.Play, error) {
	switch phase {
	case "", "deploy":
		return c.Deploy, nil
	case "uninstall":
		if c.Uninstall == nil {
			return nil, fmt.Errorf("chart %s does not provide uninstall.yaml and cannot be uninstalled", c.Meta.Name)
		}
		return c.Uninstall, nil
	case "status":
		if c.Status == nil {
			return nil, fmt.Errorf("chart %s does not provide status.yaml", c.Meta.Name)
		}
		return c.Status, nil
	default:
		return nil, fmt.Errorf("unknown --phase %q (options: deploy/uninstall/status)", phase)
	}
}

// ---- required 校验与 values 摘要 ----

// ValidateRequired 校验合并后的 values 覆盖 chart.yaml required 声明的全部点路径。
func (c *Chart) ValidateRequired(values map[string]any) error {
	if len(c.Meta.Required) == 0 {
		return nil
	}
	var missing []string
	for _, path := range c.Meta.Required {
		if !pathExists(values, path) {
			missing = append(missing, path)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing required config items (chart.yaml required): %s (provide via -f envs/*.yaml or --set)",
			strings.Join(missing, ", "))
	}
	return nil
}

// pathExists 按点路径检查 values 中是否存在该键（解析复用 --set 的
// parsePath，支持下标写法 a.b[0]——此前 "b[0]" 整段当键查找会误报缺失）。
func pathExists(values map[string]any, path string) bool {
	segs, err := parsePath(path)
	if err != nil || len(segs) == 0 {
		return false
	}
	cur := values
	for i, s := range segs {
		var v any
		if !s.hasID {
			c, ok := cur[s.key]
			if !ok || c == nil {
				return false
			}
			v = c
		} else {
			list, ok := cur[s.key].([]any)
			if !ok || s.idx >= len(list) || list[s.idx] == nil {
				return false
			}
			v = list[s.idx]
		}
		if i == len(segs)-1 {
			return true
		}
		next, ok := v.(map[string]any)
		if !ok {
			return false
		}
		cur = next
	}
	return false
}

// ValuesDigest 返回 values 的短摘要（sha256 前 12 位，marker 记录用）。
func ValuesDigest(values map[string]any) string {
	b, err := json.Marshal(values)
	if err != nil {
		return "n/a"
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:12]
}

// MarkerDir 返回 release marker 目录（meta 覆盖或默认 /var/lib/wdp）。
func (c *Chart) MarkerDir() string {
	if c.Meta.MarkerDir != "" {
		return c.Meta.MarkerDir
	}
	return "/var/lib/wdp"
}

// MarkerEnabled 报告是否写 release marker。
func (c *Chart) MarkerEnabled() bool { return !c.Meta.NoMarker }

// MarkerPath 返回指定主机的 marker 文件路径。
func (c *Chart) MarkerPath() string {
	return c.MarkerDir() + "/" + c.Meta.Name + "/release.json"
}

// MarkerContent 构造 marker JSON 内容。
func (c *Chart) MarkerContent(wdpVersion string, values map[string]any) []byte {
	type marker struct {
		Chart      string `json:"chart"`
		Version    string `json:"version"`
		Phase      string `json:"phase"`
		DeployedAt string `json:"deployed_at"`
		ValuesSHA  string `json:"values_sha256"`
		WdpVersion string `json:"wdp_version"`
	}
	b, _ := json.MarshalIndent(marker{
		Chart:      c.Meta.Name,
		Version:    c.Meta.Version,
		Phase:      "deploy",
		DeployedAt: time.Now().UTC().Format(time.RFC3339),
		ValuesSHA:  ValuesDigest(values),
		WdpVersion: wdpVersion,
	}, "", "  ")
	return b
}
