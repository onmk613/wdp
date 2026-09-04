package executor

import (
	"context"
	"time"

	"wdp/internal/model"
)

// play 推进与生命周期（hook 拆分、批次策略、收尾清理）。

func (e *Executor) runPlay(ctx context.Context, p *model.Play) bool {
	e.mergeSubHandlers(p)
	hosts, err := e.selectHosts(p.Hosts)
	if err != nil {
		e.Rep.PlayMsg("failed to select hosts: %v", err)
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
		e.Rep.PlayMsg("pre-hook failed, aborting play")
		// 与正常路径同样走 finishPlay：清理回滚快照目录、跳过 marker 写入
		e.finishPlay(ctx, name, stats, failed, executedRuns, hosts)
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
// 配置了 strategy（linear/rolling/canary）时按 batch 切批、批次失败即终止
// 后续批次（canary 首批 1 台金丝雀；linear 与 rolling 同为按批线性推进）；
// 未配置 strategy 时维持传统 serial 语义（批次失败不阻断后续批次）。
func (e *Executor) runMainBatches(ctx context.Context, main *model.Play, hosts []*model.Host, stats map[string]*model.Stats, st *playState) (bool, []*hostRun) {
	var batches [][]*model.Host
	if stg := main.Strategy; stg != nil {
		size := parseBatchSize(stg.Batch, len(hosts))
		if stg.Type == "canary" && len(hosts) > 1 {
			batches = append(batches, hosts[:1])
			batches = append(batches, chunkHosts(hosts[1:], size)...)
		} else {
			batches = chunkHosts(hosts, size)
		}
	} else {
		var err error
		batches, err = splitBatches(hosts, main.Serial)
		if err != nil {
			e.Rep.PlayMsg("batch split failed: %v", err)
			return true, nil
		}
	}

	failed := false
	var executed []*hostRun
	for _, batch := range batches {
		if ctx.Err() != nil {
			e.Rep.PlayMsg("execution cancelled (%v), terminating remaining batches", ctx.Err())
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
				e.rollbackBatch(ctx, runs, stats)
			}
			e.Rep.PlayMsg("batch failed, aborting subsequent batches (strategy=%s)", main.Strategy.Type)
			break
		}
		if main.Strategy.Gate != nil && e.runGate(ctx, main, main.Strategy.Gate, runs, stats) {
			failed = true
			if main.Strategy.AutoRollback {
				e.rollbackBatch(ctx, runs, stats)
			}
			e.Rep.PlayMsg("health gate not passed, terminating remaining batches")
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
		e.Rep.PlayMsg("check mode: changed is a change estimate (use --diff to see content-level diffs)")
	}
	// 回滚快照清理：play 结束后 shadow 目录不再有用（成功批次保留变更，
	// 失败批次已回滚），只清理登记过变更的主机，best-effort 清除避免 /tmp 残留。
	// 用独立的限时 ctx：执行取消（Ctrl+C）时父 ctx 已失效，若沿用会静默跳过清理
	if e.rollbackDir != "" {
		cleanCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		e.cleanupSnapshots(cleanCtx, executedRuns)
		cancel()
		e.rollbackDir = ""
	}
	if e.Opts.Chart != nil && !failed && !e.Opts.CheckMode {
		// marker 处置按相位属性：uninstall 等清除；deploy 及声明 release 的
		// 相位（如 update）写入；其余相位不动 marker
		spec := e.Opts.Chart.PhaseSpecFor(e.Opts.Phase)
		switch {
		case spec.ClearsMarker:
			e.removeMarkers(ctx, hosts, e.Opts.Chart)
		case spec.Release:
			e.writeMarkers(ctx, hosts, e.Opts.Chart)
		}
	}
}
