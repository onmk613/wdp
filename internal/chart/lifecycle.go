package chart

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"wdp/internal/model"
)

// Reversibility 是应用包的可逆性评估（部署前提示与确认的依据）。
type Reversibility struct {
	Reversible   int      // 可回滚任务数（copy/template/file：快照恢复 + 可 absent 卸载）
	ReadOnly     int      // 只读任务数（setup）
	Irreversible int      // 不可逆任务数（shell/script/package/service：无法自动回滚）
	Examples     []string // 不可逆任务示例（标签，最多 5 个）
	HasUninstall bool     // 提供 uninstall.yaml
	HasStatus    bool     // 提供 status.yaml
	AutoRollback bool     // 任一 play 配置 strategy.auto_rollback
}

// 可逆模块：变更经快照登记可自动回滚，且可用 file absent 逆操作卸载。
var reversibleModules = map[string]bool{
	"copy": true, "template": true, "file": true,
}

// 只读模块：不产生变更。
var readOnlyModules = map[string]bool{
	"setup": true,
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
	sort.Strings(r.Examples)
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
	switch {
	case reversibleModules[t.Module]:
		r.Reversible++
	case readOnlyModules[t.Module]:
		r.ReadOnly++
	default:
		r.Irreversible++
		r.Examples = append(r.Examples, label+" ("+t.Module+")")
	}
}

// Summary 渲染人读评估摘要（部署前提示用）。
func (r *Reversibility) Summary() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "可逆 %d（copy/template/file）· 只读 %d · 不可逆 %d",
		r.Reversible, r.ReadOnly, r.Irreversible)
	if len(r.Examples) > 0 {
		fmt.Fprintf(&sb, "，如: %s", strings.Join(r.Examples, "、"))
	}
	sb.WriteString("；")
	switch {
	case r.HasUninstall && r.AutoRollback:
		sb.WriteString("支持卸载（uninstall.yaml）与运行内自动回滚")
	case r.HasUninstall:
		sb.WriteString("支持卸载（uninstall.yaml）；未配置 auto_rollback（运行内失败不自动恢复）")
	case r.AutoRollback:
		sb.WriteString("支持运行内自动回滚；未提供 uninstall.yaml（不可卸载）")
	default:
		sb.WriteString("不可卸载、失败不自动回滚")
	}
	return sb.String()
}

// Uninstallable 报告该应用包整体是否可卸载。
func (r *Reversibility) Uninstallable() bool { return r.HasUninstall }

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
		return fmt.Errorf("缺少必需配置项（chart.yaml required）: %s（用 -f envs/*.yaml 或 --set 提供）",
			strings.Join(missing, ", "))
	}
	return nil
}

// pathExists 按点路径检查 values 中是否存在该键。
func pathExists(values map[string]any, path string) bool {
	cur := values
	for i, seg := range strings.Split(path, ".") {
		v, ok := cur[seg]
		if !ok || v == nil {
			return false
		}
		if i == len(strings.Split(path, "."))-1 {
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
