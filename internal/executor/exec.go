package executor

// 模块调用执行：taskRun 参数集上的单次模块调用（超时、until 轮询、失败重试）
// 与模块结果到任务结果/变量域的沉淀。

import (
	"context"
	"fmt"
	"time"

	"wdp/internal/connection"
	"wdp/internal/i18n"
	"wdp/internal/model"
	"wdp/internal/module"
	"wdp/internal/render"
)

// 供 loop 逐 item 的模块调用复用。
type taskRun struct {
	p          *model.Play
	task       *model.Task
	hr         *hostRun
	execHost   *model.Host       // 实际执行主机（delegate_to 后；变量域保持原主机）
	env        map[string]string // play 级 + 任务级环境变量（已渲染）
	become     bool
	becomeUser string
	loopVar    string                // loop_control.loop_var，缺省 item
	base       func() map[string]any // 原主机变量域快照构造器
	proto      *model.TaskResult     // 结果模板（Host/Task/Module/Output/NoLog）
}

// execModule 在单主机上执行一次模块调用：注入 item 变量、渲染参数、应用任务级超时，
// 再按 until 条件轮询或失败重试策略驱动调用并返回结果。
func (e *Executor) execModule(ctx context.Context, tr *taskRun, item any) *model.TaskResult {
	task := tr.task
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

	itemVars := tr.base()
	if item != nil {
		itemVars[tr.loopVar] = item
	}
	iargs, err := e.engine.RenderValue(task.Args, itemVars)
	if err != nil {
		return fail(tr.proto, err)
	}
	args, _ := iargs.(map[string]any)
	free := task.FreeForm
	if free != "" {
		free, err = e.engine.Render(free, itemVars)
		if err != nil {
			return fail(tr.proto, err)
		}
	}
	conn, err := e.Conns.Get(taskCtx, tr.execHost)
	if err != nil {
		r := cloneRes(tr.proto)
		r.Unreachable = true
		r.Msg = err.Error()
		return r
	}
	mod, scriptPath, err := e.resolveModule(tr.hr, task.Module, args, free)
	if err != nil {
		return fail(tr.proto, err)
	}
	rc := e.buildRunContext(taskCtx, tr, itemVars, conn, timeoutSec)
	// 模块调用统一入口：内置注册表优先，chart 脚本模块回退
	invoke := func() *module.Result {
		if scriptPath != "" {
			return module.RunScriptModule(rc, scriptPath, args, free)
		}
		return mod.Run(rc, args, free)
	}
	r := cloneRes(tr.proto)
	apply := func(mr *module.Result) { e.applyModuleResult(r, tr.hr, mr) }
	if task.Until != "" {
		return e.retryUntil(taskCtx, task, itemVars, invoke, apply, r)
	}
	return retryOnFailure(taskCtx, task, invoke, apply, r)
}

// resolveModule 解析任务模块（唯一规则见 module.Resolve）：内置注册表优先，
// chart 本地脚本模块回退；内置模块同时校验参数（未知键/非法 bool/mode 直接报错，
// 杜绝拼写错误静默失效）。
func (e *Executor) resolveModule(hr *hostRun, name string, args map[string]any, free string) (module.Module, string, error) {
	mod, scriptPath, ok := module.Resolve(name, e.scriptModuleDirs(hr))
	if !ok {
		return nil, "", fmt.Errorf(i18n.T("unknown module %q", "未知模块 %q"), name)
	}
	if mod != nil {
		if verr := module.ValidateArgs(mod, args, free); verr != nil {
			return nil, "", verr
		}
	}
	return mod, scriptPath, nil
}

// scriptModuleDirs 当前主机的 chart 本地脚本模块查找目录（playbook 目录优先于 chart 根）。
func (e *Executor) scriptModuleDirs(hr *hostRun) []string {
	if e.Opts.Chart == nil {
		return nil
	}
	return []string{hr.baseDir, e.Opts.BaseDir}
}

// buildRunContext 组装模块执行上下文（连接、变量域、提权、check/diff、回滚登记）。
func (e *Executor) buildRunContext(taskCtx context.Context, tr *taskRun, itemVars map[string]any, conn connection.Connection, timeoutSec int) *module.RunContext {
	rc := &module.RunContext{
		Ctx:        taskCtx,
		Conn:       conn,
		Host:       tr.execHost,
		Vars:       itemVars,
		Env:        tr.env,
		Become:     tr.become,
		BecomeUser: tr.becomeUser,
		BaseDir:    tr.hr.baseDir,
		Engine:     e.engine,
		TimeoutMs:  int64(timeoutSec) * 1000,
		CheckMode:  e.Opts.CheckMode,
		DiffMode:   e.Opts.DiffMode,

		MaxDownloadBytes: e.Opts.MaxDownloadBytes,
	}
	// 脚本模块 check 模式需 chart.yaml 显式声明 check_mode: supported
	if e.Opts.Chart != nil {
		rc.CheckScriptAllowed = bool(e.Opts.Chart.Meta.CheckMode)
	}
	// auto_rollback：注入变更日志（文件类模块登记快照/删除动作）。
	// 每条记录携带实际执行主机（delegate_to 时回滚必须打到被委托主机）。
	if tr.p.Strategy != nil && tr.p.Strategy.AutoRollback && !e.Opts.CheckMode && e.rollbackDir != "" {
		rc.Rollback = &module.RollbackCtx{
			Dir: e.rollbackDir,
			Record: func(a module.RollbackAction) {
				tr.hr.mu.Lock()
				tr.hr.journal = append(tr.hr.journal, journalEntry{action: a, execOn: tr.execHost})
				tr.hr.mu.Unlock()
			},
		}
	}
	return rc
}

// applyModuleResult 将模块结果写入任务结果，并沉淀 facts 与 group_by 动态组。
func (e *Executor) applyModuleResult(r *model.TaskResult, hr *hostRun, mr *module.Result) {
	r.Changed = mr.Changed
	r.Failed = mr.Failed
	r.Skipped = mr.Skipped
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

// retryUntil 执行 until 轮询：retries 表示重试次数，总尝试 = retries + 1（未设置时默认 3）；
// delay=轮询间隔秒数（0 不等待）；条件满足采用当轮结果；耗尽则按失败计。
func (e *Executor) retryUntil(taskCtx context.Context, task *model.Task, itemVars map[string]any, invoke func() *module.Result, apply func(*module.Result), r *model.TaskResult) *model.TaskResult {
	attempts := 3
	if task.Retries > 0 {
		attempts = task.Retries + 1
	}
	delay := task.DelaySec
	var last *module.Result
	for i := 0; i < attempts; i++ {
		if i > 0 {
			timer := time.NewTimer(time.Duration(delay) * time.Second)
			select {
			case <-taskCtx.Done():
				timer.Stop()
				r.Unreachable = true
				r.Msg = taskCtx.Err().Error()
				return r
			case <-timer.C:
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
			apply(last)
			r.Failed = true
			r.Msg = i18n.T("until render failed: ", "until 渲染失败: ") + err.Error()
			return r
		}
		if render.Truthy(s) {
			apply(last)
			return r
		}
	}
	apply(last)
	r.Failed = true
	r.Msg = fmt.Sprintf("until 条件在 %d 次尝试后仍未满足", attempts)
	if last != nil && last.Msg != "" {
		r.Msg += ": " + last.Msg
	}
	return r
}

// retryOnFailure 失败重试（无 until：成功即停）。
func retryOnFailure(taskCtx context.Context, task *model.Task, invoke func() *module.Result, apply func(*module.Result), r *model.TaskResult) *model.TaskResult {
	attempts := task.Retries + 1
	if attempts < 1 {
		attempts = 1
	}
	for i := 0; i < attempts; i++ {
		if i > 0 && task.DelaySec > 0 {
			timer := time.NewTimer(time.Duration(task.DelaySec) * time.Second)
			select {
			case <-taskCtx.Done():
				timer.Stop()
				r.Unreachable = true
				r.Msg = taskCtx.Err().Error()
				return r
			case <-timer.C:
			}
		}
		mr := invoke()
		apply(mr)
		if !mr.Failed {
			break
		}
	}
	return r
}
