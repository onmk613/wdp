package executor

import (
	"context"
	"fmt"
	"strings"
	"wdp/internal/chart"
	"wdp/internal/model"
)

// 任务展开：子 chart 引用（runChartTask）与 block/rescue/always 任务组（runBlock）。

// runChartTask 展开执行子 chart 任务序列（作用域隔离：子树 + global + 引用 vars）。
func (e *Executor) runChartTask(ctx context.Context, p *model.Play, task *model.Task, hr *hostRun, res *model.TaskResult, base func() map[string]any) *model.TaskResult {
	if e.Opts.Chart == nil {
		res.Failed = true
		res.Msg = "chart 引用仅在 chart 模式下可用（需以 chart 目录或 tgz 包作为入口运行）"
		return res
	}
	// 环引用防护：chart 自引用/互引用会在此递归展开中无限下钻，
	// 超过深度上限即报错终止（Go 栈溢出无法 recover，必须前置拦截）。
	if hr.chartDepth >= maxChartDepth {
		res.Failed = true
		res.Msg = fmt.Sprintf("chart 引用展开超过深度上限 %d（可能存在环引用: %s）", maxChartDepth, task.ChartRef)
		return res
	}
	hr.chartDepth++
	defer func() { hr.chartDepth-- }()
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
	scope := chart.SubScope(sub, hr.chartScope)
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
		l, err := e.renderLoopItems(task.Loop, base())
		if err != nil {
			return fail(res, err)
		}
		items = l
	}

	effPlay := effSubPlay(p, subPlay)

	// 保存外层状态，切换作用域
	savedVars, savedScope, savedBase := hr.vars, hr.chartScope, hr.baseDir
	defer func() {
		hr.vars, hr.chartScope, hr.baseDir = savedVars, savedScope, savedBase
	}()
	hr.baseDir = sub.Dir
	hr.chartScope = scope

	var msgs []string
	for _, item := range items {
		hr.vars = e.chartItemVars(hr, scope, subPlay, savedVars, item,
			firstNonEmpty(task.LoopVar, "item"))
		if !e.runChartItemTasks(ctx, &effPlay, subPlay, hr, res, &msgs) {
			return res // 主机不可达，立即终止
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

// effSubPlay 构建子 chart 的有效 play（hosts/become/strategy 继承父，
// environment 合并且父级优先，serial 与 vars 不下沉）。
func effSubPlay(p *model.Play, subPlay *model.Play) model.Play {
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
	return effPlay
}

// chartItemVars 组装单个 item 的子任务变量域：
// host 基础变量 + 子作用域 values + 子 play vars + 内置变量/facts 穿透 + item。
func (e *Executor) chartItemVars(hr *hostRun, scope map[string]any, subPlay *model.Play, savedVars map[string]any, item any, loopVar string) map[string]any {
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
		vars[loopVar] = item
	}
	return vars
}

// runChartItemTasks 执行单个 item 的子 chart 任务序列，结果聚合进 res 与 msgs。
// 返回 false 表示主机不可达，调用方应立即终止。
func (e *Executor) runChartItemTasks(ctx context.Context, effPlay *model.Play, subPlay *model.Play, hr *hostRun, res *model.TaskResult, msgs *[]string) bool {
	itemFailed := false
	for _, t := range subPlay.Tasks {
		if !taskSelected(t, e.Opts) {
			continue
		}
		// 子任务不发独立 TaskStart/HostResult（大规模主机下会刷屏），
		// 结果聚合进 chart 任务：异常带子任务名前缀。
		r := e.runTaskOnHost(ctx, effPlay, t, hr)
		e.recordResult(hr, r, nil, t.IgnoreErrors)
		if r.Unreachable {
			res.Unreachable = true
			res.Msg = r.Msg
			return false
		}
		if r.Failed {
			res.Failed = true
			if !t.IgnoreErrors {
				itemFailed = true
			}
			*msgs = append(*msgs, fmt.Sprintf("%s: %s", t.Label(), r.Msg))
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
	return true
}

// builtinVars 是强制注入的内置变量名（子 chart 作用域穿透清单）。
var builtinVars = []string{
	"inventory_hostname", "group_names", "play_hosts", "play_batch", "groups", "hosts",
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
			if r.Failed && !t.IgnoreErrors {
				// block 内失败即转 rescue；ignore_errors 的失败是例外，不视为 block 失败
				return true, false, append(msgs, fmt.Sprintf("%s: %s", t.Label(), r.Msg))
			}
		}
		return false, false, msgs
	}

	blockFailed, unreachable, msgs := runSeq(task.Block)
	if unreachable {
		// always 语义是"无论如何都要执行的清理"，主机不可达时仍应尝试
		// （连接故障时 always 任务会各自报 unreachable，但不会被静默跳过）
		res.Unreachable = true
		res.Msg = strings.Join(msgs, "; ")
		if len(task.Always) > 0 {
			if _, un, amsgs := runSeq(task.Always); un {
				res.Msg = strings.Join(append(msgs, amsgs...), "; ")
			} else {
				res.Msg += "（always 已尝试执行）"
			}
		}
		res.Task = task.Label()
		res.Module = "block"
		res.Host = hr.host.Name
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
				if _, un, amsgs := runSeq(task.Always); un {
					res.Msg = strings.Join(append(rmsgs, amsgs...), "; ")
				}
				res.Task = task.Label()
				res.Module = "block"
				res.Host = hr.host.Name
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
