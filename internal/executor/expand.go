package executor

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"wdp/internal/chart"
	"wdp/internal/model"
)

// 任务展开：子 chart 引用（runChartTask）与 block/rescue/always 任务组（runBlock）。

// runChartTask 展开执行子 chart 任务序列（作用域隔离：子树 + global + 引用 vars）。
// 本层负责引用解析、主机过滤与深度防护；作用域组装、schema 校验与
// loop 执行见 runChartScope。
func (e *Executor) runChartTask(ctx context.Context, p *model.Play, task *model.Task, hr *hostRun, res *model.TaskResult, base func() map[string]any) *model.TaskResult {
	sub, ok := e.chartRefTarget(task, hr, res)
	if !ok {
		return res
	}
	// 环引用防护：chart 自引用/互引用会在此递归展开中无限下钻，
	// 超过深度上限即报错终止（Go 的栈溢出无法 recover，必须前置拦截）。
	// 深度增减必须留在本层：defer 的生效范围要覆盖整个子树执行（含
	// runChartScope 内逐 item 任务），下钻进子函数会在其返回时提前递减。
	if hr.chartDepth >= maxChartDepth {
		res.Failed = true
		res.Msg = fmt.Sprintf("chart reference expansion exceeded the depth limit %d (possible reference cycle: %s)", maxChartDepth, task.ChartRef)
		return res
	}
	hr.chartDepth++
	defer func() { hr.chartDepth-- }()
	// 入口 play：缺省 deploy.yaml，`tasks_from:`/`phase: <相位>` 可选子 chart 的其他相位
	subPlay, perr := sub.EntryPlay(task.TasksFrom)
	if subPlay == nil {
		res.Failed = true
		res.Msg = perr.Error()
		return res
	}
	return e.runChartScope(ctx, p, task, hr, res, base, sub, subPlay)
}

// chartRefTarget 解析 chart 引用并做主机过滤（hosts: 选择器）。
// 返回 false 表示 res 已置终态（失败或跳过），调用方直接返回 res。
// chart 模式从父 chart 子表解析引用；裸 playbook 模式从启动期预加载的
// ChartRefs（playbook 同级 charts/）解析。
func (e *Executor) chartRefTarget(task *model.Task, hr *hostRun, res *model.TaskResult) (*chart.Chart, bool) {
	sub, serr := e.resolveChartRef(task.ChartRef)
	if sub == nil {
		res.Failed = true
		res.Msg = serr.Error()
		return nil, false
	}
	// 主机过滤（hosts: 选择器）：当前 play 批次 ∩ 选择器，不在集合的主机
	// 跳过该引用。不跨 play 重选主机——批次/串行/回滚语义保持在 play 级。
	// Select 裸读 inv.Groups/inv.Hosts：与 add_host/group_by 的并发写
	//（exec.go 持 invMu）可在同一 fanOut 波内并发，读侧同样必须持锁。
	if task.ChartHosts != "" {
		e.invMu.Lock()
		sel, herr := e.Inv.Select(task.ChartHosts)
		e.invMu.Unlock()
		if herr != nil {
			res.Failed = true
			res.Msg = fmt.Sprintf("chart %s: invalid hosts selector %q: %v", task.ChartRef, task.ChartHosts, herr)
			return nil, false
		}
		inSet := false
		for _, h := range sel {
			if h.Name == hr.host.Name {
				inSet = true
				break
			}
		}
		if !inSet {
			res.Skipped = true
			res.Msg = fmt.Sprintf("chart %s: host outside selector %q, skipped", task.ChartRef, task.ChartHosts)
			res.Task = task.Label()
			res.Module = "chart"
			res.Host = hr.host.Name
			return nil, false
		}
	}
	return sub, true
}

// runChartScope 组装子 chart 作用域（子树 + global + values_from 覆盖 +
// 引用 vars）、过展开期 schema 校验，再逐 loop item 执行子任务序列并
// 聚合结果。调用方（runChartTask）已保证深度防护与入口 play 解析。
func (e *Executor) runChartScope(ctx context.Context, p *model.Play, task *model.Task, hr *hostRun, res *model.TaskResult, base func() map[string]any, sub *chart.Chart, subPlay *model.Play) *model.TaskResult {
	// 子 chart 作用域 values（低 → 高）：子 chart 默认 values → 父作用域 <子chart名> 子树
	// → global（跨层共享）→ values_from 覆盖文件 → 引用 vars/values
	scope := chart.SubScope(sub, hr.chartScope)
	if len(task.ChartValuesFrom) > 0 {
		ov, ferr := e.loadRefValuesFiles(hr.baseDir, task.ChartValuesFrom)
		if ferr != nil {
			return fail(res, ferr)
		}
		scope = chart.Merge(scope, ov)
	}
	if task.ChartVars != nil {
		cv, err := e.engine.RenderValue(task.ChartVars, base())
		if err != nil {
			return fail(res, err)
		}
		if m, ok := cv.(map[string]any); ok {
			maps.Copy(scope, m)
		}
	}

	// 展开期 schema 校验：此刻作用域最完整（含引用 vars），静态走查覆盖不到的
	// 引用处注入值在此拦截
	if err := sub.ValidateValuesSchema(scope); err != nil {
		res.Failed = true
		res.Msg = err.Error()
		return res
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
		res.Msg = fmt.Sprintf("chart %s: %d items completed", sub.Meta.Name, len(items))
		if res.Failed {
			res.Msg = strings.Join(msgs, "; ")
		}
	}
	res.Task = task.Label()
	res.Module = "chart"
	res.Host = hr.host.Name

	if task.Register != "" {
		// 走 registerData（resultData + loop 补 results 空列表）而非裸
		// resultData：chart+loop 是受支持组合（上方逐 item 执行），跳过裸
		// resultData 会让该路径的 register 缺 results 键，下游
		// `len .r.results` 直接模板报错——与 task.go registerData/registerResult
		// 的"loop 任务恒带 results"口径对齐（chart 逐 item 结果聚合进 res，
		// 不逐项展开，故与 skip 路径同构只补空列表）。
		savedVars[task.Register] = registerData(task, res)
	}
	if res.Changed && !res.Failed && len(task.Notify) > 0 {
		for _, n := range task.Notify {
			hr.notified[n] = true
		}
	}
	return res
}

// resolveChartRef 解析 chart 引用：chart 模式走父 chart 子表（含 @版本
// 约束）；裸 playbook 模式走启动期预加载的 ChartRefs（组合根从 playbook
// 同级 charts/ 加载）。两路的版本约束语义一致（semver）。
func (e *Executor) resolveChartRef(ref string) (*chart.Chart, error) {
	name, constraint, constrained := strings.Cut(ref, "@")
	var sub *chart.Chart
	if e.Opts.Chart != nil {
		return e.Opts.Chart.ResolveSub(ref)
	}
	sub = e.Opts.ChartRefs[name]
	if sub == nil {
		return nil, fmt.Errorf("unknown chart reference %q (bare playbooks resolve refs from the charts/ directory next to the playbook; found: %s)",
			ref, sortedRefNames(e.Opts.ChartRefs))
	}
	if constrained && constraint != "" {
		// 与 chart.ResolveSub 同一约束检查入口（chart.CheckVersionConstraint），
		// 两路版本语义不再各写一份
		if err := chart.CheckVersionConstraint("chart", name, sub.Meta.Version, constraint); err != nil {
			return nil, err
		}
	}
	return sub, nil
}

func sortedRefNames(refs map[string]*chart.Chart) string {
	if len(refs) == 0 {
		return "none"
	}
	names := make([]string, 0, len(refs))
	for n := range refs {
		names = append(names, n)
	}
	slices.Sort(names)
	return strings.Join(names, ", ")
}

// loadRefValuesFiles 加载 chart 引用的 values_from 覆盖文件（相对当前
// chart/playbook 根目录，依序合并，后者胜出）。文件是纯 YAML 不渲染模板
// ——与 chart 模式 -f values 文件同口径。
func (e *Executor) loadRefValuesFiles(baseDir string, files []string) (map[string]any, error) {
	merged := map[string]any{}
	absBase, aerr := filepath.Abs(baseDir)
	if aerr != nil {
		return nil, fmt.Errorf("resolve chart dir %q: %w", baseDir, aerr)
	}
	for _, f := range files {
		path := f
		if !filepath.IsAbs(path) {
			path = filepath.Join(absBase, f)
		}
		// 路径必须落在 chart 目录内：chart 由 operator 级账号上传/编辑，
		// 放行绝对路径与 .. 就能让 chart 读控制端任意 YAML（如
		// ~/.wdp/releases/*.json）当 values 合并进变量域再外带
		abs, aerr := filepath.Abs(path)
		if aerr != nil {
			return nil, fmt.Errorf("chart values_from %s: %w", f, aerr)
		}
		rel, rerr := filepath.Rel(absBase, abs)
		if rerr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("chart values_from %s: path escapes the chart directory %s (absolute paths and .. escapes are refused)", f, absBase)
		}
		data, err := os.ReadFile(abs)
		if err != nil {
			return nil, fmt.Errorf("chart values_from %s: %w", f, err)
		}
		ov, err := chart.LoadValuesYAML(data)
		if err != nil {
			return nil, fmt.Errorf("chart values_from %s: %w", f, err)
		}
		merged = chart.Merge(merged, ov)
	}
	return merged, nil
}

// effSubPlay 构建子 chart 的有效 play（hosts/become/strategy 继承父，
// environment 合并且**子 play 覆盖父级**（更具体的作用域优先，与 values
// 分层一致），serial 与 vars 不下沉）。
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
		maps.Copy(merged, p.Environment)
		maps.Copy(merged, effPlay.Environment)
		effPlay.Environment = merged
	}
	return effPlay
}

// chartItemVars 组装单个 item 的子任务变量域：
// host 基础变量 + 子作用域 values + 子 play vars + 内置变量/facts 穿透 + item。
func (e *Executor) chartItemVars(hr *hostRun, scope map[string]any, subPlay *model.Play, savedVars map[string]any, item any, loopVar string) map[string]any {
	vars := map[string]any{}
	// host.Vars 裸读：add_host 对同名主机的变量合并（exec.go 持 invMu）
	// 可能在同一 fanOut 波内并发，读侧同样必须持锁
	e.invMu.Lock()
	maps.Copy(vars, hr.host.Vars)
	e.invMu.Unlock()
	maps.Copy(vars, scope)
	maps.Copy(vars, subPlay.Vars)
	// 主机 facts（setup/stat 等运行时数据）同样穿透：属于主机而非 chart 作用域
	e.seedFacts(hr.host.Name, vars)
	// 内置变量最后注入：穿透子 chart 作用域（清单与赋值统一在 builtins.go），
	// 且不被 facts 或子作用域同名键覆盖（与顶层 prepareBatchRuns 的强制注入对齐）
	for _, k := range builtinVarNames {
		if v, ok := savedVars[k]; ok {
			vars[k] = v
		}
	}
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
			// 子任务 ignore_errors 只豁免主机存活与批次中止：聚合结果
			// 不置 Failed（此前无条件置位会让 ignore_errors 在 chart
			// 引用内完全失效——主机被标死、后续 play 全跳过）
			if !t.IgnoreErrors {
				res.Failed = true
				itemFailed = true
			}
			*msgs = append(*msgs, fmt.Sprintf("%s: %s", t.Label(), r.Msg))
		}
		if r.Changed {
			res.Changed = true
		}
		if r.Stdout != "" {
			// 与 loop 聚合同一总量上限：子 chart 任务序列可能很长，逐项
			// `+=` 不封顶会撑爆内存
			res.Stdout = appendCapped(res.Stdout, r.Stdout)
		}
		if itemFailed {
			break // 该 item 的子序列中断，继续下一 item
		}
	}
	return true
}

// runBlock 执行 block/rescue/always 任务组（单主机内顺序，支持嵌套）。
// 本层负责 block/rescue/always 的控制流编排；单段序列的执行见 runBlockSeq。
func (e *Executor) runBlock(ctx context.Context, p *model.Play, task *model.Task, hr *hostRun, res *model.TaskResult) *model.TaskResult {
	// block 状态变量只在 block/rescue/always 执行期间可见，任务结束即清理，
	// 防止后续任务的 when 读到陈旧的 block_failed=true
	defer func() {
		delete(hr.vars, "block_failed")
		delete(hr.vars, "block_failed_msgs")
	}()
	// 容器 tags 对子任务生效（继承语义，与顶层单任务一致）
	effTags := effectiveTags(task, nil)
	runSeq := func(tasks []*model.Task) (bool, bool, []string) {
		return e.runBlockSeq(ctx, p, hr, res, effTags, tasks)
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
				res.Msg += " (always attempted)"
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
			res.Msg = "block failed: " + strings.Join(msgs, "; ")
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
				res.Msg = "block failed and rescue failed: " + strings.Join(append(msgs, rmsgs...), "; ")
			} else {
				// rescue 成功兜底：组视为已恢复（changed 保持）
				res.Msg = "block failure recovered by rescue: " + strings.Join(msgs, "; ")
				delete(hr.vars, "block_failed")
			}
		}
	}

	if af, unreach, amsgs := runSeq(task.Always); unreach {
		res.Unreachable = true
		res.Msg = strings.Join(amsgs, "; ")
	} else if af {
		// always 清理任务失败同样视为 block 失败（控制流与 RECAP 保持一致；
		// 此前该失败被静默吞掉，部署会误报成功）
		res.Failed = true
		res.Msg = "always task failed: " + strings.Join(amsgs, "; ")
	}
	res.Task = task.Label()
	res.Module = "block"
	res.Host = hr.host.Name
	return res
}

// runBlockSeq 执行 block/rescue/always 中的一段任务序列：按容器 tags
// 过滤，changed 聚合进 res。返回（是否失败，主机是否不可达，消息集）；
// block 内失败即转 rescue，ignore_errors 的失败是例外，不视为 block 失败。
func (e *Executor) runBlockSeq(ctx context.Context, p *model.Play, hr *hostRun, res *model.TaskResult, effTags []string, tasks []*model.Task) (bool, bool, []string) {
	var msgs []string
	for _, t := range tasks {
		if !blockSelected(t, e.Opts, effTags) {
			continue // block 子任务同样遵循 --tags/--skip-tags 过滤
		}
		r := e.runTaskOnHost(ctx, p, t, hr)
		e.recordResult(hr, r, nil, t.IgnoreErrors)
		if r.Changed {
			res.Changed = true
		}
		if r.Unreachable {
			return false, true, append(msgs, r.Msg)
		}
		if r.Failed && !t.IgnoreErrors {
			return true, false, append(msgs, fmt.Sprintf("%s: %s", t.Label(), r.Msg))
		}
	}
	return false, false, msgs
}
