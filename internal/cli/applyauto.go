package cli

// 自治执行的控制端编排（docs/15 §7）：
//
//   - 按 via 中继根分组，每组一个分片（plan.Shard），提交给根主机上的
//     agent（POST /plan 异步，gzip）；agent 对分片中"本机"主机走 selfexec
//     本地执行，其余主机远程执行——同一机制覆盖直连与网段中继（G3）；
//   - 控制端轮询 /plan/status 收集 journal 增量；轮询失败不影响主机侧
//     收敛（G1：断连容忍）；
//   - 旧版 agent 无 /plan 端点（404）→ 回退本控制端直接执行（G4）；
//   - 非 agent 通道不支持自治执行（无常驻进程承载后台收敛）——显式报错
//     并说明原因，不静默降级。
//
// run_id 确定性生成（sha256(plan 分片 PlanID + 根主机名)）：同一分片重复
// 提交命中 agent 幂等；断连恢复后 `wdp apply status` 可重算并回查。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"wdp/internal/config"
	"wdp/internal/conn"
	"wdp/internal/conn/agentc"
	"wdp/internal/executor"
	"wdp/internal/inventory"
	"wdp/internal/model"
	"wdp/internal/plan"
)

// relayGroup 是一个自治提交组：根（agent 所在主机）与其负责的目标主机。
type relayGroup struct {
	root    string
	rootCnn plan.HostConn
	targets []string
}

// groupByRoot 按 via 中继根分组计划主机（无 via 的主机自成一组，根=自身）。
func groupByRoot(p *plan.Plan) ([]relayGroup, error) {
	groups := map[string]*relayGroup{}
	for _, host := range p.Host() {
		hp := p.HostPlansOf(host)[0]
		root := host
		if len(hp.Conn.Via) > 0 {
			root = hp.Conn.Via[len(hp.Conn.Via)-1]
		}
		g := groups[root]
		if g == nil {
			g = &relayGroup{root: root}
			switch root {
			case host:
				g.rootCnn = hp.Conn
			default:
				rc, ok := p.Relays[root]
				if !ok {
					// 根自身也是目标主机（连接元数据在 Hosts 里）
					if rhp := p.HostPlansOf(root); len(rhp) > 0 {
						rc = rhp[0].Conn
					} else {
						return nil, fmt.Errorf("via relay root %s has no connection metadata in the plan (recompile the plan with the relay host in the inventory)", root)
					}
				}
				g.rootCnn = rc
			}
			groups[root] = g
		}
		g.targets = append(g.targets, host)
	}
	out := make([]relayGroup, 0, len(groups))
	for _, g := range groups {
		sort.Strings(g.targets)
		out = append(out, *g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].root < out[j].root })
	return out, nil
}

// filterGroups 按允许的主机集收窄分组的 targets（组内无剩余主机则丢弃该组）。
func filterGroups(groups []relayGroup, allowed map[string]bool) []relayGroup {
	out := make([]relayGroup, 0, len(groups))
	for _, g := range groups {
		var targets []string
		for _, h := range g.targets {
			if allowed[h] {
				targets = append(targets, h)
			}
		}
		if len(targets) == 0 {
			continue
		}
		g.targets = targets
		out = append(out, g)
	}
	return out
}

// autonomousRunID 由分片内容与根主机确定性派生（幂等重提与事后回查的键）。
func autonomousRunID(shard *plan.Plan, root string) string {
	h := sha256.Sum256([]byte(shard.PlanID + ":" + root))
	return hex.EncodeToString(h[:])[:16]
}

// runApplyAutonomous 分组提交分片并轮询进度；返回是否存在失败。
// allowed 非 nil 时只提交其中的主机（--limit）。
func runApplyAutonomous(ctx context.Context, p *plan.Plan, opts applyOptions, allowed map[string]bool) (bool, error) {
	if opts.check {
		return false, fmt.Errorf("--autonomous does not support --check (check mode needs synchronous module probing; run `wdp apply --check` without --autonomous)")
	}
	// 计划携带的是编译期固化的任务清单，agent 侧没有 tags 过滤能力：
	// 静默忽略会让用户以为只跑了部分任务，必须在提交前显式拒绝。
	if len(opts.tags) > 0 || len(opts.skipTags) > 0 {
		return false, fmt.Errorf("--autonomous does not support --tags/--skip-tags (the submitted plan carries the compiled task list; filter at compile time or run `wdp apply` without --autonomous)")
	}
	// 制品不进 plan 本体：agent 侧没有 --chart-dir 可传，payload 缺失时
	// 只有告警，会在收敛中途才失败——离线补件的语义与自治执行不兼容，
	// 必须在提交前拒绝。
	if opts.chartDir != "" && len(p.Payloads) > 0 {
		return false, fmt.Errorf("--chart-dir is not supported with --autonomous (%d payload(s) referenced by the plan cannot be delivered through the control host; distribute them to the target agents first, or run `wdp apply` without --autonomous)", len(p.Payloads))
	}
	groups, err := groupByRoot(p)
	if err != nil {
		return false, err
	}
	if allowed != nil {
		groups = filterGroups(groups, allowed)
		if len(groups) == 0 {
			return false, fmt.Errorf("--limit matched no plan hosts")
		}
	}
	becomePW := ""
	if opts.becomePasswordEnv != "" {
		becomePW = os.Getenv(opts.becomePasswordEnv)
	}

	var subs []autonomousSubmission
	var fallback []relayGroup // 旧版 agent（无 /plan）→ 本控制端直接执行
	anyFailed := false

	for _, g := range groups {
		if g.rootCnn.Conn != "agent" {
			return false, fmt.Errorf("--autonomous requires the agent channel, but relay root %s uses conn %q "+
				"(autonomous execution needs a resident agent to carry background convergence; use ssh/push/local via plain `wdp apply`)", g.root, g.rootCnn.Conn)
		}
		shard := p.Shard(g.targets)
		host := g.rootCnn.Host(g.root)
		client := agentc.New(host, connDefaults())
		runID := autonomousRunID(shard, g.root)

		resp, serr := client.SubmitPlan(ctx, &agentc.PlanSubmit{
			RunID:          runID,
			Plan:           shard,
			LocalHost:      g.root,
			Resume:         opts.resume,
			BecomePassword: becomePW,
			Forks:          config.Current().Forks(),
		})
		switch serr {
		case nil:
			fmt.Fprintf(os.Stderr, "[autonomous] %s: run %s accepted (state=%s%s)\n",
				g.root, shortID(resp.RunID), resp.State, resumedNote(resp.ResumedFromIdx))
			subs = append(subs, autonomousSubmission{g, runID, client})
		case agentc.ErrPlanUnsupported:
			// 旧版 agent（G4 回退：与 handleArchive 404 回退同惯例）
			fmt.Fprintf(os.Stderr, "[autonomous] %s: agent has no /plan endpoint (older agent), falling back to direct execution\n", g.root)
			fallback = append(fallback, g)
		default:
			fmt.Fprintf(os.Stderr, "[autonomous] %s: submit failed: %v\n", g.root, serr)
			anyFailed = true
		}
	}

	// --detach：接受即返回（G1：控制端此刻断开不影响收敛）
	if !opts.detach && len(subs) > 0 {
		ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
		defer stop()
		if pollSubmissions(ctx, subs) {
			anyFailed = true
		}
	} else if len(subs) > 0 {
		fmt.Fprintf(os.Stderr, "[autonomous] detached; poll later with: wdp apply status <plan.json> <run-id>\n")
	}

	// 回退组：本控制端直接执行（含旧版 agent 的主机）
	if len(fallback) > 0 {
		var hosts []string
		for _, g := range fallback {
			hosts = append(hosts, g.targets...)
		}
		if ok := runDirect(ctx, p.Shard(hosts), opts); ok {
			anyFailed = true
		}
	}
	return anyFailed, nil
}

// autonomousSubmission 是一个已接受的自治提交。
type autonomousSubmission struct {
	group  relayGroup
	runID  string
	client *agentc.Conn
}

// pollSubmissions 轮询全部提交直到终态，流式打印 journal 增量。
func pollSubmissions(ctx context.Context, subs []autonomousSubmission) bool {
	cursors := map[string]int64{}
	anyFailed := false
	pending := map[string]bool{}
	for _, s := range subs {
		pending[s.runID] = true
	}
	for len(pending) > 0 {
		if ctx.Err() != nil {
			fmt.Fprintf(os.Stderr, "[autonomous] polling cancelled (%v); agent-side convergence continues\n", ctx.Err())
			return true
		}
		for _, s := range subs {
			if !pending[s.runID] {
				continue
			}
			pctx, cancel := context.WithTimeout(ctx, 15*time.Second)
			st, err := s.client.PlanStatus(pctx, s.runID, cursors[s.runID])
			cancel()
			if err != nil {
				// 轮询失败不影响主机侧收敛（G1）；退避后重试
				fmt.Fprintf(os.Stderr, "[autonomous] %s: status poll failed (%v), retrying\n", s.group.root, err)
				continue
			}
			for _, e := range st.Journal {
				cursors[s.runID] = e.Seq
				printJournalLine(s.group.root, e)
			}
			switch st.State {
			case "done":
				delete(pending, s.runID)
				fmt.Fprintf(os.Stderr, "[autonomous] %s: run %s done\n", s.group.root, shortID(s.runID))
			case "failed", "cancelled":
				delete(pending, s.runID)
				anyFailed = true
				fmt.Fprintf(os.Stderr, "[autonomous] %s: run %s %s%s\n", s.group.root, shortID(s.runID), st.State, errNote(st.Error))
			}
		}
		if len(pending) > 0 {
			sleep(ctx, 2*time.Second)
		}
	}
	return anyFailed
}

// runDirect 本控制端直接执行一个分片（回退路径）。
func runDirect(ctx context.Context, shard *plan.Plan, opts applyOptions) bool {
	var hosts []*model.Host
	seen := map[string]bool{}
	for _, name := range shard.Host() {
		if seen[name] {
			continue
		}
		seen[name] = true
		hosts = append(hosts, shard.HostPlansOf(name)[0].Conn.Host(name))
	}
	inv := inventory.FromHosts(hosts)
	rep, finish := buildReporter()
	conns := conn.NewManagerWithDefaults(connDefaults())
	conns.SetConnectConcurrency(2 * config.Current().Forks())
	ex := executor.New(inv, conns, rep, executor.Options{
		Forks:            config.Current().Forks(),
		Phase:            shard.Phase,
		WdpVersion:       Version,
		PayloadDir:       opts.chartDir,
		MaxDownloadBytes: maxDownloadBytes(),
	})
	failed := ex.RunPlan(ctx, shard)
	conns.CloseAll()
	finish()
	return failed
}

// runApplyStatus 回查自治执行进度（确定性 run_id 由计划与根重算）。
// --limit 收窄到与提交时同一口径的主机集合：只看部分主机的进度时，
// 未被选中的 relay 分组不再查询（此前 flag 声明了却不生效）。
func runApplyStatus(ctx context.Context, planPath, runID string, opts applyOptions) error {
	p, err := plan.Load(planPath)
	if err != nil {
		return err
	}
	groups, err := groupByRoot(p)
	if err != nil {
		return err
	}
	if opts.limit != "" {
		hosts := make([]*model.Host, 0, len(p.Host()))
		for _, name := range p.Host() {
			hosts = append(hosts, p.HostPlansOf(name)[0].Conn.Host(name))
		}
		limited, lerr := inventory.FromHosts(hosts).Select(opts.limit)
		if lerr != nil {
			return lerr
		}
		if len(limited) == 0 {
			return fmt.Errorf("--limit %s matched no plan hosts", opts.limit)
		}
		allowed := make(map[string]bool, len(limited))
		for _, h := range limited {
			allowed[h.Name] = true
		}
		groups = filterGroups(groups, allowed)
		if len(groups) == 0 {
			return fmt.Errorf("--limit %s matched no plan hosts", opts.limit)
		}
	}
	found := false
	var notOK []string
	for _, g := range groups {
		if g.rootCnn.Conn != "agent" {
			continue
		}
		shard := p.Shard(g.targets)
		id := autonomousRunID(shard, g.root)
		if !strings.HasPrefix(id, runID) {
			continue
		}
		found = true
		client := agentc.New(g.rootCnn.Host(g.root), connDefaults())
		st, serr := client.PlanStatus(ctx, id, 0)
		if serr != nil {
			fmt.Fprintf(os.Stderr, "[autonomous] %s: status query failed: %v\n", g.root, serr)
			notOK = append(notOK, fmt.Sprintf("%s (status query failed)", shortID(id)))
			continue
		}
		fmt.Printf("run %s @ %s  state=%s%s\n", shortID(id), g.root, st.State, errNote(st.Error))
		// 终态失败必须反映到退出码：回查命令此前恒返回 0，CI 无法据此判失败
		switch st.State {
		case "failed", "cancelled":
			notOK = append(notOK, fmt.Sprintf("%s (%s)", shortID(id), st.State))
		}
		for _, e := range st.Journal {
			printJournalLine(g.root, agentc.PlanJournalEntry{
				Seq: e.Seq, Host: e.Host, Idx: e.Idx, Label: e.Label,
				State: e.State, Changed: e.Changed, Msg: e.Msg, At: e.At,
			})
		}
	}
	if !found {
		return fmt.Errorf("no autonomous run matching %q (runs are keyed by plan content and relay root; recompile produces new ids)", runID)
	}
	if len(notOK) > 0 {
		return fmt.Errorf("autonomous run(s) not successful: %s", strings.Join(notOK, ", "))
	}
	return nil
}

func printJournalLine(root string, e agentc.PlanJournalEntry) {
	marker := ""
	switch e.State {
	case "changed":
		marker = "*"
	case "failed", "unreachable":
		marker = "!"
	case "skipped":
		marker = "-"
	}
	line := fmt.Sprintf("  [%s] %s #%d %s %s", root, e.Host, e.Idx, marker, e.Label)
	if e.Msg != "" && (e.State == "failed" || e.State == "unreachable") {
		line += " — " + e.Msg
	}
	fmt.Println(line)
}

func resumedNote(m map[string]int) string {
	if len(m) == 0 {
		return ""
	}
	var parts []string
	for _, h := range sortedHostsOf(m) {
		parts = append(parts, fmt.Sprintf("%s:+%d", h, m[h]))
	}
	return ", resumed " + strings.Join(parts, " ")
}

func sortedHostsOf(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for h := range m {
		out = append(out, h)
	}
	sort.Strings(out)
	return out
}

func errNote(errMsg string) string {
	if errMsg == "" {
		return ""
	}
	return " (" + errMsg + ")"
}

func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
