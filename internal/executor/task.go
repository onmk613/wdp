package executor

// 单任务在单主机上的执行：when 求值 → 参数/环境渲染 → delegate_to 解析 →
// 模块调用（超时/until 轮询/失败重试）→ loop 聚合 → changed_when/failed_when
// 判定 → register → notify。

import (
	"context"
	"fmt"
	"maps"
	"time"

	"wdp/internal/model"
	"wdp/internal/render"
)

// taskRun 携带单任务解析后的执行参数（delegate_to 目标、提权、环境变量等），
// runTaskOnHost 在单主机上执行任务（含 when/loop/register/retry，chart 任务递归展开）。
func (e *Executor) runTaskOnHost(ctx context.Context, p *model.Play, task *model.Task, hr *hostRun) *model.TaskResult {
	base := func() map[string]any {
		v := map[string]any{}
		maps.Copy(v, hr.vars)
		return v
	}
	res := newTaskResult(hr, task)
	start := time.Now()
	defer func() { res.ElapsedMs = time.Since(start).Milliseconds() }()

	// 断点续跑：journal 已记 ok/changed 的任务不重做（register 记 skipped
	// 数据——崩溃前的真实输出不可恢复，依赖它的 when 分支按 skipped 处理）
	if task.PlanIdx > 0 && e.Opts.SkipDone != nil && e.Opts.SkipDone[hr.host.Name][task.PlanIdx] {
		res.Skipped = true
		res.SkipReason = "already completed (resumed from journal)"
		if task.Register != "" {
			hr.vars[task.Register] = registerData(task, res)
		}
		return res
	}

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
	res := &model.TaskResult{Host: hr.host.Name, Task: task.Label(), Module: task.Module, PlanIdx: task.PlanIdx}
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
				hr.vars[task.Register] = registerData(task, res)
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
	// HostByName 裸遍历 inv.Hosts：与 add_host/group_by 的并发写（exec.go
	// 持 invMu）可能在同一 fanOut 波内并发，读侧同样必须持锁
	e.invMu.Lock()
	dh := e.Inv.HostByName(s)
	e.invMu.Unlock()
	if dh != nil {
		return dh, s, nil
	}
	return nil, "", fmt.Errorf("delegate_to target host %q not found in inventory", s)
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
			res.Stdout = appendCapped(res.Stdout, lr.Stdout)
		}
		if lr.Stderr != "" {
			res.Stderr = appendCapped(res.Stderr, lr.Stderr)
		}
	}
	if len(loopResults) > 0 {
		last := loopResults[len(loopResults)-1]
		res.Rc, res.Msg = last.Rc, last.Msg
		// Items 逐项输出截到 16KiB：Items 整体随结果对象驻留内存并进
		// JSON 报告，单项 1MiB × 万级 item 不受 appendCapped 的聚合预算
		// 保护。浅拷贝后截断（register 引用的原对象不动），聚合 stdout
		// 仍走 appendCapped 的 1MiB 总预算。
		items := make([]*model.TaskResult, len(loopResults))
		for i, lr := range loopResults {
			c := new(*lr)
			c.Stdout = truncateTo(lr.Stdout, maxItemOutLen, "item output")
			c.Stderr = truncateTo(lr.Stderr, maxItemOutLen, "item stderr")
			items[i] = c
		}
		res.Items = items // 逐项结果（-vv 展示 / JSON 记录）
	}
	return false
}

// maxItemOutLen 是 Items 数组内单项输出/错误输出的保留上限。
const maxItemOutLen = 16 << 10

// truncateTo 定长截断（Items 逐项压缩用；truncateOut 的 1MiB 口径不适配
// 万级 item 的数组场景）。
func truncateTo(s string, max int, what string) string {
	if len(s) <= max {
		return s
	}
	return truncateAtBoundary(s, max) + fmt.Sprintf("\n…[wdp] %s truncated (%d bytes)", what, len(s))
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

// registerData 构造 skip 路径（when 不满足 / journal 续跑）的 register
// 变量数据：loop 任务同样附 results 空列表——与 registerResult 的口径
// 一致，下游 len .r.results 不因任务被跳过而报错。
func registerData(task *model.Task, res *model.TaskResult) map[string]any {
	data := resultData(res)
	if task.Loop != nil {
		data["results"] = []any{}
	}
	return data
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
