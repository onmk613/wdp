// Package executor 是编排引擎：按 play 推进任务，
// 以 forks 限流并发到各主机，处理 when/loop/register/notify/handlers，
// 并支持 chart 任务（子 chart 任务序列就地展开，作用域隔离）。
package executor

import (
	"context"
	"strings"
	"sync"

	"wdp/internal/chart"
	"wdp/internal/conn"
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
	Phase       string // chart 生命周期相位（缺省 deploy）；hook 按 pre_<phase>/post_<phase> 匹配（deploy 沿用 install 命名）
	WdpVersion  string // 控制端版本（release marker 记录）

	// FactCachePath 非空时启用跨运行 fact cache：启动时加载 JSON 快照并入
	// fact store，run 结束原子落盘（setup/set_fact 结果跨次运行复用）。
	FactCachePath string

	// MaxDownloadBytes 是 get_url 下载响应体上限（字节；0 = 内置默认 2GiB）。
	// 组合根从 --max-download-mb / wdp.cfg [transfer].max_download_mb 注入。
	MaxDownloadBytes int64

	// MaxUploadBytes 是 copy/unarchive 读取本地分发源的上限（字节；0 =
	// 内置默认 2GiB）。组合根从 wdp.cfg [transfer].max_upload_mb 注入。
	MaxUploadBytes int64

	Chart  *chart.Chart   // chart 模式（nil = 裸 playbook 模式）
	Values map[string]any // chart 合并后的最终 values
	// ChartRefs 是裸 playbook 模式的 chart 引用解析表：引用名（去 @版本
	// 约束）→ 启动期从 playbook 同级 charts/ 预加载的 chart。组合根
	//（cli/run）预扫描注入；引用缺失在启动期即失败。
	ChartRefs map[string]*chart.Chart
	// HostValues 按主机名覆盖 Values（非部署相位从 marker 还原的实际部署
	// values；主机间可能因 deploy 时的 inventory_override 而不同）。空 = 全
	// 部主机用 Values。
	HostValues map[string]map[string]any
	// PlanVars 非空时进入 plan 执行模式：主机名 → 编译期冻结的完整变量域
	//（inventory vars + values + play vars + 内置变量快照）。跨主机信息
	//（groups/hosts/hostvars）取冻结值，不再从运行期 inventory 注入；
	// 单主机运行时信息（register/facts/play_batch/BaseDir）仍在本机求值。
	PlanVars map[string]map[string]any
	// SkipDone 是断点续跑的已完成集合：主机名 → plan 任务序号集合
	//（journal 重放）。命中的任务直接按 skipped 记账不执行——幂等模块重跑
	// 通常安全但昂贵（逐主机 checksum 探测），且金丝雀语义会被重置。
	SkipDone map[string]map[int]bool
	// PayloadDir 补齐 plan 大文件引用（--chart-dir）：plan.Payloads 记录的
	// 制品不进 plan 本体，执行时从此目录按相对路径取回并校验摘要。
	PayloadDir string
	Engine     *render.Engine // 渲染引擎（含 helpers 命名模板）
}

// Executor 执行 playbook / chart。
type Executor struct {
	Inv   *inventory.Inventory
	Conns *conn.Manager
	Rep   report.Reporter
	Opts  Options

	engine *render.Engine

	rollbackDir string // 当前 play 的回滚快照根目录（auto_rollback 时非空）

	invMu sync.Mutex // 动态组（group_by）并发聚合锁

	factsMu sync.Mutex
	facts   map[string]map[string]any // 跨 play / 子 chart 作用域的主机 facts（setup/stat 等）

	statsMu sync.Mutex
	stats   map[string]*model.Stats // 当前 play 的统计（子 chart 子任务也计入）
	// totalStats 是整个 run 的跨 play 累计统计（LastStats 的数据源）。
	// stats 每 play 整体替换，release 记录若取 stats 会只剩最后一个 play。
	totalStats map[string]*model.Stats

	deadMu    sync.Mutex
	deadHosts map[string]bool // 本次 run 内失败/不可达的主机（后续 play 不再参与）
}

// hostRun 是单主机在单个 play 中的运行态。
//
// 单写者约定（hr.vars 的无锁写点 why）：vars 的多数写点
// （task.go 的 registerResult/registerData、expand.go 的 chart 作用域切换
// 与 chart register、batch.go fanOutRunOnce 的 register 复制）都不持
// hr.mu；唯一例外是 exec.go applyModuleResult 的 facts 沉淀（持 hr.mu，
// 因 reporter 可能在并发回调中读 vars）。无锁写点的正确性依赖三个不变量
// **同时成立**：
//  1. 每 host 单 goroutine：fanOut/fanOutRunOnce 保证同一 hostRun 同刻
//     只有一个执行 goroutine（固定 worker 池按主机派发，主机间不共享
//     hostRun）；
//  2. 波次串行：批次/hook/handler 之间经 wg.Wait 严格串行，跨波次的
//     写点（playState.seed/harvest 换手）有 happens-before；
//  3. run_once 同步执行：fanOutRunOnce 先在执行主机写完 register，再
//     同步复制到其余主机，无并发写。
//
// 违反任一不变量即为数据竞争（-race 可测）。新增并发特性（如主机内
// 任务并行、跨主机变量共享）前必须先打破本约定并评审：要么给 vars
// 全部读写点加 hr.mu（注意与 exec.go 回滚登记处的既有 hr.mu 用法避免
// 嵌套死锁），要么改为按写点交接不可变快照。
type hostRun struct {
	mu         sync.Mutex
	host       *model.Host
	vars       map[string]any // 当前变量域（无锁读写的边界见上"单写者约定"）
	chartScope map[string]any // 当前 chart 的 values 作用域
	baseDir    string         // 当前 chart 根目录（src 相对路径基准）
	alive      bool
	notified   map[string]bool
	stats      *model.Stats
	journal    []journalEntry // 变更日志（auto_rollback 时快照恢复依据）
	// rollbackGaps 记录"未能建立快照"的路径（快照失败）：这些路径没有
	// 还原依据，回滚汇总必须把它们计入失败，而不是当作回滚完成
	rollbackGaps []string
	chartDepth   int // chart 引用展开深度（防自引用/互引用递归崩溃）
}

// journalEntry 一条变更日志：动作 + 实际执行主机 + 有效提权用户。
// delegate_to 时变更发生在被委托主机上（快照也在那里），回滚与快照清理
// 必须打到实际执行主机；becomeUser 记录变更发生时的有效提权用户
// （空 = 未提权）——快照由该用户创建（提权时通常 root 属主），回放与
// 清理以非提权用户执行会权限不足而失败/残留。
type journalEntry struct {
	action     module.RollbackAction
	execOn     *model.Host // nil = 登记主机自身（无委托）
	becomeUser string      // 空表示变更未提权；become:true 未指定用户时为 "root"
}

// maxChartDepth 是 chart 引用展开的深度上限（单一事实来源在 chart 包，
// 可逆性评估共用同一上限）：合法的组件组合远小于此值，超过即视为环引用
// 并报错（而非栈溢出崩溃）。
const maxChartDepth = chart.MaxChartDepth

// playState 延续同一 play 内跨批次/跨 hook 的主机运行态：
// register/facts 变量与回滚变更日志在批次间保持（修复原批次间 register 丢失）。
type playState struct {
	mu      sync.Mutex
	vars    map[string]map[string]any // 主机名 → 最新变量域
	journal map[string][]journalEntry // 主机名 → 累积变更日志
}

func newPlayState() *playState {
	return &playState{
		vars:    map[string]map[string]any{},
		journal: map[string][]journalEntry{},
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
func (s *playState) takeJournal(host string) []journalEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]journalEntry{}, s.journal[host]...)
}

// New 创建执行器。
func New(inv *inventory.Inventory, conns *conn.Manager, rep report.Reporter, opts Options) *Executor {
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
	if opts.FactCachePath != "" {
		e.loadFactCache(opts.FactCachePath)
	}
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
// 启用了 fact cache 时无论成败都落盘（部分采集的 facts 对下次运行仍有价值）。
func (e *Executor) Run(ctx context.Context, plays []*model.Play) bool {
	// --start-at-task 的标签必须真实存在：拼错时所有 play 静默空跑、
	// 退出码 0，还会写部署记录与 release marker——后续 uninstall/status/
	// drift 全部被误导。这里提前 fail-loud（匹配口径与 runBatchTasks 一致：
	// play 顶层任务标签）。
	if e.Opts.StartAtTask != "" && !startAtTaskExists(plays, e.Opts.StartAtTask) {
		e.Rep.PlayMsg("start-at-task %q: no such task in any play", e.Opts.StartAtTask)
		return true
	}
	if e.Opts.FactCachePath != "" {
		defer e.saveFactCache(e.Opts.FactCachePath)
	}
	anyFail := false
	for _, p := range plays {
		if ctx.Err() != nil {
			e.Rep.PlayMsg("execution cancelled (%v), terminating remaining plays", ctx.Err())
			return true
		}
		if e.runPlay(ctx, p) {
			anyFail = true
		}
	}
	return anyFail
}

// startAtTaskExists 报告任一 play 的顶层任务是否带该标签（runBatchTasks
// 只对 play 顶层任务做 StartAtTask 匹配，嵌套 block 内的任务无法作为起点）。
func startAtTaskExists(plays []*model.Play, label string) bool {
	for _, p := range plays {
		for _, t := range p.Tasks {
			if t.Label() == label {
				return true
			}
		}
	}
	return false
}

// LastStats 返回最近一次 run 的汇总统计快照（部署记录用；跨 play 累计）。
// 返回深拷贝而非内部 map：调用方持有快照期间 run 仍可能写入，避免数据竞争。
func (e *Executor) LastStats() map[string]*model.Stats {
	e.statsMu.Lock()
	defer e.statsMu.Unlock()
	out := make(map[string]*model.Stats, len(e.totalStats))
	for k, v := range e.totalStats {
		if v == nil {
			continue
		}
		out[k] = new(*v)
	}
	return out
}

// mergeStats 把单个 play 的统计并入跨 play 累计表：同一主机的计数叠加、
// 主机集取并集。必须在 play 收尾时调用（此后该 play 的 stats 不再写入）。
func (e *Executor) mergeStats(stats map[string]*model.Stats) {
	e.statsMu.Lock()
	defer e.statsMu.Unlock()
	if e.totalStats == nil {
		e.totalStats = map[string]*model.Stats{}
	}
	for host, s := range stats {
		if s == nil {
			continue
		}
		total := e.totalStats[host]
		if total == nil {
			total = &model.Stats{}
			e.totalStats[host] = total
		}
		total.Ok += s.Ok
		total.Changed += s.Changed
		total.Failed += s.Failed
		total.Unreachable += s.Unreachable
		total.Skipped += s.Skipped
		total.Ignored += s.Ignored
	}
}
