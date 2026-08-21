package executor

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"wdp/internal/chart"
	"wdp/internal/connection"
	"wdp/internal/model"
	"wdp/internal/shellquote"
)

// play 推进与生命周期辅助（hook 拆分、marker 写入、facts、handlers 合并）。

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
		e.rollbackDir = "/tmp/.wdp-rollback-" + randSuffix()
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

	batchFailed, batchRuns := e.runMainBatches(ctx, main, hosts, stats, st)
	if batchFailed {
		failed = true
	}
	executedRuns = append(executedRuns, batchRuns...)

	// post-hook 仅在 play 成功后执行（post_install 语义）
	if !failed && len(postHooks) > 0 {
		if runHooks(postHooks, "[post-hook]") {
			failed = true
		}
	}

	e.finishPlay(ctx, name, stats, failed, executedRuns, hosts)
	return failed
}

// runMainBatches 按策略切批执行主任务列表，返回（是否失败，已执行主机运行态）。
// rolling/canary 按 batch 切批（canary 首批 1 台金丝雀）；
// linear（或未配置）维持传统 serial 语义。
func (e *Executor) runMainBatches(ctx context.Context, main *model.Play, hosts []*model.Host, stats map[string]*model.Stats, st *playState) (bool, []*hostRun) {
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

	failed := false
	var executed []*hostRun
	for _, batch := range batches {
		if ctx.Err() != nil {
			e.Rep.PlayMsg("执行已取消（%v），终止剩余批次", ctx.Err())
			failed = true
			break
		}
		batchFailed, runs := e.runBatch(ctx, main, hosts, batch, stats, st)
		executed = append(executed, runs...)
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
	return failed, executed
}

// finishPlay 收尾：RECAP、回滚快照清理与 chart 生命周期 marker
// （deploy 成功后写 / uninstall 成功后清除）。
func (e *Executor) finishPlay(ctx context.Context, name string, stats map[string]*model.Stats, failed bool, executedRuns []*hostRun, hosts []*model.Host) {
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
	if e.Opts.Chart != nil && !failed && !e.Opts.CheckMode {
		switch e.Opts.Phase {
		case "uninstall":
			e.removeMarkers(ctx, hosts, e.Opts.Chart)
		case "", "deploy":
			e.writeMarkers(ctx, hosts, e.Opts.Chart)
		}
	}
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
	sort.Strings(out)
	return out, byHost
}
