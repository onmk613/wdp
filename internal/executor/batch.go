package executor

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"wdp/internal/model"
	"wdp/internal/module"
)

// 批次执行：runBatch 驱动一批主机跑完 play 任务，fanOut 以 forks 并发派发任务。

// runBatch 在一批主机上执行 play 全部任务与 handlers，返回（是否失败，主机运行态）。
// st 延续跨批次/跨 hook 的运行态（register/facts/回滚日志）。
func (e *Executor) runBatch(ctx context.Context, p *model.Play, playHosts, hosts []*model.Host, stats map[string]*model.Stats, st *playState) (bool, []*hostRun) {
	runs := e.prepareBatchRuns(p, playHosts, hosts, stats, st)
	// 批次结束（含全部提前返回路径）回收运行态
	defer st.harvest(runs)

	taskFailed, stop := e.runBatchTasks(ctx, p, runs, stats)
	if !stop && e.flushHandlers(ctx, p, runs, stats) {
		taskFailed = true
	}
	return taskFailed, runs
}

// hostValuesFor 返回主机实际生效的 chart values：HostValues 有该主机条目
// 时用它（非部署相位从 marker 还原），否则用全局 Values。
func (e *Executor) hostValuesFor(host string) map[string]any {
	if v, ok := e.Opts.HostValues[host]; ok && v != nil {
		return v
	}
	return e.Opts.Values
}

// prepareBatchRuns 组装批次内各主机的运行态（变量分层合并 + 内置变量注入）
// 并初始化统计条目。
func (e *Executor) prepareBatchRuns(p *model.Play, playHosts, hosts []*model.Host, stats map[string]*model.Stats, st *playState) []*hostRun {
	runs := make([]*hostRun, 0, len(hosts))
	for _, h := range hosts {
		hostVals := e.hostValuesFor(h.Name)
		vars := map[string]any{}
		// inventory 层（最低）
		maps.Copy(vars, h.Vars)
		if frozen, ok := e.Opts.PlanVars[h.Name]; ok && frozen != nil {
			// plan 执行模式：编译期冻结的完整变量域（inventory vars + values
			// + play vars + 内置变量快照）整体胜出——跨主机信息已固化，不再
			// 依赖运行期 inventory
			maps.Copy(vars, frozen)
		} else {
			// chart 合并 values（覆盖 inventory：默认同名时 values 恒赢）
			maps.Copy(vars, hostVals)
			// inventory_override 白名单（chart.yaml 显式 opt-in）：列入的键
			// 允许 inventory 同名变量反超 values（ansible 式语义按 chart 领取）。
			// 仅顶层 values 域生效；play vars 及以上层级不受影响。
			if e.Opts.Chart != nil {
				for _, k := range e.Opts.Chart.Meta.InventoryOverride {
					if v, ok := h.Vars[k]; ok {
						vars[k] = v
					}
				}
			}
			// play vars（最高静态层）
			maps.Copy(vars, p.Vars)
		}
		st.seed(h.Name, vars) // 叠加此前批次的运行时变量（register/facts）
		e.seedFacts(h.Name, vars)
		// 内置变量最后强制注入：plan 模式只注入运行期才知道的三项
		//（BaseDir 是物化目录、批次是执行期事实），其余（groups/hosts/
		// hostvars/inventory_hostname）保留冻结值
		if e.Opts.PlanVars != nil {
			e.injectPlanBuiltins(vars, h, playHosts, hosts)
		} else {
			e.injectBuiltins(vars, h, playHosts, hosts)
		}
		runs = append(runs, &hostRun{
			host:       h,
			vars:       vars,
			chartScope: hostVals,
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
	return runs
}

// runBatchTasks 顺序执行批次任务列表（含 --start-at-task 定位与 tag 过滤）。
// 返回（是否失败，是否终止批次）：stop=true 时调用方跳过 handler 冲洗直接返回
// （执行取消或全部主机不可达）。
func (e *Executor) runBatchTasks(ctx context.Context, p *model.Play, runs []*hostRun, stats map[string]*model.Stats) (bool, bool) {
	started := e.Opts.StartAtTask == ""
	taskFailed := false
	for _, task := range p.Tasks {
		if ctx.Err() != nil {
			e.Rep.PlayMsg("execution cancelled (%v), terminating remaining tasks", ctx.Err())
			return true, true
		}
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
				e.markDead(r.hr.host.Name)
			}
		}
		e.Rep.TaskDone()
		if !anyAlive(runs) {
			e.Rep.PlayMsg("all hosts unavailable, terminating remaining tasks of this play")
			return taskFailed, true
		}
	}
	return taskFailed, false
}

// flushHandlers 冲洗 handlers：每个 handler 只在通知了它的主机上执行
// （h2 未变更则不在 h2 重启服务）。返回 handler 是否存在失败。
func (e *Executor) flushHandlers(ctx context.Context, p *model.Play, runs []*hostRun, stats map[string]*model.Stats) bool {
	notifiedList, notifiedByHost := collectNotified(runs, p.Handlers)
	if len(notifiedList) == 0 {
		return false
	}
	failed := false
	e.Rep.PlayMsg("triggering handlers: %s", strings.Join(notifiedList, ", "))
	for _, h := range p.Handlers {
		if !slices.Contains(notifiedList, h.Name) {
			continue
		}
		targets := make([]*hostRun, 0, len(runs))
		for _, hr := range runs {
			if notifiedByHost[hr.host.Name][h.Name] && hr.alive {
				targets = append(targets, hr)
			}
		}
		e.Rep.TaskStart(h.Label()+" (handler)", h.Module)
		results := e.fanOut(ctx, p, h, targets)
		for _, r := range results {
			e.recordResult(r.hr, r.res, stats, false)
			e.Rep.HostResult(r.hr.host.Name, r.res)
			if r.res.Failed || r.res.Unreachable {
				r.hr.alive = false
				failed = true
				e.markDead(r.hr.host.Name)
			}
		}
		e.Rep.TaskDone()
	}
	return failed
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
		if _, _, ok := module.Resolve(task.Module, e.allScriptModuleDirs(runs)); !ok {
			out := make([]fanResult, 0, len(runs))
			for _, hr := range runs {
				if !hr.alive {
					continue
				}
				out = append(out, fanResult{hr, &model.TaskResult{
					Host: hr.host.Name, Task: task.Label(), Module: task.Module,
					Failed: true, Msg: fmt.Sprintf("unknown module %q", task.Module),
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

// allScriptModuleDirs 汇总批次内全部主机的脚本模块查找目录（chart 根 + 各 playbook 目录）。
func (e *Executor) allScriptModuleDirs(runs []*hostRun) []string {
	if e.Opts.Chart == nil {
		return nil
	}
	dirs := []string{e.Opts.BaseDir}
	for _, hr := range runs {
		if hr != nil && hr.baseDir != "" {
			dirs = append(dirs, hr.baseDir)
		}
	}
	return dirs
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
