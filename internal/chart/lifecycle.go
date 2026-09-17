package chart

import (
	"fmt"
	"slices"
	"strings"

	"wdp/internal/model"
)

// ValuesFrom 决定相位读取 values 的来源。
type ValuesFrom string

const (
	// ValuesFromChart 用 chart 默认 values + -f/--set 覆盖（部署语义：本次
	// 提供的入参）。
	ValuesFromChart ValuesFrom = "chart"
	// ValuesFromMarker 用各主机 release marker 记录的实际部署 values
	//（+ -f/--set 显式覆盖）——卸载类相位必须按"当初实际部署了什么"来删。
	ValuesFromMarker ValuesFrom = "marker"
)

// PhaseSpec 是相位属性：相位文件（根目录 <phase>.yaml）决定"跑哪些任务"，
// 属性决定"相位语义"——values 从哪来、是否视同一次部署、是否留部署记录、
// marker 如何处置。内置相位有缺省属性，chart.yaml 的 phases: 可为任意相位
// 补充声明（只能追加，不能关闭内置行为）。未声明的自定义相位：跑任务、
// 读 chart values、不动 marker 不留记录。
type PhaseSpec struct {
	Release      bool       `yaml:"release"`       // 部署语义：required 校验、可逆性确认、成功后写 marker、记部署记录
	Record       bool       `yaml:"record"`        // 记部署记录（Release 隐含本项；uninstall 内置——卸载同样留痕）
	ClearsMarker bool       `yaml:"clears_marker"` // 成功后清除 release marker（uninstall 内置）
	ValuesFrom   ValuesFrom `yaml:"values_from"`   // values 来源（空 = 按相位推导，见 EffectiveValuesFrom）
}

// Records 报告该相位是否写部署记录（release record）。
func (s PhaseSpec) Records() bool { return s.Release || s.Record }

// Destructive 报告该相位是否可能破坏现场 → 必须走可逆性确认，且用
// values 拼删除路径时必须过 required + schema 校验。
func (s PhaseSpec) Destructive() bool { return s.Release || s.ClearsMarker }

// EffectiveValuesFrom 解析该相位实际使用的 values 来源：显式声明优先；
// 否则清除 marker 的相位（uninstall/purge）取 marker 记录的实际部署入参，
// 其余相位取 chart values。
//
// 这条规则此前是隐式的（非 release 相位一律读 marker），把"纯准备/只读"
// 相位（download、status）也拖进了"必须先部署过"的前提——未部署的主机上
// 连制品预下载都跑不起来。
func (s PhaseSpec) EffectiveValuesFrom() ValuesFrom {
	if s.ValuesFrom != "" {
		return s.ValuesFrom
	}
	if s.ClearsMarker {
		return ValuesFromMarker
	}
	return ValuesFromChart
}

// builtinPhaseSpecs 是内置相位的缺省属性。
var builtinPhaseSpecs = map[string]PhaseSpec{
	"deploy":    {Release: true, Record: true},
	"uninstall": {Record: true, ClearsMarker: true},
	// status 与未声明的自定义相位：零值（只跑任务、读 chart values、
	// 不写记录不动 marker）。
}

// DefaultPhaseSpec 返回相位的内置缺省属性（无 chart 上下文时的判定依据；
// 空串按 deploy，未知名返回零值）。
func DefaultPhaseSpec(phase string) PhaseSpec {
	if phase == "" {
		phase = "deploy"
	}
	return builtinPhaseSpecs[phase]
}

// MergePhaseSpec 合并内置缺省与 chart.yaml phases: 声明（声明只能追加
// 属性；values_from 是"覆盖"语义——它是唯一可以改向的字段）。
// chart.PhaseSpecFor 与 plan 快照侧的合成共用本函数，避免两处规则漂移。
func MergePhaseSpec(base, declared PhaseSpec) PhaseSpec {
	base.Release = base.Release || declared.Release
	base.Record = base.Record || declared.Record || declared.Release
	base.ClearsMarker = base.ClearsMarker || declared.ClearsMarker
	if declared.ValuesFrom != "" {
		base.ValuesFrom = declared.ValuesFrom
	}
	return base
}

// PhaseSpecFor 返回相位属性：内置缺省与 chart.yaml phases: 声明合并
// （声明只能追加属性）。空串按 deploy。
func (c *Chart) PhaseSpecFor(phase string) PhaseSpec {
	spec := DefaultPhaseSpec(phase)
	if c != nil {
		if declared, ok := c.Meta.Phases[phase]; ok {
			spec = MergePhaseSpec(spec, declared)
		}
	}
	return spec
}

// HookNameFor 返回相位对应的 hook 词干：hook 任务标记 pre_<词干>/post_<词干>
// 即在该相位的 pre/post 时机执行。deploy 相位沿用历史命名 install
// （pre_install/post_install），其余相位词干即相位名。
func HookNameFor(phase string) string {
	if phase == "" || phase == "deploy" {
		return "install"
	}
	return phase
}

// NormalizeHook 归一化 hook 名：deploy 相位的词干是 install（历史命名），
// 但按"词干即相位名"的规则直觉会写 pre_deploy——两者等价，统一收敛到
// install 词干，避免同一时机有两个只有拼写不同的名字。
func NormalizeHook(hook string) string {
	switch hook {
	case "pre_deploy":
		return "pre_install"
	case "post_deploy":
		return "post_install"
	default:
		return hook
	}
}

// PhasePlays 返回指定生命周期相位对应的 play 清单。相位 = chart 根目录的
// <phase>.yaml 文件：deploy.yaml 必需，uninstall/status/自定义相位可选
// （空串按 deploy）。未知相位报错并列出 chart 实际提供的相位。
func (c *Chart) PhasePlays(phase string) ([]*model.Play, error) {
	if phase == "" || phase == "deploy" {
		return c.Deploy, nil
	}
	plays, ok := c.Phases[phase]
	if !ok || plays == nil {
		return nil, fmt.Errorf("unknown --phase %q (chart provides: %s)", phase, strings.Join(c.PhaseNames(), ", "))
	}
	return plays, nil
}

// EntryPlay 返回 chart 引用任务（`chart: <ref>` 可选 `tasks_from: <phase>`）的
// 入口 play：phase 为空或 "deploy" 用 deploy.yaml（缺 play 时空 play 兜底，与
// 历史行为一致），否则取该相位文件的唯一 play——入口任务序列要内联进父 play，
// 多 play 相位无法展开。未知相位报错并列出 chart 实际提供的相位。
func (c *Chart) EntryPlay(phase string) (*model.Play, error) {
	if phase == "" || phase == "deploy" {
		if len(c.Deploy) == 1 {
			return c.Deploy[0], nil
		}
		return &model.Play{}, nil
	}
	plays, ok := c.Phases[phase]
	if !ok || len(plays) == 0 {
		return nil, fmt.Errorf("unknown tasks_from phase %q (chart provides: %s)", phase, strings.Join(c.PhaseNames(), ", "))
	}
	if len(plays) > 1 {
		return nil, fmt.Errorf("tasks_from phase %q contains %d plays (only one is supported)", phase, len(plays))
	}
	return plays[0], nil
}

// AllPlays 返回全部相位（deploy 在前，其余按相位名字典序）的全部 play。
// 消费方是 executor 的子 chart handler 合并：入口相位（tasks_from 引用的
// 自定义相位）里的 handler 同样要能被 notify 触发。
func (c *Chart) AllPlays() []*model.Play {
	var out []*model.Play
	for _, pf := range c.phaseFiles() {
		out = append(out, pf.plays...)
	}
	return out
}

// PhaseNames 返回全部可用相位名（deploy 在前，其余按字典序，输出确定）。
func (c *Chart) PhaseNames() []string {
	names := make([]string, 0, len(c.Phases)+1)
	names = append(names, "deploy")
	for n := range c.Phases {
		names = append(names, n)
	}
	slices.Sort(names[1:])
	return names
}

// phaseFile 是相位文件条目（name 为文件名，plays 为解析结果）。
type phaseFile struct {
	name  string
	plays []*model.Play
}

// phaseFiles 返回全部相位文件条目（deploy.yaml 在前，其余按相位名字典序，
// 遍历输出确定）——lint 等需要逐文件定位问题的消费者用。
func (c *Chart) phaseFiles() []phaseFile {
	out := make([]phaseFile, 0, len(c.Phases)+1)
	out = append(out, phaseFile{name: "deploy.yaml", plays: c.Deploy})
	for _, n := range c.PhaseNames()[1:] {
		out = append(out, phaseFile{name: n + ".yaml", plays: c.Phases[n]})
	}
	return out
}

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
