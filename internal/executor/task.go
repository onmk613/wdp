package executor

// 单任务在单主机上的执行：when 求值 → 参数/环境渲染 → delegate_to 解析 →
// 模块调用（超时/until 轮询/失败重试）→ loop 聚合 → changed_when/failed_when
// 判定 → register → notify。

import (
	"context"
	"fmt"
	"time"

	"wdp/internal/i18n"
	"wdp/internal/model"
	"wdp/internal/render"
)

// taskRun 携带单任务解析后的执行参数（delegate_to 目标、提权、环境变量等），
// runTaskOnHost 在单主机上执行任务（含 when/loop/register/retry，chart 任务递归展开）。
func (e *Executor) runTaskOnHost(ctx context.Context, p *model.Play, task *model.Task, hr *hostRun) *model.TaskResult {
	base := func() map[string]any {
		v := map[string]any{}
		for k, val := range hr.vars {
			v[k] = val
		}
		return v
	}
	res := newTaskResult(hr, task)
	start := time.Now()
	defer func() { res.ElapsedMs = time.Since(start).Milliseconds() }()

	// when（可引用 register 变量）
	if r, done := e.evalWhen(task, base, hr, res); done {
		return r
	}

	// chart 引用任务：展开子 chart 任务序列
	if task.ChartRef != "" {
		return e.runChartTask(ctx, p, task, hr, res, base)
	}

	// block 组：顺序执行，失败转 rescue，always 恒执行
	if task.Block != nil {
		return e.runBlock(ctx, p, task, hr, res)
	}

	// 渲染参数（free-form 在 execModule 内按 item 上下文渲染，loop 场景需注入 item）
	vars := base()
	env, err := e.renderEnv(p, task, vars)
	if err != nil {
		return fail(res, err)
	}
	become := p.Become
	if task.Become != nil {
		become = *task.Become
	}
	execHost, delegate, err := e.resolveDelegate(task, vars, hr.host)
	if err != nil {
		return fail(res, err)
	}

	tr := &taskRun{
		p: p, task: task, hr: hr,
		execHost: execHost, env: env,
		become:     become,
		becomeUser: firstNonEmpty(task.BecomeUser, p.BecomeUser),
		loopVar:    firstNonEmpty(task.LoopVar, "item"),
		base:       base, proto: res,
	}
	if delegate != "" {
		res.DelegateTo = delegate
	}

	var loopResults []*model.TaskResult
	if task.Loop != nil {
		items, err := e.renderLoopItems(task.Loop, vars)
		if err != nil {
			return fail(res, err)
		}
		for _, item := range items {
			lr := e.execModule(ctx, tr, item)
			if item != nil {
				lr.Item = fmt.Sprint(item)
			}
			loopResults = append(loopResults, lr)
		}
		if stop := aggregateLoopResults(res, loopResults); stop {
			return res
		}
	} else {
		*res = *e.execModule(ctx, tr, nil)
	}
	res.Task = task.Label()
	res.Module = task.Module
	res.Host = hr.host.Name

	if err := e.applyResultJudgements(task, base, res); err != nil {
		return fail(res, err)
	}
	registerResult(task, hr, res, loopResults)

	// notify
	if res.Changed && !res.Failed && len(task.Notify) > 0 {
		for _, n := range task.Notify {
			hr.notified[n] = true
		}
	}
	return res
}

// newTaskResult 构造任务结果模板（含任务级展示控制 output/no_log）。
func newTaskResult(hr *hostRun, task *model.Task) *model.TaskResult {
	res := &model.TaskResult{Host: hr.host.Name, Task: task.Label(), Module: task.Module}
	// 任务级展示控制（只影响回显，不影响 register 数据）
	res.Output = task.Output
	if task.NoLog {
		res.NoLog = true // 供 JSON 报告遮蔽（output=none 仅控制台回显语义）
		res.Output = "none"
	}
	return res
}

// evalWhen 渲染 when 条件列表；任一条件不满足时标记跳过并（按需）register，
// 使 `when: not r.skipped` 之类的惯用法可用。返回 (结果, true) 表示调用方应立即返回。
func (e *Executor) evalWhen(task *model.Task, base func() map[string]any, hr *hostRun, res *model.TaskResult) (*model.TaskResult, bool) {
	if len(task.When) == 0 {
		return nil, false
	}
	vars := base()
	for _, cond := range task.When {
		s, err := e.engine.Render(cond, vars)
		if err != nil {
			return fail(res, err), true
		}
		if !render.Truthy(s) {
			res.Skipped = true
			res.SkipReason = cond
			if task.Register != "" {
				hr.vars[task.Register] = resultData(res)
			}
			return res, true
		}
	}
	return nil, false
}

// renderEnv 渲染 play 级与任务级环境变量（值支持模板引用变量）。
func (e *Executor) renderEnv(p *model.Play, task *model.Task, vars map[string]any) (map[string]string, error) {
	env := map[string]string{}
	for k, v := range p.Environment {
		rv, err := e.engine.Render(v, vars)
		if err != nil {
			return nil, err
		}
		env[k] = rv
	}
	for k, v := range task.Environment {
		rv, err := e.engine.Render(v, vars)
		if err != nil {
			return nil, err
		}
		env[k] = rv
	}
	return env, nil
}

// resolveDelegate 解析 delegate_to：任务改在指定主机（或 localhost）上执行，
// 变量域保持原主机（inventory_hostname 不变），结果归属原主机。
// 非委托任务返回原主机与空目标名。
func (e *Executor) resolveDelegate(task *model.Task, vars map[string]any, origin *model.Host) (*model.Host, string, error) {
	if task.DelegateTo == "" {
		return origin, "", nil
	}
	s, err := e.engine.Render(task.DelegateTo, vars)
	if err != nil {
		return nil, "", err
	}
	if s == "localhost" {
		return e.localhost(), s, nil
	}
	if dh := e.Inv.HostByName(s); dh != nil {
		return dh, s, nil
	}
	return nil, "", fmt.Errorf(i18n.T("delegate_to target host %q not found in inventory", "delegate_to 目标主机 %q 不存在于 inventory"), s)
}

// aggregateLoopResults 聚合 loop 逐项结果到任务级结果；
// 任一项不可达时终止聚合（后续 when 覆盖与 register 不再执行）。
func aggregateLoopResults(res *model.TaskResult, loopResults []*model.TaskResult) bool {
	for _, lr := range loopResults {
		if lr.Unreachable {
			res.Unreachable = true
			res.Msg = lr.Msg
			return true
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
	return false
}

// applyResultJudgements 以 changed_when / failed_when 覆盖结果判定。
func (e *Executor) applyResultJudgements(task *model.Task, base func() map[string]any, res *model.TaskResult) error {
	if task.ChangedWhen == "" && task.FailedWhen == "" {
		return nil
	}
	judge := base()
	judge["result"] = resultData(res)
	if task.ChangedWhen != "" {
		s, err := e.engine.Render(task.ChangedWhen, judge)
		if err != nil {
			return err
		}
		res.Changed = render.Truthy(s)
	}
	if task.FailedWhen != "" {
		s, err := e.engine.Render(task.FailedWhen, judge)
		if err != nil {
			return err
		}
		res.Failed = render.Truthy(s)
	}
	return nil
}

// registerResult 把任务结果写入 register 变量（loop 任务附 results 列表；
// 空 loop 同样产出 results: []，下游 len .r.results 不炸）。
func registerResult(task *model.Task, hr *hostRun, res *model.TaskResult, loopResults []*model.TaskResult) {
	if task.Register == "" {
		return
	}
	data := resultData(res)
	if task.Loop != nil {
		list := make([]any, len(loopResults))
		for i, lr := range loopResults {
			list[i] = resultData(lr)
		}
		data["results"] = list
	}
	hr.vars[task.Register] = data
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
