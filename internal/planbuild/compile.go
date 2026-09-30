package planbuild

// plan 编译器：控制端离线把 chart + inventory + values 编译为完全解析的
// 执行计划。不连接任何主机（跨主机信息在此固化为字面值；非部署相位的
// marker values 由调用方先行解析传入）。
//
// 编译器与 plan 数据模型分属两包（分层方案 P3）：plan 是冻结契约包
//（依赖面收敛到 model，见 plan/deps_test.go），本包承载 chart 耦合的
// 编译逻辑——chart/playbook 的演进只影响本包，不触碰契约面。

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"wdp/internal/chart"
	"wdp/internal/inventory"
	"wdp/internal/model"
	"wdp/internal/module"
	"wdp/internal/plan"
)

// CompileOptions 是编译参数。
type CompileOptions struct {
	Phase      string // 生命周期相位（空串按 deploy）
	Limit      string // --limit
	WdpVersion string // 编译端版本（记录进 plan）
	FactCache  string // --fact-cache 路径（可选：已有 facts 冻结进变量域）

	// HostValues 每主机 values（非部署相位由调用方从 marker 还原后传入）。
	// nil = 部署相位，编译器从 chart 默认 values + 覆盖计算并应用
	// inventory_override。
	HostValues map[string]map[string]any

	Limits chart.Limits // 加载上限（tgz 解包等）；Compile 便捷入口加载 target 时使用，CompileChart 忽略
}

// Compile 编译执行计划（便捷入口：自行按 target 加载 chart）。target 是
// chart 目录或 .tgz；inv 提供主机选择与内置变量快照；valuesFiles/setArgs
// 是 -f/--set 覆盖（仅部署相位使用）。已持有 chart 的调用方（marker 相位
// 需先加载 chart 读相位与主机）走 CompileChart 免去二次加载。
func Compile(target string, inv *inventory.Inventory, valuesFiles, setArgs []string, opts CompileOptions) (*plan.Plan, error) {
	ch, err := chart.LoadWithLimits(target, opts.Limits)
	if err != nil {
		return nil, err
	}
	defer ch.Close()
	return CompileChart(ch, inv, valuesFiles, setArgs, opts)
}

// CompileChart 在已加载的 chart 上编译执行计划（参数语义同 Compile）。
// chart 的生命周期归调用方：编译期间须保持打开（tgz 形态挂着解包临时
// 目录），结束后自行 Close。
func CompileChart(ch *chart.Chart, inv *inventory.Inventory, valuesFiles, setArgs []string, opts CompileOptions) (*plan.Plan, error) {
	phase := opts.Phase
	if phase == "" {
		phase = "deploy"
	}

	plays, err := ch.PhasePlays(phase)
	if err != nil {
		return nil, err
	}
	spec := ch.PhaseSpecFor(phase)

	values, err := compileValues(ch, phase, spec, valuesFiles, setArgs, opts.HostValues)
	if err != nil {
		return nil, err
	}

	files, payloads, err := snapshotFiles(ch.Dir)
	if err != nil {
		return nil, err
	}

	facts := loadFacts(opts.FactCache)

	p := &plan.Plan{
		SchemaVer:  plan.SchemaVer,
		Chart:      ch.Meta.Name,
		Version:    ch.Meta.Version,
		Phase:      phase,
		WdpVersion: opts.WdpVersion,
		Values:     values,
		Meta:       metaOf(ch.Meta),
		Helpers:    ch.CollectHelpers(),
		Files:      files,
		Payloads:   payloads,
	}
	p.Hosts = compileHostPlans(inv, ch, plays, phase, values, opts, facts)
	// via 中继根的连接元数据单列：中继机不一定是部署目标，其连接信息不
	// 在 Hosts 里；自治提交按根分组时需要
	if relays := compileRelays(inv, p.Hosts); len(relays) > 0 {
		p.Relays = relays
	}
	if err := p.FillID(); err != nil {
		return nil, err
	}
	return p, nil
}

// compileValues 按相位语义解析代表性 values：部署事件相位取 chart 默认
// values + 覆盖并过 required/schema 校验；marker 相位要求调用方传入逐
// 主机还原值，代表值取主机名字典序首个。
func compileValues(ch *chart.Chart, phase string, spec chart.PhaseSpec, valuesFiles, setArgs []string, hostValues map[string]map[string]any) (map[string]any, error) {
	var values map[string]any
	switch spec.EffectiveValuesFrom() {
	case chart.ValuesFromChart:
		var err error
		values, err = ch.BuildValues(valuesFiles, setArgs)
		if err != nil {
			return nil, err
		}
		// 与 run 同门控：只有部署事件相位强制 required + schema
		if spec.Release {
			if err := ch.ValidateRequired(values); err != nil {
				return nil, err
			}
			if err := ch.ValidateValuesSchema(values); err != nil {
				return nil, err
			}
			if err := ch.ValidateSubchartsSchema(values); err != nil {
				return nil, err
			}
		}
	default: // ValuesFromMarker
		if len(hostValues) == 0 {
			return nil, fmt.Errorf("phase %q resolves values from host release markers; pass the resolved per-host values (compile after reading markers)", phase)
		}
		// 代表性 values 必须确定：map 迭代随机会让两次编译产出不同
		// Plan.Values/PlanID，破坏"逐字节相同 plan.json"的内容寻址承诺。
		// 取主机名字典序首个。
		first := slices.Sorted(maps.Keys(hostValues))[0]
		values = hostValues[first]
	}
	return values, nil
}

// compileHostPlans 按 play × host 编译主机计划：连接元数据（含 via 链）、
// 主机 values 与变量域快照在此冻结，任务树解析为计划镜像。
// idx 按主机顺序统一编号（journal 的 (主机, 任务) 键要求主机内唯一且稳定）
func compileHostPlans(inv *inventory.Inventory, ch *chart.Chart, plays []*model.Play, phase string, values map[string]any, opts CompileOptions, facts map[string]map[string]any) []*plan.HostPlan {
	var hostPlans []*plan.HostPlan
	counters := map[string]int{}
	for playIdx, play := range plays {
		hosts := inv.SelectPlays([]*model.Play{play}, opts.Limit)
		pre, post, main := chart.SplitHookTasks(play.Tasks, phase)
		for _, h := range hosts {
			hc := hostConnOf(h)
			hc.Via = inv.ViaChain(h.Name)
			// hostValues 含全量 deepCopy，只算一次并复用（此前算了两遍）
			hv := hostValues(ch, values, h, opts.HostValues)
			hp := &plan.HostPlan{
				PlayIdx: playIdx,
				Host:    h.Name,
				Conn:    hc,
				Values:  hv,
				Vars:    compileVars(inv, h, hv, play, hosts, facts),
				Play:    playMetaOf(play, hosts),
			}
			n := counters[h.Name]
			if n == 0 {
				n = 1 // idx 从 1 起：0 保留为"非 plan 任务"哨兵（journal/续跑判定）
			}
			for _, group := range []struct {
				src []*model.Task
				dst *[]*plan.ResolvedTask
			}{
				{pre, &hp.Pre}, {main, &hp.Tasks}, {post, &hp.Post}, {play.Handlers, &hp.Handlers},
			} {
				resolved := resolveTasks(group.src, n)
				*group.dst = resolved
				n += countResolved(resolved)
			}
			counters[h.Name] = n
			hostPlans = append(hostPlans, hp)
		}
	}
	return hostPlans
}

// compileRelays 收集 via 中继根的连接元数据（不在 Hosts 里的非目标中继机）。
func compileRelays(inv *inventory.Inventory, hostPlans []*plan.HostPlan) map[string]plan.HostConn {
	relays := map[string]plan.HostConn{}
	targets := map[string]bool{}
	for _, hp := range hostPlans {
		targets[hp.Host] = true
	}
	for _, h := range inv.Hosts {
		if chain := inv.ViaChain(h.Name); len(chain) > 0 {
			root := chain[len(chain)-1]
			if rh := inv.HostByName(root); rh != nil && !targets[root] {
				relays[root] = hostConnOf(rh)
			}
		}
	}
	return relays
}

// countResolved 统计任务树节点数（block 子任务递归计入 idx 空间）。
func countResolved(tasks []*plan.ResolvedTask) int {
	n := 0
	var walk func(ts []*plan.ResolvedTask)
	walk = func(ts []*plan.ResolvedTask) {
		for _, t := range ts {
			n++
			walk(t.Block)
			walk(t.Rescue)
			walk(t.Always)
		}
	}
	walk(tasks)
	return n
}

// playMetaOf 快照 play 级编排属性。
func playMetaOf(p *model.Play, hosts []*model.Host) plan.PlayMeta {
	names := make([]string, len(hosts))
	for i, h := range hosts {
		names[i] = h.Name
	}
	return plan.PlayMeta{
		Name:        p.Name,
		Hosts:       p.Hosts,
		Become:      p.Become,
		BecomeUser:  p.BecomeUser,
		Serial:      p.Serial,
		Strategy:    p.Strategy,
		Environment: p.Environment,
		Vars:        p.Vars,
	}
}

// hostValues 计算主机实际生效 values：非部署相位用调用方传入的 marker
// 还原值；部署相位在全局 values 上应用 inventory_override 白名单。
func hostValues(ch *chart.Chart, values map[string]any, h *model.Host, markerBased map[string]map[string]any) map[string]any {
	if mv, ok := markerBased[h.Name]; ok && mv != nil {
		return mv
	}
	vals := deepCopy(values)
	for _, k := range ch.Meta.InventoryOverride {
		if v, ok := h.Vars[k]; ok {
			vals[k] = v
		}
	}
	return vals
}

// compileVars 冻结主机变量域：inventory vars → values → play vars →
// fact cache → 内置变量快照（groups/hosts/hostvars 跨主机信息固化——
// 主机侧执行时拿不到其他主机 facts）。playbook_dir 不冻结（执行侧
// BaseDir 是物化目录，与编译端路径不同）；play_batch 由执行侧按实际
// 批次覆盖。
func compileVars(inv *inventory.Inventory, h *model.Host, hostVals map[string]any, play *model.Play, playHosts []*model.Host, facts map[string]map[string]any) map[string]any {
	vars := map[string]any{}
	maps.Copy(vars, h.Vars)
	maps.Copy(vars, hostVals)
	maps.Copy(vars, play.Vars)
	if f := facts[h.Name]; len(f) > 0 {
		maps.Copy(vars, f)
	}
	names := make([]string, len(playHosts))
	for i, ph := range playHosts {
		names[i] = ph.Name
	}
	vars["inventory_hostname"] = h.Name
	vars["group_names"] = h.Vars["group_names"]
	vars["play_hosts"] = names
	vars["play_batch"] = names
	vars["groups"] = inv.GroupsMap()
	vars["hosts"] = inv.HostsMeta()
	vars["hostvars"] = hostvarsSnapshot(inv, facts)
	return vars
}

// hostvarsSnapshot 主机名 → 变量域快照（inventory vars + fact cache），
// 与 executor.hostvarsSnapshot 同口径（不含 register——跨主机共享一律经
// set_fact）。
func hostvarsSnapshot(inv *inventory.Inventory, facts map[string]map[string]any) map[string]map[string]any {
	out := make(map[string]map[string]any, len(inv.Hosts))
	for _, h := range inv.Hosts {
		m := make(map[string]any, len(h.Vars)+4)
		maps.Copy(m, h.Vars)
		if f := facts[h.Name]; len(f) > 0 {
			maps.Copy(m, f)
		}
		out[h.Name] = m
	}
	return out
}

// hostConnOf 提取可安全落盘的连接元数据（明文密钥剔除，env 引用保留）。
func hostConnOf(h *model.Host) plan.HostConn {
	c := plan.HostConn{
		Address:            h.Address,
		Port:               h.Port,
		User:               h.User,
		Conn:               h.Conn,
		AgentURL:           h.AgentURL,
		AgentPort:          h.AgentPort,
		TLS:                h.TLS,
		InsecureSkipVerify: h.InsecureSkipVerify,
		TLSSkipHostVerify:  h.TLSSkipHostVerify,
		TLSServerName:      h.TLSServerName,
		CAFile:             h.CAFile,
		CertFile:           h.CertFile,
		KeyFile:            h.KeyFile,
		HostKeyCheck:       h.HostKeyCheck,
		KnownHosts:         h.KnownHosts,
		ConnectTimeoutSec:  h.ConnectTimeoutSec,
		PasswordEnv:        h.PasswordEnv,
		KeyPassphraseEnv:   h.KeyPassphraseEnv,
		BecomePasswordEnv:  h.BecomePasswordEnv,
	}
	// "env:VAR" 前缀的间接引用不是密钥本体，保留（apply 时经 Secret 解析）
	if strings.HasPrefix(h.Password, "env:") {
		c.PasswordEnvRef = h.Password
	}
	if strings.HasPrefix(h.BecomePassword, "env:") {
		c.BecomePasswordRef = h.BecomePassword
	}
	return c
}

// resolveTasks 把 model.Task 树转成计划镜像（idx 从 start 起顺序编号，
// block 子任务递归编号）。
func resolveTasks(tasks []*model.Task, start int) []*plan.ResolvedTask {
	if len(tasks) == 0 {
		return nil
	}
	idx := start
	var out []*plan.ResolvedTask
	var conv func(t *model.Task) *plan.ResolvedTask
	conv = func(t *model.Task) *plan.ResolvedTask {
		rt := &plan.ResolvedTask{
			Idx:             idx,
			Label:           t.Label(),
			Module:          t.Module,
			Rollback:        rollbackOf(t),
			Hook:            t.Hook,
			ChartRef:        t.ChartRef,
			TasksFrom:       t.TasksFrom,
			ChartVars:       t.ChartVars,
			ChartHosts:      t.ChartHosts,
			ChartValuesFrom: t.ChartValuesFrom,
			Args:            t.Args,
			FreeForm:        t.FreeForm,
			When:            t.When,
			Loop:            t.Loop,
			LoopVar:         t.LoopVar,
			Register:        t.Register,
			Notify:          t.Notify,
			Tags:            t.Tags,
			Environment:     t.Environment,
			IgnoreErrors:    t.IgnoreErrors,
			Retries:         t.Retries,
			DelaySec:        t.DelaySec,
			TimeoutSec:      t.TimeoutSec,
			Become:          t.Become,
			BecomeUser:      t.BecomeUser,
			ChangedWhen:     t.ChangedWhen,
			FailedWhen:      t.FailedWhen,
			Until:           t.Until,
			Output:          t.Output,
			NoLog:           t.NoLog,
			DelegateTo:      t.DelegateTo,
			RunOnce:         t.RunOnce,
		}
		idx++
		for _, b := range t.Block {
			rt.Block = append(rt.Block, conv(b))
		}
		for _, b := range t.Rescue {
			rt.Rescue = append(rt.Rescue, conv(b))
		}
		for _, b := range t.Always {
			rt.Always = append(rt.Always, conv(b))
		}
		return rt
	}
	for _, t := range tasks {
		out = append(out, conv(t))
	}
	return out
}

// rollbackOf 返回模块声明的回滚能力分级（chart 引用按 none——其子任务
// 在执行侧展开后各自分级）。
func rollbackOf(t *model.Task) string {
	if t.ChartRef != "" {
		return "none"
	}
	if module.IsReadOnlyModule(t.Module) {
		return "readonly"
	}
	switch module.RollbackCapabilityOf(t.Module) {
	case module.RollbackFull:
		return "full"
	case module.RollbackPartial:
		return "partial"
	default:
		return "none"
	}
}

// payloadDirName 是制品缓存目录约定（顶层 packages/，artifact 模块的
// cache 基准）。该目录整体不进 plan 本体：制品经 artifact 模块按 URL
// 分发或 apply 时经 --chart-dir 本地补齐——plan 是配置与意图的载体。
const payloadDirName = "packages"

// snapshotFileEnt 是 walk 期缓存的文件条目：相对路径 + 目录项自带的
// FileInfo。快照循环里再逐个 os.Stat 既多一倍系统调用，也引入 walk 与
// stat 之间文件被替换的观察口径漂移（TOCTOU）——统一以 walk 时刻的
// d.Info() 为准。
type snapshotFileEnt struct {
	rel  string
	info fs.FileInfo
}

// snapshotFiles 以确定序快照 chart 目录树：packages/ 与超限大文件记录为
// PayloadRef（路径+尺寸+sha256，不进 plan 本体），其余小文件嵌入 base64。
func snapshotFiles(dir string) (map[string]string, []plan.PayloadRef, error) {
	files := map[string]string{}
	var payloads []plan.PayloadRef
	var ents []snapshotFileEnt
	var total int64
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, rerr := filepath.Rel(dir, path)
		if rerr != nil {
			return rerr
		}
		info, ierr := d.Info()
		if ierr != nil {
			return ierr
		}
		ents = append(ents, snapshotFileEnt{rel: rel, info: info})
		return nil
	})
	if err != nil {
		return nil, nil, fmt.Errorf("failed to snapshot chart files: %w", err)
	}
	sort.Slice(ents, func(i, j int) bool { return ents[i].rel < ents[j].rel })
	for _, ent := range ents {
		path := filepath.Join(dir, ent.rel)
		if ent.info.Size() > maxFileBytes || isPayloadPath(ent.rel) {
			sum, err := fileSHA256(path)
			if err != nil {
				return nil, nil, err
			}
			payloads = append(payloads, plan.PayloadRef{Path: ent.rel, Size: ent.info.Size(), SHA256: sum})
			continue
		}
		total += ent.info.Size()
		if total > maxTotalBytes {
			return nil, nil, fmt.Errorf("chart tree exceeds the %d MiB plan total; move large payloads out of the chart (artifact module distributes them by URL)", maxTotalBytes>>20)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, nil, err
		}
		files[ent.rel] = base64.StdEncoding.EncodeToString(data)
	}
	return files, payloads, nil
}

// isPayloadPath 报告相对路径是否落在制品缓存目录（packages/...）。
func isPayloadPath(rel string) bool {
	first, _, _ := strings.Cut(filepath.ToSlash(rel), "/")
	return first == payloadDirName
}

// fileSHA256 流式计算文件摘要。
func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// loadFacts 读取 fact cache（损坏/缺失返回空——cache 是加速器不是数据源）。
func loadFacts(path string) map[string]map[string]any {
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var facts map[string]map[string]any
	if err := json.Unmarshal(data, &facts); err != nil {
		return nil
	}
	return facts
}

// deepCopy 深拷贝 values（map/[]any 递归）。
func deepCopy(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = deepCopyAny(v)
	}
	return out
}

func deepCopyAny(v any) any {
	switch x := v.(type) {
	case map[string]any:
		return deepCopy(x)
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = deepCopyAny(e)
		}
		return out
	default:
		return v
	}
}

// 文件嵌入上限：chart 是配置载体（模板/清单/小文件），大负载走 artifact
// 模块按 URL 分发——超限即编译报错，避免 plan 变成制品分发通道。
const (
	MaxFileBytes  int64 = 32 << 20  // 单文件 32MiB
	MaxTotalBytes int64 = 256 << 20 // 全部文件合计 256MiB
)

// 阈值以变量形式参与判定：测试用小型临时文件即可覆盖"超限转 payload /
// 总量超限报错"两条分支，无需真的写出 32MiB 制品。生产路径恒等于上面的常量。
var (
	maxFileBytes  = MaxFileBytes
	maxTotalBytes = MaxTotalBytes
)

// metaOf 把 chart.Meta 转换为计划快照镜像（字段级拷贝；plan.Meta 与
// chart.Meta 的 JSON 编码字节一致，PlanID 内容寻址不受影响）。
func metaOf(m chart.Meta) plan.Meta {
	pm := plan.Meta{
		Name:              m.Name,
		Version:           m.Version,
		Description:       m.Description,
		Required:          m.Required,
		MarkerDir:         m.MarkerDir,
		NoMarker:          m.NoMarker,
		CheckMode:         bool(m.CheckMode),
		InventoryOverride: m.InventoryOverride,
		SensitiveValues:   m.SensitiveValues,
	}
	if len(m.Phases) > 0 {
		pm.Phases = make(map[string]plan.PhaseSpec, len(m.Phases))
		for k, spec := range m.Phases {
			pm.Phases[k] = plan.PhaseSpec{
				Release:      spec.Release,
				Record:       spec.Record,
				ClearsMarker: spec.ClearsMarker,
				ValuesFrom:   plan.ValuesFrom(spec.ValuesFrom),
			}
		}
	}
	return pm
}
