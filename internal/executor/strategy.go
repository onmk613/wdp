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
		for _, g := range gaps {
			res := &model.TaskResult{
				Host: hr.host.Name, Task: "auto-rollback", Module: "rollback",
				Failed: true, Msg: "no snapshot was taken, cannot restore: " + g,
			}
			e.recordResult(hr, res, stats, false)
			e.Rep.HostResult(hr.host.Name, res)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
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
				out, err := cn.Exec(ctx, conn.ExecRequest{Script: script, TimeoutMs: 30_000, BecomeUser: je.becomeUser})
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
		if hostOK && len(gaps) == 0 {
			rolled++
		} else {
			rollFailed++
		}
		cancel()
	}
	if rollFailed > 0 {
		e.Rep.PlayMsg("auto rollback finished: %d hosts restored, %d hosts FAILED (manual check required); procedural changes like shell cannot be auto-rolled-back", rolled, rollFailed)
	} else {
		e.Rep.PlayMsg("auto rollback complete: %d hosts restored from snapshots (procedural changes like shell cannot be auto-rolled-back)", rolled)
	}
}

// cleanupSnapshots 清除登记过回滚动作的主机上的快照目录（best-effort；
// 未产生变更的主机不建连）。delegate_to 产生的变更快照在执行主机上，
// 按动作的执行主机去重清理。
// 限时预算每主机独立（与 rollbackBatch 同一原则）：共用总预算时大批次
// 排在后面的主机清理必然因预算耗尽而静默失败——root 属主快照目录
// （含部署文件副本）残留在远端 /tmp。清理以变更发生时的提权身份执行
// （该主机任一动作提权即用其用户；多数场景下快照由 root 创建）。
func (e *Executor) cleanupSnapshots(_ context.Context, runs []*hostRun) {
	script := fmt.Sprintf("rm -rf -- %s", shellquote.Quote(e.rollbackDir))
	done := 0
	for _, hr := range runs {
		hr.mu.Lock()
		acts := append([]journalEntry{}, hr.journal...)
		hr.mu.Unlock()
		if len(acts) == 0 {
			continue
		}
		targets := map[string]*model.Host{}
		becomeOf := map[string]string{}
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
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		for _, t := range targets {
			cn, err := e.Conns.Get(ctx, t)
			if err != nil {
				continue
			}
			if out, bad := cn.Exec(ctx, conn.ExecRequest{Script: script, TimeoutMs: 30_000, BecomeUser: becomeOf[t.Name]}); bad == nil && out.Code == 0 {
				done++
			}
		}
		cancel()
	}
	if done > 0 {
		e.Rep.PlayMsg("rollback snapshots cleaned from %d hosts", done)
	}
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
