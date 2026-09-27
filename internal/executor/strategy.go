package executor

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"wdp/internal/conn"
	"wdp/internal/model"
	"wdp/internal/shellquote"
)

// rollbackHostBudget 是单主机自动回滚的总时长预算：恢复动作
// （mv/cp -a/rm -rf）常见秒级完成，120s 覆盖慢盘/大目录场景。
const rollbackHostBudget = 120 * time.Second

// snapshotCleanupBudget 是单台目标主机快照清理的时长预算（rm -rf 快照
// 目录），与 rollbackHostBudget 同一"每主机独立"原则。
const snapshotCleanupBudget = 30 * time.Second

// journalActionTimeoutMs 是回滚/清理动作的单次远端执行超时（毫秒）。
const journalActionTimeoutMs = 30_000

// parseBatchSize 解析 batch 表达式："10%"（百分比，向上取整）或 "3"（绝对数）。
// 空/非法时回退 25%（min 1）。
func parseBatchSize(batch string, total int) int {
	s := strings.TrimSpace(batch)
	if s == "" {
		return defaultBatchSize(total)
	}
	if before, ok := strings.CutSuffix(s, "%"); ok {
		p, err := strconv.Atoi(strings.TrimSpace(before))
		// 0% 也回退默认：0/100 向上取整后 max(...,1) 会静默变成逐台批次，
		// 与空串/非法值共用回退语义才不自相矛盾
		if err != nil || p <= 0 {
			return defaultBatchSize(total)
		}
		size := max(
			// 向上取整
			(total*p+99)/100, 1)
		return size
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 {
		return defaultBatchSize(total)
	}
	if n > total {
		n = total
	}
	return n
}

func defaultBatchSize(total int) int {
	if total < 4 {
		return total // 少量主机一批到位
	}
	size := (total + 3) / 4 // 25%
	return size
}

// chunkHosts 按大小切批。
func chunkHosts(hosts []*model.Host, size int) [][]*model.Host {
	if size < 1 {
		size = 1
	}
	var out [][]*model.Host
	for chunk := range slices.Chunk(hosts, size) {
		out = append(out, chunk)
	}
	return out
}

// runGate 在批次主机上执行健康门任务（复用 until 轮询机制），返回是否未通过。
func (e *Executor) runGate(ctx context.Context, p *model.Play, gate *model.Task, runs []*hostRun, stats map[string]*model.Stats) bool {
	alive := make([]*hostRun, 0, len(runs))
	for _, hr := range runs {
		if hr.alive {
			alive = append(alive, hr)
		}
	}
	if len(alive) == 0 {
		return true
	}
	e.Rep.TaskStart("health-gate (gate)", gate.Module)
	results := e.fanOut(ctx, p, gate, alive)
	gateFailed := false
	for _, r := range results {
		e.recordResult(r.hr, r.res, stats, false)
		e.Rep.HostResult(r.hr.host.Name, r.res)
		if r.res.Failed || r.res.Unreachable {
			gateFailed = true
			// 健康门失败的主机处于未知状态：标记死亡，后续 play 不再在其上执行
			r.hr.alive = false
			e.markDead(r.hr.host.Name)
		}
	}
	e.Rep.TaskDone()
	return gateFailed
}

// rollbackBatch 按变更日志逆序回滚一批主机（快照恢复/新建删除）。
// 覆盖文件类变更（copy/template/file）；shell 等过程性变更无法自动回滚。
// 每条动作打到其实际执行主机上（delegate_to 时快照在被委托主机）。
// 用独立的限时 Background ctx：自动回滚最常见的触发场景就是执行被
// 取消（Ctrl+C）或批次失败——沿用已取消的父 ctx 会让回滚本身必然
// 全部失败，恰与该功能承诺兜底的场景相反（同 finishPlay 的处理）。
// 限时预算每主机独立：共用总预算时大批次排在后面的主机回滚必然因
// 预算耗尽失败——恰好发生在 auto_rollback 承诺兜底的场景。
// 刻意保持串行：回滚是失败路径上的兜底，不与主流程争连接配额。
func (e *Executor) rollbackBatch(_ context.Context, runs []*hostRun, stats map[string]*model.Stats) {
	rolled, rollFailed := 0, 0
	for _, hr := range runs {
		hr.mu.Lock()
		acts := append([]journalEntry{}, hr.journal...)
		gaps := append([]string{}, hr.rollbackGaps...)
		hr.mu.Unlock()
		// 快照失败的路径没有还原依据：即便 journal 为空也必须算作回滚
		// 不完整（否则"没登记动作"会被当成"没有需要回滚的变更"）
		if len(acts) == 0 && len(gaps) == 0 {
			continue
		}
		if e.rollbackHostRun(hr, acts, gaps, stats) {
			rolled++
		} else {
			rollFailed++
		}
	}
	if rollFailed > 0 {
		e.Rep.PlayMsg("auto rollback finished: %d hosts restored, %d hosts FAILED (manual check required); procedural changes like shell cannot be auto-rolled-back", rolled, rollFailed)
	} else {
		e.Rep.PlayMsg("auto rollback complete: %d hosts restored from snapshots (procedural changes like shell cannot be auto-rolled-back)", rolled)
	}
}

// rollbackHostRun 回滚单台主机（快照恢复/新建删除），返回该主机是否完整
// 回滚。限时预算的 ctx 在本函数内创建并 defer 释放（防未来在循环体内
// 新增提前 return 时泄漏）。
func (e *Executor) rollbackHostRun(hr *hostRun, acts []journalEntry, gaps []string, stats map[string]*model.Stats) bool {
	for _, g := range gaps {
		res := &model.TaskResult{
			Host: hr.host.Name, Task: "auto-rollback", Module: "rollback",
			Failed: true, Msg: "no snapshot was taken, cannot restore: " + g,
		}
		e.recordResult(hr, res, stats, false)
		e.Rep.HostResult(hr.host.Name, res)
	}
	ctx, cancel := context.WithTimeout(context.Background(), rollbackHostBudget)
	defer cancel()
	hostOK := true
	// 逆序恢复：后发生的变更先回滚
	for _, je := range slices.Backward(acts) {

		a := je.action
		target := je.execOn
		if target == nil {
			target = hr.host
		}
		var script string
		switch a.Kind {
		case "restore":
			// 先删后拷：目标可能已被后续任务重建（目录/文件形态都可能变），
			// `cp -a shadow path` 在 path 已存在（尤其带尾斜杠）时会变成
			// "拷入"——现场变成 path/<basename>，原内容不在原位却报成功。
			// rm -rf 后目标必不存在，cp -a 才是"复原到该路径"的语义。
			script = fmt.Sprintf("rm -rf -- %s && mkdir -p -- %s && cp -a -- %s %s",
				shellquote.Quote(a.Path), shellquote.Quote(pathDir(a.Path)),
				shellquote.Quote(a.Shadow), shellquote.Quote(a.Path))
		case "remove":
			script = fmt.Sprintf("rm -rf -- %s", shellquote.Quote(a.Path))
		default:
			continue
		}
		msg := a.Kind + " " + a.Path
		if target.Name != hr.host.Name {
			msg += " @" + target.Name // 委托产生的变更，标注实际执行主机
		}
		res := &model.TaskResult{
			Host: hr.host.Name, Task: "auto-rollback", Module: "rollback",
			Msg: msg,
		}
		cn, err := e.Conns.Get(ctx, target)
		if err != nil {
			res.Failed = true
			res.Msg += " failed (connection unavailable): " + err.Error()
			hostOK = false
		} else {
			// 与变更发生时同一提权身份执行：非 root 连接用户 + become 的
			// 场景下，快照是 root 属主，不提权则恢复/删除必然权限不足
			out, err := execWithRetry(ctx, cn, conn.ExecRequest{Script: script, TimeoutMs: journalActionTimeoutMs, BecomeUser: je.becomeUser})
			switch {
			case err != nil:
				res.Failed = true
				res.Msg += " failed: " + err.Error()
				hostOK = false
			case out.Code != 0:
				res.Failed = true
				res.Msg += fmt.Sprintf(" failed rc=%d: %s", out.Code, strings.TrimSpace(out.Stderr))
				hostOK = false
			default:
				res.Changed = true
			}
		}
		e.recordResult(hr, res, stats, false)
		e.Rep.HostResult(hr.host.Name, res)
	}
	return hostOK && len(gaps) == 0
}

// cleanupSnapshots 清除登记过回滚动作的主机上的快照目录（best-effort；
// 未产生变更的主机不建连）。delegate_to 产生的变更快照在执行主机上，
// 按动作的执行主机去重清理——去重发生在全部 hostRun 之上：多个主机
// delegate_to 到同一执行主机时，同一快照目录只清一次，计数与播报的
// "hosts" 数才是实际清理的主机数（此前按 hostRun × target 累加，同一
// 目标会被计成 N 台）。
// 限时预算每目标主机独立（与 rollbackBatch 同一原则）：共用总预算时
// 大批次排在后面的主机清理必然因预算耗尽而静默失败——root 属主快照
// 目录（含部署文件副本）残留在远端 /tmp。清理以变更发生时的提权身份
// 执行（该主机任一动作提权即用其用户；多数场景下快照由 root 创建）。
// 刻意保持串行：清理是收尾 best-effort，不与主流程争连接配额。
func (e *Executor) cleanupSnapshots(_ context.Context, runs []*hostRun) {
	script := fmt.Sprintf("rm -rf -- %s", shellquote.Quote(e.rollbackDir))
	targets := map[string]*model.Host{}
	becomeOf := map[string]string{}
	for _, hr := range runs {
		hr.mu.Lock()
		acts := append([]journalEntry{}, hr.journal...)
		hr.mu.Unlock()
		if len(acts) == 0 {
			continue
		}
		for _, je := range acts {
			t := je.execOn
			if t == nil {
				t = hr.host
			}
			targets[t.Name] = t
			if je.becomeUser != "" {
				becomeOf[t.Name] = je.becomeUser
			}
		}
	}
	done := 0
	for _, t := range targets {
		if e.cleanupHostSnapshots(t, becomeOf[t.Name], script) {
			done++
		}
	}
	if done > 0 {
		e.Rep.PlayMsg("rollback snapshots cleaned from %d hosts", done)
	}
}

// cleanupHostSnapshots 清除单台目标主机上的快照目录（best-effort），返回
// 是否成功。限时预算的 ctx 在本函数内创建并 defer 释放。
func (e *Executor) cleanupHostSnapshots(t *model.Host, becomeUser, script string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), snapshotCleanupBudget)
	defer cancel()
	cn, err := e.Conns.Get(ctx, t)
	if err != nil {
		return false
	}
	out, bad := execWithRetry(ctx, cn, conn.ExecRequest{Script: script, TimeoutMs: journalActionTimeoutMs, BecomeUser: becomeUser})
	return bad == nil && out.Code == 0
}

// execWithRetry 传输级失败重试一次的远端执行（回滚/清理动作共用）：
// 批次失败/取消恰是连接最可能已断的时刻，回滚动作（mv/cp -a/rm -rf）
// 与清理动作 rm -rf 都幂等可重放；连接层会在失败后作废底层连接，
// 重试即隐式重建。
func execWithRetry(ctx context.Context, cn conn.Conn, req conn.ExecRequest) (conn.ExecResult, error) {
	out, err := cn.Exec(ctx, req)
	if err != nil {
		out, err = cn.Exec(ctx, req)
	}
	return out, err
}

func pathDir(p string) string {
	// 尾斜杠会让 LastIndexByte 指到末尾空段，dirname 退化成路径本身
	// （mkdir -p 预建目标 → cp -a 变"拷入"）；统一先剥掉
	p = strings.TrimRight(p, "/")
	if p == "" {
		return "/"
	}
	if i := strings.LastIndexByte(p, '/'); i > 0 {
		return p[:i]
	}
	return "/"
}
