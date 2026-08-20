// Package executor 是编排引擎：按 play 推进任务，
// 以 forks 限流并发到各主机，处理 when/loop/register/notify/handlers，
// 并支持 chart 任务（子 chart 任务序列就地展开，作用域隔离）。
package executor

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"wdp/internal/chart"
	"wdp/internal/connection"
	"wdp/internal/inventory"
	"wdp/internal/model"
	"wdp/internal/module"
	"wdp/internal/render"
	"wdp/internal/report"
	"wdp/internal/shellquote"
)

// Options 是执行选项。
type Options struct {
	Forks       int    // 并发上限，缺省 5
	Limit       string // --limit，进一步收窄主机范围
	Tags        []string
	SkipTags    []string
	ListHosts   bool
	StartAtTask string
	BaseDir     string // playbook/chart 目录（模块解析 src 相对路径）

	TaskTimeout int    // 任务默认超时秒数（0 不限），任务级 timeout 属性可覆盖
	CheckMode   bool   // check 模式：模块预演不实际变更
	DiffMode    bool   // diff 模式：check 下输出内容级差异（copy/template/file 等）
	Phase       string // chart 生命周期相位：deploy（缺省）| uninstall | status
	WdpVersion  string // 控制端版本（release marker 记录）

	Chart  *chart.Chart   // chart 模式（nil = 裸 playbook 模式）
	Values map[string]any // chart 合并后的最终 values
	Engine *render.Engine // 渲染引擎（含 helpers 命名模板）
}

// Executor 执行 playbook / chart。
type Executor struct {
	Inv   *inventory.Inventory
	Conns *connection.Manager
	Rep   report.Reporter
	Opts  Options

	engine *render.Engine

	rollbackDir string // 当前 play 的回滚快照根目录（auto_rollback 时非空）

	invMu sync.Mutex // 动态组（group_by）并发聚合锁

	factsMu sync.Mutex
	facts   map[string]map[string]any // 跨 play / 子 chart 作用域的主机 facts（setup/stat 等）

	statsMu sync.Mutex
	stats   map[string]*model.Stats // 当前 play 的统计（子 chart 子任务也计入）
}

// hostRun 是单主机在单个 play 中的运行态。
type hostRun struct {
	mu         sync.Mutex
	host       *model.Host
	vars       map[string]any // 当前变量域
	chartScope map[string]any // 当前 chart 的 values 作用域
	baseDir    string         // 当前 chart 根目录（src 相对路径基准）
	alive      bool
	notified   map[string]bool
	stats      *model.Stats
	journal    []module.RollbackAction // 变更日志（auto_rollback 时快照恢复依据）
}

// playState 延续同一 play 内跨批次/跨 hook 的主机运行态：
// register/facts 变量与回滚变更日志在批次间保持（修复原批次间 register 丢失）。
type playState struct {
	mu      sync.Mutex
	vars    map[string]map[string]any          // 主机名 → 最新变量域
	journal map[string][]module.RollbackAction // 主机名 → 累积变更日志
}

func newPlayState() *playState {
	return &playState{
		vars:    map[string]map[string]any{},
		journal: map[string][]module.RollbackAction{},
	}
}

// transientVars 是每次批次重建的瞬态变量（不从上一批延续）。
var transientVars = map[string]bool{"item": true, "result": true, "play_batch": true}

// seed 把此前批次的运行时变量叠加到新建变量域（静态层之上：register/facts 应胜出）。
func (s *playState) seed(host string, vars map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, v := range s.vars[host] {
		if transientVars[k] {
			continue
		}
		vars[k] = v
	}
}

// harvest 回收一批主机运行态（变量剔除瞬态键；回滚日志累积）。
func (s *playState) harvest(runs []*hostRun) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, hr := range runs {
		clean := make(map[string]any, len(hr.vars))
		for k, v := range hr.vars {
			if transientVars[k] {
				continue
			}
			clean[k] = v
		}
		s.vars[hr.host.Name] = clean
		hr.mu.Lock()
		s.journal[hr.host.Name] = hr.journal // hr.journal 以累积副本起步，整体替换即可
		hr.mu.Unlock()
	}
}

// takeJournal 取走主机已累积的回滚日志作为本批起点。
func (s *playState) takeJournal(host string) []module.RollbackAction {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]module.RollbackAction{}, s.journal[host]...)
}

// splitHookTasks 按生命周期相位切分任务：相位匹配的 hook 任务归 pre/post，
// 无 hook 的归主列表，其它相位的 hook 任务跳过（uninstall 时不跑 install hook）。
func splitHookTasks(tasks []*model.Task, phase string) (pre, post, main []*model.Task) {
	var preHook, postHook string
	switch phase {
	case "uninstall":
		preHook, postHook = "pre_uninstall", "post_uninstall"
	case "", "deploy":
		preHook, postHook = "pre_install", "post_install"
	default: // status 等只读相位不执行 hook
		return nil, nil, tasks
	}
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

// New 创建执行器。
func New(inv *inventory.Inventory, conns *connection.Manager, rep report.Reporter, opts Options) *Executor {
	if opts.Forks <= 0 {
		opts.Forks = 5
	}
	e := &Executor{Inv: inv, Conns: conns, Rep: rep, Opts: opts}
	e.engine = opts.Engine
	if e.engine == nil {
		e.engine = render.DefaultEngine()
	}
	e.facts = map[string]map[string]any{}
	return e
}

// Run 依次执行全部 play，返回是否存在失败。
func (e *Executor) Run(ctx context.Context, plays []*model.Play) bool {
	anyFail := false
	for _, p := range plays {
		if e.runPlay(ctx, p) {
			anyFail = true
		}
	}
	return anyFail
}

// LastStats 返回最近一次 run 的汇总统计（部署记录用）。
func (e *Executor) LastStats() map[string]*model.Stats {
	e.statsMu.Lock()
	defer e.statsMu.Unlock()
	if e.stats == nil {
		return map[string]*model.Stats{}
	}
	return e.stats
}

func (e *Executor) runPlay(ctx context.Context, p *model.Play) bool {
	e.mergeSubHandlers(p)
	hosts, err := e.selectHosts(p.Hosts)
	if err != nil {
		e.Rep.PlayMsg("选择主机失败: %v", err)
		return true
	}
	name := p.Name
	if name == "" {
		name = p.Hosts
	}
	if e.Opts.ListHosts {
		e.Rep.PlayStart(name, hostNames(hosts))
		return false
	}
	e.Rep.PlayStart(name, hostNames(hosts))

	stats := map[string]*model.Stats{}
	e.statsMu.Lock()
	e.stats = stats
	e.statsMu.Unlock()

	failed := false
	// 生命周期 hook 分离（pre/post 在策略批次之外、全部主机一批执行）；
	// 主任务列表按相位过滤（如 uninstall 时跳过 install hook 任务）
	preHooks, postHooks, mainTasks := splitHookTasks(p.Tasks, e.Opts.Phase)
	main := p
	if len(mainTasks) != len(p.Tasks) {
		mp := *p
		mp.Tasks = mainTasks
		main = &mp
	}

	// 自动回滚快照目录（play 级唯一，hook 变更同样登记）
	e.rollbackDir = ""
	if p.Strategy != nil && p.Strategy.AutoRollback {
		e.rollbackDir = fmt.Sprintf("/tmp/.wdp-rollback-%d", time.Now().UnixNano())
	}

	// 跨批次/跨 hook 的主机运行态延续（register/facts/回滚日志）
	st := newPlayState()
	var executedRuns []*hostRun // 已执行批次的主机运行态（快照清理用）

	runHooks := func(tasks []*model.Task, label string) bool {
		hp := *p
		hp.Name, hp.Tasks, hp.Serial = name+" "+label, tasks, ""
		hf, runs := e.runBatch(ctx, &hp, hosts, hosts, stats, st)
		executedRuns = append(executedRuns, runs...)
		return hf
	}

	if len(preHooks) > 0 && runHooks(preHooks, "[pre-hook]") {
		failed = true
		e.Rep.Recap(name, stats)
		e.Rep.PlayMsg("pre-hook 失败，终止 play")
		return true
	}

	// 策略分批：rolling/canary 按 batch 切批；canary 首批 1 台金丝雀；
	// linear（或未配置）维持传统 serial 语义
	var batches [][]*model.Host
	if stg := main.Strategy; stg != nil && (stg.Type == "rolling" || stg.Type == "canary") {
		size := parseBatchSize(stg.Batch, len(hosts))
		if stg.Type == "canary" && len(hosts) > 1 {
			batches = append(batches, hosts[:1])
			batches = append(batches, chunkHosts(hosts[1:], size)...)
		} else {
			batches = chunkHosts(hosts, size)
		}
	} else {
		batches = splitBatches(hosts, main.Serial)
	}

	for _, batch := range batches {
		batchFailed, runs := e.runBatch(ctx, main, hosts, batch, stats, st)
		executedRuns = append(executedRuns, runs...)
		if batchFailed {
			failed = true
		}
		if main.Strategy == nil {
			continue // 传统语义：批次失败不阻断后续批次
		}
		if batchFailed {
			if main.Strategy.AutoRollback {
				e.rollbackBatch(ctx, main, runs, stats)
			}
			e.Rep.PlayMsg("批次失败，终止后续批次（strategy=%s）", main.Strategy.Type)
			break
		}
		if main.Strategy.Gate != nil && e.runGate(ctx, main, main.Strategy.Gate, runs, stats) {
			failed = true
			if main.Strategy.AutoRollback {
				e.rollbackBatch(ctx, main, runs, stats)
			}
			e.Rep.PlayMsg("健康门未通过，终止后续批次")
			break
		}
	}

	// post-hook 仅在 play 成功后执行（post_install 语义）
	if !failed && len(postHooks) > 0 {
		if runHooks(postHooks, "[post-hook]") {
			failed = true
		}
	}

	e.Rep.Recap(name, stats)
	if e.Opts.CheckMode {
		e.Rep.PlayMsg("check 模式：changed 为变更预估（--diff 可看内容级差异）")
	}

	// 回滚快照清理：play 结束后 shadow 目录不再有用（成功批次保留变更，
	// 失败批次已回滚），只清理登记过变更的主机，best-effort 清除避免 /tmp 残留
	if e.rollbackDir != "" {
		e.cleanupSnapshots(ctx, executedRuns)
		e.rollbackDir = ""
	}

	// chart 生命周期 marker：deploy 成功后写 / uninstall 成功后清除
	if e.Opts.Chart != nil && !failed && !e.Opts.CheckMode {
		ch := e.Opts.Chart
		switch e.Opts.Phase {
		case "uninstall":
			e.removeMarkers(ctx, hosts, ch)
		case "", "deploy":
			e.writeMarkers(ctx, hosts, ch)
		}
	}
	return failed
}

// cleanupSnapshots 清除登记过回滚动作的主机上的快照目录（best-effort；
// 未产生变更的主机不建连）。
func (e *Executor) cleanupSnapshots(ctx context.Context, runs []*hostRun) {
	script := fmt.Sprintf("rm -rf -- %s", shellquote.Quote(e.rollbackDir))
	done := 0
	for _, hr := range runs {
		hr.mu.Lock()
		n := len(hr.journal)
		hr.mu.Unlock()
		if n == 0 {
			continue
		}
		conn, err := e.Conns.Get(ctx, hr.host)
		if err != nil {
			continue
		}
		if out, bad := conn.Exec(ctx, connection.ExecRequest{Script: script, TimeoutMs: 30_000}); bad == nil && out.Code == 0 {
			done++
		}
	}
	if done > 0 {
		e.Rep.PlayMsg("回滚快照已从 %d 台主机清理", done)
	}
}

// recordFacts 记录主机 facts（键级覆盖），供后续 play / 子 chart 作用域引用。
func (e *Executor) recordFacts(host string, facts map[string]any) {
	e.factsMu.Lock()
	defer e.factsMu.Unlock()
	cur, ok := e.facts[host]
	if !ok {
		cur = map[string]any{}
		e.facts[host] = cur
	}
	for k, v := range facts {
		cur[k] = v
	}
}

// seedFacts 把已积累的主机 facts 叠加进新建变量域（运行时数据覆盖静态层）。
func (e *Executor) seedFacts(host string, vars map[string]any) {
	e.factsMu.Lock()
	defer e.factsMu.Unlock()
	for k, v := range e.facts[host] {
		vars[k] = v
	}
}

// writeMarkers 部署成功后写 release marker（best-effort：失败仅告警不中断）。
func (e *Executor) writeMarkers(ctx context.Context, hosts []*model.Host, ch *chart.Chart) {
	if !ch.MarkerEnabled() {
		return
	}
	path := ch.MarkerPath()
	content := ch.MarkerContent(e.Opts.WdpVersion, e.Opts.Values)
	written := 0
	for _, h := range hosts {
		conn, err := e.Conns.Get(ctx, h)
		if err != nil {
			continue
		}
		if out, bad := conn.Exec(ctx, connection.ExecRequest{
			Script: fmt.Sprintf("mkdir -p -- %s", shellquote.Quote(pathDir(path))), TimeoutMs: 10_000,
		}); bad != nil || out.Code != 0 {
			continue
		}
		if err := conn.UploadFile(ctx, path, strings.NewReader(string(content)), 0o644); err == nil {
			written++
		}
	}
	if written > 0 {
		e.Rep.PlayMsg("release marker 已写入 %d 台主机: %s", written, path)
	} else if len(hosts) > 0 {
		e.Rep.PlayMsg("警告: release marker 写入失败（uninstall/status 将不可用）: %s", path)
	}
}

// removeMarkers 卸载成功后清除 release marker。
func (e *Executor) removeMarkers(ctx context.Context, hosts []*model.Host, ch *chart.Chart) {
	script := fmt.Sprintf("rm -rf -- %s", shellquote.Quote(pathDir(ch.MarkerPath())))
	done := 0
	for _, h := range hosts {
		conn, err := e.Conns.Get(ctx, h)
		if err != nil {
			continue
		}
		if out, bad := conn.Exec(ctx, connection.ExecRequest{Script: script, TimeoutMs: 10_000}); bad == nil && out.Code == 0 {
			done++
		}
	}
	if done > 0 {
		e.Rep.PlayMsg("release marker 已从 %d 台主机清除", done)
	}
}

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
		for _, subPlay := range sub.Deploy {
			for _, h := range subPlay.Handlers {
				if seen[h.Name] {
					e.Rep.PlayMsg("handler %q 重名，忽略子 chart %s 的同名 handler", h.Name, sub.Meta.Name)
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

// runBatch 在一批主机上执行 play 全部任务与 handlers，返回（是否失败，主机运行态）。
// st 延续跨批次/跨 hook 的运行态（register/facts/回滚日志）。
func (e *Executor) runBatch(ctx context.Context, p *model.Play, playHosts, hosts []*model.Host, stats map[string]*model.Stats, st *playState) (bool, []*hostRun) {
	runs := make([]*hostRun, 0, len(hosts))
	for _, h := range hosts {
		vars := map[string]any{}
		for k, v := range h.Vars { // inventory 层（最低）
			vars[k] = v
		}
		for k, v := range e.Opts.Values { // chart 合并 values（覆盖 inventory）
			vars[k] = v
		}
		for k, v := range p.Vars { // play vars（最高静态层）
			vars[k] = v
		}
		// 内置变量（强制注入，不可被任何静态层覆盖）：
		//   play_hosts 当前 play 全部选中主机 / play_batch 当前批次 /
		//   groups 组→成员（含 children 与 all）/ hosts 主机名→{name,address,port,conn}
		vars["play_hosts"] = hostNames(playHosts)
		vars["play_batch"] = hostNames(hosts)
		vars["groups"] = e.Inv.GroupsMap()
		vars["hosts"] = e.Inv.HostsMeta()
		st.seed(h.Name, vars) // 叠加此前批次的运行时变量（register/facts）
		e.seedFacts(h.Name, vars)
		runs = append(runs, &hostRun{
			host:       h,
			vars:       vars,
			chartScope: e.Opts.Values,
			baseDir:    e.Opts.BaseDir,
			alive:      true,
			notified:   map[string]bool{},
			stats:      &model.Stats{},
			journal:    st.takeJournal(h.Name),
		})
		if _, ok := stats[h.Name]; !ok {
			stats[h.Name] = &model.Stats{}
		}
	}
	// 批次结束（含全部提前返回路径）回收运行态
	defer st.harvest(runs)

	started := e.Opts.StartAtTask == ""
	taskFailed := false
	for _, task := range p.Tasks {
		if !started {
			if task.Label() == e.Opts.StartAtTask {
				started = true
			} else {
				continue
			}
		}
		if !taskSelected(task, e.Opts) {
			continue
		}
		e.Rep.TaskStart(task.Label(), task.Module)
		results := e.fanOut(ctx, p, task, runs)
		for _, r := range results {
			e.recordResult(r.hr, r.res, stats, task.IgnoreErrors)
			e.Rep.HostResult(r.hr.host.Name, r.res)
			if (r.res.Failed && !task.IgnoreErrors) || r.res.Unreachable {
				r.hr.alive = false
				taskFailed = true
			}
		}
		e.Rep.TaskDone()
		if !anyAlive(runs) {
			e.Rep.PlayMsg("全部主机不可用，终止本 play 剩余任务")
			return taskFailed, runs
		}
	}

	// flush handlers
	notifiedList := collectNotified(runs, p.Handlers)
	notified := map[string]bool{}
	for _, n := range notifiedList {
		notified[n] = true
	}
	if len(notifiedList) > 0 {
		e.Rep.PlayMsg("触发 handlers: %s", strings.Join(notifiedList, ", "))
		for _, h := range p.Handlers {
			if !notified[h.Name] {
				continue
			}
			e.Rep.TaskStart(h.Label()+" (handler)", h.Module)
			results := e.fanOut(ctx, p, h, runs)
			for _, r := range results {
				e.recordResult(r.hr, r.res, stats, false)
				e.Rep.HostResult(r.hr.host.Name, r.res)
				if r.res.Failed || r.res.Unreachable {
					r.hr.alive = false
					taskFailed = true
				}
			}
			e.Rep.TaskDone()
		}
	}
	return taskFailed, runs
}

// recordResult 计入统计（含子 chart 子任务路径）。
func (e *Executor) recordResult(hr *hostRun, r *model.TaskResult, stats map[string]*model.Stats, ignore bool) {
	if stats == nil {
		e.statsMu.Lock()
		stats = e.stats
		e.statsMu.Unlock()
	}
	if stats == nil {
		return
	}
	s, ok := stats[hr.host.Name]
	if !ok {
		s = &model.Stats{}
		stats[hr.host.Name] = s
	}
	switch {
	case r.Unreachable:
		s.Unreachable++
	case r.Failed:
		if ignore {
			s.Ignored++
		} else {
			s.Failed++
		}
	case r.Skipped:
		s.Skipped++
	case r.Changed:
		s.Changed++
	default:
		s.Ok++
	}
}

type fanResult struct {
	hr  *hostRun
	res *model.TaskResult
}

// fanOut 将任务并发派发到所有存活主机。
func (e *Executor) fanOut(ctx context.Context, p *model.Play, task *model.Task, runs []*hostRun) []fanResult {
	if task.ChartRef == "" && task.Block == nil {
		if _, ok := module.Get(task.Module); !ok && !e.hasScriptModule(runs, task.Module) {
			out := make([]fanResult, 0, len(runs))
			for _, hr := range runs {
				if !hr.alive {
					continue
				}
				out = append(out, fanResult{hr, &model.TaskResult{
					Host: hr.host.Name, Task: task.Label(), Module: task.Module,
					Failed: true, Msg: fmt.Sprintf("未知模块 %q", task.Module),
				}})
			}
			return out
		}
	}

	// run_once：整批只在一台存活主机执行，结果复制到全部存活主机
	// （register/notify 同步到各主机变量域与通知表）
	if task.RunOnce {
		return e.fanOutRunOnce(ctx, p, task, runs)
	}

	sem := make(chan struct{}, e.Opts.Forks)
	var wg sync.WaitGroup
	var mu sync.Mutex
	out := make([]fanResult, 0, len(runs))
	for _, hr := range runs {
		if !hr.alive {
			continue
		}
		wg.Add(1)
		go func(hr *hostRun) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			res := e.runTaskOnHost(ctx, p, task, hr)
			mu.Lock()
			out = append(out, fanResult{hr, res})
			mu.Unlock()
		}(hr)
	}
	wg.Wait()
	return out
}

// hasScriptModule 判断任务模块是否为 chart 本地脚本模块（fanOut 预检用）。
func (e *Executor) hasScriptModule(runs []*hostRun, name string) bool {
	if e.Opts.Chart == nil {
		return false
	}
	dirs := []string{e.Opts.BaseDir}
	for _, hr := range runs {
		if hr != nil && hr.baseDir != "" {
			dirs = append(dirs, hr.baseDir)
		}
	}
	return module.FindScriptModule(dirs, name) != ""
}

// fanOutRunOnce 处理 run_once：首台存活主机执行，结果复制到其余存活主机。
func (e *Executor) fanOutRunOnce(ctx context.Context, p *model.Play, task *model.Task, runs []*hostRun) []fanResult {
	var first *hostRun
	for _, hr := range runs {
		if hr.alive {
			first = hr
			break
		}
	}
	if first == nil {
		return nil
	}
	res := e.runTaskOnHost(ctx, p, task, first)
	out := []fanResult{{first, res}}
	// register 数据与 notify 触发同步到其余主机
	var data map[string]any
	if task.Register != "" {
		data = resultData(res)
	}
	for _, hr := range runs {
		if !hr.alive || hr == first {
			continue
		}
		if data != nil {
			hr.vars[task.Register] = data
		}
		if res.Changed && !res.Failed {
			for _, n := range task.Notify {
				hr.notified[n] = true
			}
		}
		c := *res
		c.Host = hr.host.Name
		out = append(out, fanResult{hr, &c})
	}
	return out
}

// runTaskOnHost 在单主机上执行任务（含 when/loop/register/retry，chart 任务递归展开）。
func (e *Executor) runTaskOnHost(ctx context.Context, p *model.Play, task *model.Task, hr *hostRun) *model.TaskResult {
	base := func() map[string]any {
		v := map[string]any{}
		for k, val := range hr.vars {
			v[k] = val
		}
		return v
	}

	res := &model.TaskResult{Host: hr.host.Name, Task: task.Label(), Module: task.Module}
	// 任务级展示控制（只影响回显，不影响 register 数据）
	res.Output = task.Output
	if task.NoLog {
		res.NoLog = true // 供 JSON 报告遮蔽（output=none 仅控制台回显语义）
		res.Output = "none"
	}
	start := time.Now()
	defer func() { res.ElapsedMs = time.Since(start).Milliseconds() }()

	// when（可引用 register 变量）
	if len(task.When) > 0 {
		vars := base()
		for _, cond := range task.When {
			s, err := e.engine.Render(cond, vars)
			if err != nil {
				return fail(res, err)
			}
			if !render.Truthy(s) {
				res.Skipped = true
				res.SkipReason = cond
				return res
			}
		}
	}

	// chart 引用任务：展开子 chart 任务序列
	if task.ChartRef != "" {
		return e.runChartTask(ctx, p, task, hr, res, base)
	}

	// block 组：顺序执行，失败转 rescue，always 恒执行
	if task.Block != nil {
		return e.runBlock(ctx, p, task, hr, res)
	}

	// 渲染参数（free-form 在 runOne 内按 item 上下文渲染，loop 场景需注入 item）
	vars := base()

	env := map[string]string{}
	for k, v := range p.Environment {
		rv, err := e.engine.Render(v, vars)
		if err != nil {
			return fail(res, err)
		}
		env[k] = rv
	}
	for k, v := range task.Environment {
		rv, err := e.engine.Render(v, vars)
		if err != nil {
			return fail(res, err)
		}
		env[k] = rv
	}

	become := p.Become
	if task.Become != nil {
		become = *task.Become
	}
	becomeUser := firstNonEmpty(task.BecomeUser, p.BecomeUser)

	// delegate_to：任务改在指定主机（或 localhost）上执行，
	// 变量域保持原主机（inventory_hostname 不变），结果归属原主机
	execHost := hr.host
	if task.DelegateTo != "" {
		s, err := e.engine.Render(task.DelegateTo, vars)
		if err != nil {
			return fail(res, err)
		}
		if s == "localhost" {
			execHost = e.localhost()
		} else if dh := e.Inv.HostByName(s); dh != nil {
			execHost = dh
		} else {
			return fail(res, fmt.Errorf("delegate_to 目标主机 %q 不存在于 inventory", s))
		}
		res.DelegateTo = s
	}

	// 循环变量名（loop_control.loop_var，缺省 item；嵌套 loop 用自定义名区分层级）
	loopVar := firstNonEmpty(task.LoopVar, "item")

	runOne := func(item any) *model.TaskResult {
		// 任务级超时（0/-1 不限；task.TimeoutSec 覆盖全局默认）
		timeoutSec := task.TimeoutSec
		if timeoutSec == 0 {
			timeoutSec = e.Opts.TaskTimeout
		}
		taskCtx := ctx
		if timeoutSec > 0 {
			var cancel context.CancelFunc
			taskCtx, cancel = context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second)
			defer cancel()
		}

		itemVars := base()
		if item != nil {
			itemVars[loopVar] = item
		}
		iargs, err := e.engine.RenderValue(task.Args, itemVars)
		if err != nil {
			return fail(res, err)
		}
		args, _ := iargs.(map[string]any)
		free := task.FreeForm
		if free != "" {
			free, err = e.engine.Render(free, itemVars)
			if err != nil {
				return fail(res, err)
			}
		}
		conn, err := e.Conns.Get(taskCtx, execHost)
		if err != nil {
			r := cloneRes(res)
			r.Unreachable = true
			r.Msg = err.Error()
			return r
		}
		mod, ok := module.Get(task.Module)
		scriptPath := ""
		if !ok {
			// chart 本地脚本模块（modules/<名>）回退
			if e.Opts.Chart != nil {
				scriptPath = module.FindScriptModule([]string{hr.baseDir, e.Opts.BaseDir}, task.Module)
			}
			if scriptPath == "" {
				return fail(res, fmt.Errorf("未知模块 %q", task.Module))
			}
		}
		rc := &module.RunContext{
			Ctx:        taskCtx,
			Conn:       conn,
			Host:       execHost,
			Vars:       itemVars,
			Env:        env,
			Become:     become,
			BecomeUser: becomeUser,
			BaseDir:    hr.baseDir,
			Engine:     e.engine,
			TimeoutMs:  int64(timeoutSec) * 1000,
			CheckMode:  e.Opts.CheckMode,
			DiffMode:   e.Opts.DiffMode,
		}
		// auto_rollback：注入变更日志（文件类模块登记快照/删除动作）
		if p.Strategy != nil && p.Strategy.AutoRollback && !e.Opts.CheckMode && e.rollbackDir != "" {
			rc.Rollback = &module.RollbackCtx{
				Dir: e.rollbackDir,
				Record: func(a module.RollbackAction) {
					hr.mu.Lock()
					hr.journal = append(hr.journal, a)
					hr.mu.Unlock()
				},
			}
		}
		// 模块调用统一入口：内置注册表优先，chart 脚本模块回退
		invoke := func() *module.Result {
			if scriptPath != "" {
				return module.RunScriptModule(rc, scriptPath, args, free)
			}
			return mod.Run(rc, args, free)
		}
		r := cloneRes(res)
		applyModule := func(mr *module.Result) {
			r.Changed = mr.Changed
			r.Failed = mr.Failed
			r.Msg = mr.Msg
			r.Stdout = truncateOut(mr.Stdout)
			r.Stderr = truncateOut(mr.Stderr)
			r.Rc = mr.Rc
			// no_log 任务不携带 diff（内容级差异即敏感内容本身；register 数据不受影响）
			if !r.NoLog {
				r.Diff = mr.Diff
			}
			if mr.Facts != nil {
				hr.mu.Lock()
				for k, v := range mr.Facts {
					hr.vars[k] = v
				}
				hr.mu.Unlock()
				e.recordFacts(hr.host.Name, mr.Facts) // 跨 play / 子 chart 作用域持久
			}
			if len(mr.Groups) > 0 {
				// group_by：动态组聚合进 inventory（下一批次/play 的选择期可见）
				e.invMu.Lock()
				for _, g := range mr.Groups {
					e.Inv.AddDynamicGroup(g, []string{hr.host.Name})
				}
				e.invMu.Unlock()
			}
		}

		if task.Until != "" {
			// until 轮询：retries=总尝试上限（默认 3），delay=轮询间隔秒数（0 不等待）；
			// 条件满足采用当轮结果；耗尽则按失败计（即使模块本身成功）。
			attempts := task.Retries
			if attempts <= 0 {
				attempts = 3
			}
			delay := task.DelaySec
			var last *module.Result
			for i := 0; i < attempts; i++ {
				if i > 0 {
					select {
					case <-taskCtx.Done():
						r.Unreachable = true
						r.Msg = taskCtx.Err().Error()
						return r
					case <-time.After(time.Duration(delay) * time.Second):
					}
				}
				last = invoke()
				judge := map[string]any{}
				for k, v := range itemVars {
					judge[k] = v
				}
				judge["result"] = moduleData(last)
				s, err := e.engine.Render(task.Until, judge)
				if err != nil {
					applyModule(last)
					r.Failed = true
					r.Msg = "until 渲染失败: " + err.Error()
					return r
				}
				if render.Truthy(s) {
					applyModule(last)
					return r
				}
			}
			applyModule(last)
			r.Failed = true
			r.Msg = fmt.Sprintf("until 条件在 %d 次尝试后仍未满足", attempts)
			if last != nil && last.Msg != "" {
				r.Msg += ": " + last.Msg
			}
			return r
		}

		// 失败重试（无 until：成功即停）
		attempts := task.Retries + 1
		if attempts < 1 {
			attempts = 1
		}
		for i := 0; i < attempts; i++ {
			if i > 0 && task.DelaySec > 0 {
				select {
				case <-taskCtx.Done():
					r.Unreachable = true
					r.Msg = taskCtx.Err().Error()
					return r
				case <-time.After(time.Duration(task.DelaySec) * time.Second):
				}
			}
			mr := invoke()
			applyModule(mr)
			if !mr.Failed {
				break
			}
		}
		return r
	}

	var loopResults []*model.TaskResult
	if task.Loop != nil {
		lv, err := e.engine.RenderValue(task.Loop, vars)
		if err != nil {
			return fail(res, err)
		}
		items, _ := lv.([]any)
		for _, item := range items {
			lr := runOne(item)
			if item != nil {
				lr.Item = fmt.Sprint(item)
			}
			loopResults = append(loopResults, lr)
		}
		for _, lr := range loopResults {
			if lr.Unreachable {
				res.Unreachable = true
				res.Msg = lr.Msg
				return res
			}
			if lr.Failed {
				res.Failed = true
			}
			if lr.Changed {
				res.Changed = true
			}
			if lr.Stdout != "" {
				res.Stdout += lr.Stdout
			}
			if lr.Stderr != "" {
				res.Stderr += lr.Stderr
			}
		}
		if len(loopResults) > 0 {
			last := loopResults[len(loopResults)-1]
			res.Rc, res.Msg = last.Rc, last.Msg
			res.Items = loopResults // 逐项结果（-vv 展示 / JSON 记录）
		}
	} else {
		r := runOne(nil)
		*res = *r
	}
	res.Task = task.Label()
	res.Module = task.Module
	res.Host = hr.host.Name

	// changed_when / failed_when 覆盖判定
	if task.ChangedWhen != "" || task.FailedWhen != "" {
		judge := base()
		judge["result"] = resultData(res)
		if task.ChangedWhen != "" {
			s, err := e.engine.Render(task.ChangedWhen, judge)
			if err != nil {
				return fail(res, err)
			}
			res.Changed = render.Truthy(s)
		}
		if task.FailedWhen != "" {
			s, err := e.engine.Render(task.FailedWhen, judge)
			if err != nil {
				return fail(res, err)
			}
			res.Failed = render.Truthy(s)
		}
	}

	// register
	if task.Register != "" {
		data := resultData(res)
		if len(loopResults) > 0 {
			list := make([]any, len(loopResults))
			for i, lr := range loopResults {
				list[i] = resultData(lr)
			}
			data["results"] = list
		}
		hr.vars[task.Register] = data
	}

	// notify
	if res.Changed && !res.Failed && len(task.Notify) > 0 {
		for _, n := range task.Notify {
			hr.notified[n] = true
		}
	}
	return res
}

// runChartTask 展开执行子 chart 任务序列（作用域隔离：子树 + global + 引用 vars）。
func (e *Executor) runChartTask(ctx context.Context, p *model.Play, task *model.Task, hr *hostRun, res *model.TaskResult, base func() map[string]any) *model.TaskResult {
	if e.Opts.Chart == nil {
		res.Failed = true
		res.Msg = "chart 引用仅在 chart 模式下可用（wdp run <chart目录|tgz>）"
		return res
	}
	sub, serr := e.Opts.Chart.ResolveSub(task.ChartRef)
	if sub == nil {
		res.Failed = true
		res.Msg = serr.Error()
		return res
	}
	var subPlay *model.Play
	if len(sub.Deploy) == 1 {
		subPlay = sub.Deploy[0]
	} else {
		subPlay = &model.Play{}
	}

	// 子 chart 作用域 values（低 → 高）：子 chart 默认 values → 父作用域 <子chart名> 子树
	// → global（跨层共享）→ 引用 vars
	scope := map[string]any{}
	for k, v := range sub.Values {
		scope[k] = v
	}
	if tree, ok := hr.chartScope[sub.Meta.Name].(map[string]any); ok {
		scope = chart.Merge(scope, tree)
	}
	if g, ok := hr.chartScope["global"].(map[string]any); ok {
		gc := map[string]any{}
		for k, v := range g {
			gc[k] = v
		}
		scope["global"] = gc
	}
	if task.ChartVars != nil {
		cv, err := e.engine.RenderValue(task.ChartVars, base())
		if err != nil {
			return fail(res, err)
		}
		if m, ok := cv.(map[string]any); ok {
			for k, v := range m {
				scope[k] = v
			}
		}
	}

	// loop 项
	items := []any{nil}
	if task.Loop != nil {
		lv, err := e.engine.RenderValue(task.Loop, base())
		if err != nil {
			return fail(res, err)
		}
		if l, ok := lv.([]any); ok {
			items = l
		}
	}

	// 构建子 chart 有效 play（hosts/become/strategy 继承父）
	effPlay := *subPlay
	effPlay.Hosts = p.Hosts
	effPlay.Serial = ""
	effPlay.Vars = nil
	if effPlay.Strategy == nil {
		effPlay.Strategy = p.Strategy // 父策略（含 auto_rollback 变更日志）对子任务生效
	}
	if !effPlay.Become {
		effPlay.Become = p.Become
	}
	if effPlay.BecomeUser == "" {
		effPlay.BecomeUser = p.BecomeUser
	}
	if len(p.Environment) > 0 {
		merged := map[string]string{}
		for k, v := range p.Environment {
			merged[k] = v
		}
		for k, v := range effPlay.Environment {
			merged[k] = v
		}
		effPlay.Environment = merged
	}

	// 保存外层状态，切换作用域
	savedVars, savedScope, savedBase := hr.vars, hr.chartScope, hr.baseDir
	defer func() {
		hr.vars, hr.chartScope, hr.baseDir = savedVars, savedScope, savedBase
	}()
	hr.baseDir = sub.Dir
	hr.chartScope = scope

	var msgs []string
	for _, item := range items {
		// 子任务域 = host 基础变量 + 子作用域 values + 子 play vars + item
		vars := map[string]any{}
		for k, v := range hr.host.Vars {
			vars[k] = v
		}
		for k, v := range scope {
			vars[k] = v
		}
		for k, v := range subPlay.Vars {
			vars[k] = v
		}
		// 内置变量穿透子 chart 作用域（play_hosts/groups 等在组件内同样可用）
		for _, k := range builtinVars {
			if v, ok := savedVars[k]; ok {
				vars[k] = v
			}
		}
		// 主机 facts（setup/stat 等运行时数据）同样穿透：属于主机而非 chart 作用域
		e.seedFacts(hr.host.Name, vars)
		if item != nil {
			vars[firstNonEmpty(task.LoopVar, "item")] = item
		}
		hr.vars = vars

		itemFailed := false
		for _, t := range subPlay.Tasks {
			if !taskSelected(t, e.Opts) {
				continue
			}
			// 子任务不发独立 TaskStart/HostResult（大规模主机下会刷屏），
			// 结果聚合进 chart 任务：异常带子任务名前缀。
			r := e.runTaskOnHost(ctx, &effPlay, t, hr)
			e.recordResult(hr, r, nil, t.IgnoreErrors)
			if r.Unreachable {
				res.Unreachable = true
				res.Msg = r.Msg
				return res
			}
			if r.Failed {
				res.Failed = true
				if !t.IgnoreErrors {
					itemFailed = true
				}
				msgs = append(msgs, fmt.Sprintf("%s: %s", t.Label(), r.Msg))
			}
			if r.Changed {
				res.Changed = true
			}
			if r.Stdout != "" {
				res.Stdout += r.Stdout
			}
			if itemFailed {
				break // 该 item 的子序列中断，继续下一 item
			}
		}
	}
	if res.Msg == "" {
		res.Msg = fmt.Sprintf("chart %s: %d 项执行完成", sub.Meta.Name, len(items))
		if res.Failed {
			res.Msg = strings.Join(msgs, "; ")
		}
	}
	res.Task = task.Label()
	res.Module = "chart"
	res.Host = hr.host.Name

	if task.Register != "" {
		savedVars[task.Register] = resultData(res)
	}
	if res.Changed && !res.Failed && len(task.Notify) > 0 {
		for _, n := range task.Notify {
			hr.notified[n] = true
		}
	}
	return res
}

// ---- 辅助 ----

func (e *Executor) selectHosts(pattern string) ([]*model.Host, error) {
	hosts, err := e.Inv.Select(pattern)
	if err != nil {
		return nil, err
	}
	if e.Opts.Limit == "" {
		return hosts, nil
	}
	limited, err := e.Inv.Select(e.Opts.Limit)
	if err != nil {
		return nil, err
	}
	set := map[string]bool{}
	for _, h := range limited {
		set[h.Name] = true
	}
	out := []*model.Host{}
	for _, h := range hosts {
		if set[h.Name] {
			out = append(out, h)
		}
	}
	return out, nil
}

func taskSelected(t *model.Task, opts Options) bool {
	if len(opts.Tags) > 0 {
		for _, want := range opts.Tags {
			for _, has := range t.Tags {
				if has == want {
					return true
				}
			}
		}
		return false
	}
	for _, skip := range opts.SkipTags {
		for _, has := range t.Tags {
			if has == skip {
				return false
			}
		}
	}
	return true
}

func collectNotified(runs []*hostRun, handlers []*model.Task) []string {
	set := map[string]bool{}
	for _, hr := range runs {
		if !hr.alive {
			continue
		}
		for n := range hr.notified {
			set[n] = true
		}
	}
	var out []string
	for _, h := range handlers {
		if set[h.Name] {
			out = append(out, h.Name)
		}
	}
	sort.Strings(out)
	return out
}

// splitBatches 按 serial 表达式分批："5"（每批 5 台）/"10%"（百分比）/
// "5,10,20"（逐批尺寸，最后一个尺寸对剩余主机重复）；空 = 一批。
func splitBatches(hosts []*model.Host, serial string) [][]*model.Host {
	if serial == "" {
		return [][]*model.Host{hosts}
	}
	var sizes []int
	for _, t := range strings.Split(serial, ",") {
		sizes = append(sizes, parseBatchSize(t, len(hosts)))
	}
	if len(sizes) == 0 {
		return [][]*model.Host{hosts}
	}
	var out [][]*model.Host
	for i := 0; i < len(hosts); {
		size := sizes[len(sizes)-1] // 最后一个尺寸对剩余主机重复使用
		if len(out) < len(sizes) {
			size = sizes[len(out)]
		}
		end := i + size
		if end > len(hosts) {
			end = len(hosts)
		}
		out = append(out, hosts[i:end])
		i = end
	}
	return out
}

func anyAlive(runs []*hostRun) bool {
	for _, hr := range runs {
		if hr.alive {
			return true
		}
	}
	return false
}

func hostNames(hosts []*model.Host) []string {
	out := make([]string, len(hosts))
	for i, h := range hosts {
		out[i] = h.Name
	}
	return out
}

// builtinVars 是强制注入的内置变量名（子 chart 作用域穿透清单）。
var builtinVars = []string{
	"inventory_hostname", "group_names", "play_hosts", "play_batch", "groups", "hosts",
}

func resultData(r *model.TaskResult) map[string]any {
	return map[string]any{
		"changed": r.Changed,
		"failed":  r.Failed,
		"rc":      r.Rc,
		"stdout":  r.Stdout,
		"stderr":  r.Stderr,
		"msg":     r.Msg,
		"skipped": r.Skipped,
	}
}

// moduleData 构造 until/changed_when 判断用的模块结果域。
func moduleData(mr *module.Result) map[string]any {
	if mr == nil {
		return map[string]any{}
	}
	return map[string]any{
		"changed": mr.Changed,
		"failed":  mr.Failed,
		"rc":      mr.Rc,
		"stdout":  mr.Stdout,
		"stderr":  mr.Stderr,
		"msg":     mr.Msg,
	}
}

// runBlock 执行 block/rescue/always 任务组（单主机内顺序，支持嵌套）。
func (e *Executor) runBlock(ctx context.Context, p *model.Play, task *model.Task, hr *hostRun, res *model.TaskResult) *model.TaskResult {
	runSeq := func(tasks []*model.Task) (failed bool, unreachable bool, msgs []string) {
		for _, t := range tasks {
			r := e.runTaskOnHost(ctx, p, t, hr)
			e.recordResult(hr, r, nil, t.IgnoreErrors)
			if r.Changed {
				res.Changed = true
			}
			if r.Unreachable {
				return false, true, append(msgs, r.Msg)
			}
			if r.Failed {
				failed = true
				msgs = append(msgs, fmt.Sprintf("%s: %s", t.Label(), r.Msg))
				if !t.IgnoreErrors {
					return true, false, msgs // block 内失败即转 rescue
				}
			}
		}
		return failed, false, msgs
	}

	blockFailed, unreachable, msgs := runSeq(task.Block)
	if unreachable {
		res.Unreachable = true
		res.Msg = strings.Join(msgs, "; ")
		return res
	}

	if blockFailed {
		// 注入失败信息供 rescue 引用
		hr.vars["block_failed"] = true
		hr.vars["block_failed_msgs"] = strings.Join(msgs, "; ")
		if len(task.Rescue) == 0 {
			res.Failed = true
			res.Msg = "block 失败: " + strings.Join(msgs, "; ")
		} else {
			rescueFailed, resUnreachable, rmsgs := runSeq(task.Rescue)
			if resUnreachable {
				res.Unreachable = true
				res.Msg = strings.Join(rmsgs, "; ")
				return res
			}
			if rescueFailed {
				res.Failed = true
				res.Msg = "block 失败且 rescue 失败: " + strings.Join(append(msgs, rmsgs...), "; ")
			} else {
				// rescue 成功兜底：组视为已恢复（changed 保持）
				res.Msg = "block 失败已由 rescue 恢复: " + strings.Join(msgs, "; ")
				delete(hr.vars, "block_failed")
			}
		}
	}

	if _, unreach, amsgs := runSeq(task.Always); unreach {
		res.Unreachable = true
		res.Msg = strings.Join(amsgs, "; ")
	}
	res.Task = task.Label()
	res.Module = "block"
	res.Host = hr.host.Name
	return res
}

func cloneRes(r *model.TaskResult) *model.TaskResult {
	n := *r
	return &n
}

// maxOutLen 是单任务捕获输出的上限（防 cat 大文件撑爆内存）。
const maxOutLen = 1 << 20

// truncateOut 截断超长输出并标注。
func truncateOut(s string) string {
	if len(s) <= maxOutLen {
		return s
	}
	return s[:maxOutLen] + fmt.Sprintf("\n…[wdp] 输出超长已截断（%d 字节）", len(s))
}

func fail(res *model.TaskResult, err error) *model.TaskResult {
	res.Failed = true
	res.Msg = err.Error()
	return res
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// localOnce 复用的 localhost 主机（delegate_to: localhost 目标）。
var localHost = &model.Host{Name: "localhost", Conn: "local"}

func (e *Executor) localhost() *model.Host { return localHost }
