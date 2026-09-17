package plan

// plan 编译器：控制端离线把 chart + inventory + values 编译为完全解析的
// 执行计划。不连接任何主机（跨主机信息在此固化为字面值；非部署相位的
// marker values 由调用方先行解析传入）。

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
	"sort"
	"strings"

	"wdp/internal/chart"
	"wdp/internal/inventory"
	"wdp/internal/model"
	"wdp/internal/module"
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

	Limits chart.Limits // 加载上限（tgz 解包等）
}

// Compile 编译执行计划。target 是 chart 目录或 .tgz；inv 提供主机选择与
// 内置变量快照；valuesFiles/setArgs 是 -f/--set 覆盖（仅部署相位使用）。
func Compile(target string, inv *inventory.Inventory, valuesFiles, setArgs []string, opts CompileOptions) (*Plan, error) {
	phase := opts.Phase
	if phase == "" {
		phase = "deploy"
	}
	ch, err := chart.LoadWithLimits(target, opts.Limits)
	if err != nil {
		return nil, err
	}
	defer ch.Close()

	plays, err := ch.PhasePlays(phase)
	if err != nil {
		return nil, err
	}
	spec := ch.PhaseSpecFor(phase)

	values := map[string]any{}
	switch spec.EffectiveValuesFrom() {
	case chart.ValuesFromChart:
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
		if len(opts.HostValues) == 0 {
			return nil, fmt.Errorf("phase %q resolves values from host release markers; pass the resolved per-host values (compile after reading markers)", phase)
		}
		for _, v := range opts.HostValues {
			values = v // 代表性 values（展示口径）
			break
		}
	}

	files, payloads, err := snapshotFiles(ch.Dir)
	if err != nil {
		return nil, err
	}

	facts := loadFacts(opts.FactCache)

	p := &Plan{
		SchemaVer:  SchemaVer,
		Chart:      ch.Meta.Name,
		Version:    ch.Meta.Version,
		Phase:      phase,
		WdpVersion: opts.WdpVersion,
		Values:     values,
		Meta:       ch.Meta,
		Helpers:    ch.CollectHelpers(),
		Files:      files,
		Payloads:   payloads,
	}
	// idx 按主机顺序统一编号（journal 的 (主机, 任务) 键要求主机内唯一且稳定）
	counters := map[string]int{}
	for playIdx, play := range plays {
		hosts := inv.SelectPlays([]*model.Play{play}, opts.Limit)
		pre, post, main := splitHooks(play.Tasks, phase)
		for _, h := range hosts {
			hc := hostConnOf(h)
			hc.Via = inv.ViaChain(h.Name)
			hp := &HostPlan{
				PlayIdx: playIdx,
				Host:    h.Name,
				Conn:    hc,
				Values:  hostValues(ch, values, h, opts.HostValues),
				Vars:    compileVars(inv, h, hostValues(ch, values, h, opts.HostValues), play, hosts, facts),
				Play:    playMetaOf(play, hosts),
			}
			n := counters[h.Name]
			if n == 0 {
				n = 1 // idx 从 1 起：0 保留为"非 plan 任务"哨兵（journal/续跑判定）
			}
			for _, group := range []struct {
				src []*model.Task
				dst *[]*ResolvedTask
			}{
				{pre, &hp.Pre}, {main, &hp.Tasks}, {post, &hp.Post}, {play.Handlers, &hp.Handlers},
			} {
				resolved := resolveTasks(group.src, n)
				*group.dst = resolved
				n += countResolved(resolved)
			}
			counters[h.Name] = n
			p.Hosts = append(p.Hosts, hp)
		}
	}
	// via 中继根的连接元数据单列：中继机不一定是部署目标，其连接信息不
	// 在 Hosts 里；自治提交按根分组时需要
	relays := map[string]HostConn{}
	targets := map[string]bool{}
	for _, hp := range p.Hosts {
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
	if len(relays) > 0 {
		p.Relays = relays
	}
	p.FillID()
	return p, nil
}

// countResolved 统计任务树节点数（block 子任务递归计入 idx 空间）。
func countResolved(tasks []*ResolvedTask) int {
	n := 0
	var walk func(ts []*ResolvedTask)
	walk = func(ts []*ResolvedTask) {
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

// splitHooks 按相位切分 hook 任务（与 executor.splitHookTasks 同语义）：
// pre_<phase>/post_<phase> 归两侧，无 hook 归主列表，其它相位的 hook 丢弃
// （uninstall 不跑 install hook）。deploy 相位沿用 install 命名。
func splitHooks(tasks []*model.Task, phase string) (pre, post, main []*model.Task) {
	preHook := "pre_" + chart.HookNameFor(phase)
	postHook := "post_" + chart.HookNameFor(phase)
	for _, t := range tasks {
		switch t.Hook {
		case preHook:
			pre = append(pre, t)
		case postHook:
			post = append(post, t)
		case "":
			main = append(main, t)
		}
	}
	return pre, post, main
}

// playMetaOf 快照 play 级编排属性。
func playMetaOf(p *model.Play, hosts []*model.Host) PlayMeta {
	names := make([]string, len(hosts))
	for i, h := range hosts {
		names[i] = h.Name
	}
	return PlayMeta{
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
func hostConnOf(h *model.Host) HostConn {
	c := HostConn{
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

// HostOf 把计划连接元数据还原为执行用主机（密钥经 env 引用恢复）。
func (c HostConn) Host(name string) *model.Host {
	h := &model.Host{
		Name:               name,
		Address:            c.Address,
		Port:               c.Port,
		User:               c.User,
		Conn:               c.Conn,
		AgentURL:           c.AgentURL,
		AgentPort:          c.AgentPort,
		TLS:                c.TLS,
		InsecureSkipVerify: c.InsecureSkipVerify,
		TLSSkipHostVerify:  c.TLSSkipHostVerify,
		TLSServerName:      c.TLSServerName,
		CAFile:             c.CAFile,
		CertFile:           c.CertFile,
		KeyFile:            c.KeyFile,
		HostKeyCheck:       c.HostKeyCheck,
		KnownHosts:         c.KnownHosts,
		ConnectTimeoutSec:  c.ConnectTimeoutSec,
		PasswordEnv:        c.PasswordEnv,
		Password:           c.PasswordEnvRef,
		KeyPassphraseEnv:   c.KeyPassphraseEnv,
		BecomePasswordEnv:  c.BecomePasswordEnv,
		BecomePassword:     c.BecomePasswordRef,
		Vars:               map[string]any{},
	}
	if h.Address == "" {
		h.Address = name
	}
	return h
}

// resolveTasks 把 model.Task 树转成计划镜像（idx 从 start 起顺序编号，
// block 子任务递归编号）。
func resolveTasks(tasks []*model.Task, start int) []*ResolvedTask {
	if len(tasks) == 0 {
		return nil
	}
	idx := start
	var out []*ResolvedTask
	var conv func(t *model.Task) *ResolvedTask
	conv = func(t *model.Task) *ResolvedTask {
		rt := &ResolvedTask{
			Idx:          idx,
			Label:        t.Label(),
			Module:       t.Module,
			Rollback:     rollbackOf(t),
			Hook:         t.Hook,
			ChartRef:     t.ChartRef,
			TasksFrom:    t.TasksFrom,
			ChartVars:    t.ChartVars,
			Args:         t.Args,
			FreeForm:     t.FreeForm,
			When:         t.When,
			Loop:         t.Loop,
			LoopVar:      t.LoopVar,
			Register:     t.Register,
			Notify:       t.Notify,
			Tags:         t.Tags,
			Environment:  t.Environment,
			IgnoreErrors: t.IgnoreErrors,
			Retries:      t.Retries,
			DelaySec:     t.DelaySec,
			TimeoutSec:   t.TimeoutSec,
			Become:       t.Become,
			BecomeUser:   t.BecomeUser,
			ChangedWhen:  t.ChangedWhen,
			FailedWhen:   t.FailedWhen,
			Until:        t.Until,
			Output:       t.Output,
			NoLog:        t.NoLog,
			DelegateTo:   t.DelegateTo,
			RunOnce:      t.RunOnce,
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

// snapshotFiles 以确定序快照 chart 目录树：packages/ 与超限大文件记录为
// PayloadRef（路径+尺寸+sha256，不进 plan 本体），其余小文件嵌入 base64。
func snapshotFiles(dir string) (map[string]string, []PayloadRef, error) {
	files := map[string]string{}
	var payloads []PayloadRef
	var paths []string
	var total int64
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(dir, path)
		if rerr != nil {
			return rerr
		}
		if !d.Type().IsRegular() {
			return nil
		}
		paths = append(paths, rel)
		return nil
	})
	if err != nil {
		return nil, nil, fmt.Errorf("failed to snapshot chart files: %w", err)
	}
	sort.Strings(paths)
	for _, rel := range paths {
		info, err := os.Stat(filepath.Join(dir, rel))
		if err != nil {
			return nil, nil, err
		}
		if info.Size() > maxFileBytes || isPayloadPath(rel) {
			sum, err := fileSHA256(filepath.Join(dir, rel))
			if err != nil {
				return nil, nil, err
			}
			payloads = append(payloads, PayloadRef{Path: rel, Size: info.Size(), SHA256: sum})
			continue
		}
		total += info.Size()
		if total > maxTotalBytes {
			return nil, nil, fmt.Errorf("chart tree exceeds the %d MiB plan total; move large payloads out of the chart (artifact module distributes them by URL)", maxTotalBytes>>20)
		}
		data, err := os.ReadFile(filepath.Join(dir, rel))
		if err != nil {
			return nil, nil, err
		}
		files[rel] = base64.StdEncoding.EncodeToString(data)
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
