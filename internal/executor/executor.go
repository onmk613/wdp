// Package executor 是编排引擎：按 play 推进任务，
// 以 forks 限流并发到各主机，处理 when/loop/register/notify/handlers，
// 并支持 chart 任务（子 chart 任务序列就地展开，作用域隔离）。
package executor

import (
	"context"
	"strings"
	"sync"

	"wdp/internal/chart"
	"wdp/internal/connection"
	"wdp/internal/inventory"
	"wdp/internal/model"
	"wdp/internal/module"
	"wdp/internal/render"
	"wdp/internal/report"

	"gopkg.in/yaml.v3"
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

	deadMu    sync.Mutex
	deadHosts map[string]bool // 本次 run 内失败/不可达的主机（后续 play 不再参与）
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
	chartDepth int                     // chart 引用展开深度（防自引用/互引用递归崩溃）
}

// maxChartDepth 是 chart 引用展开的深度上限：
// 合法的组件组合远小于此值，超过即视为环引用并报错（而非栈溢出崩溃）。
const maxChartDepth = 32

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
	e.deadHosts = map[string]bool{}
	return e
}

// renderLoopItems 渲染 loop 项。单个模板元素渲染结果为 JSON/YAML 列表字符串时
// 展开为多项（loop 支持模板：渲染结果须是列表，如 '{{ to_json .vals }}'）；
// 非列表语法保持单元素语义不变（'{{ .name }}' 仍是单个字符串项）。
func (e *Executor) renderLoopItems(loop []any, vars map[string]any) ([]any, error) {
	lv, err := e.engine.RenderValue(loop, vars)
	if err != nil {
		return nil, err
	}
	items, _ := lv.([]any)
	if len(loop) != 1 || len(items) != 1 {
		return items, nil
	}
	tpl, ok := loop[0].(string)
	if !ok || !strings.Contains(tpl, "{{") {
		return items, nil
	}
	rs, ok := items[0].(string)
	if !ok || !strings.HasPrefix(strings.TrimSpace(rs), "[") {
		return items, nil
	}
	var parsed []any
	if err := yaml.Unmarshal([]byte(rs), &parsed); err != nil || parsed == nil {
		return items, nil // 解析失败维持单元素（字符串原文即用户数据）
	}
	return parsed, nil
}

// Run 依次执行全部 play，返回是否存在失败。
func (e *Executor) Run(ctx context.Context, plays []*model.Play) bool {
	anyFail := false
	for _, p := range plays {
		if ctx.Err() != nil {
			e.Rep.PlayMsg("执行已取消（%v），终止剩余 play", ctx.Err())
			return true
		}
		if e.runPlay(ctx, p) {
			anyFail = true
		}
	}
	return anyFail
}

// LastStats 返回最近一次 run 的汇总统计快照（部署记录用）。
// 返回深拷贝而非内部 map：调用方持有快照期间 run 仍可能写入，避免数据竞争。
func (e *Executor) LastStats() map[string]*model.Stats {
	e.statsMu.Lock()
	defer e.statsMu.Unlock()
	out := make(map[string]*model.Stats, len(e.stats))
	for k, v := range e.stats {
		if v == nil {
			continue
		}
		c := *v
		out[k] = &c
	}
	return out
}
