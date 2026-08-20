package executor

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"wdp/internal/connection"
	"wdp/internal/model"
	"wdp/internal/module"
	"wdp/internal/shellquote"
)

// parseBatchSize 解析 batch 表达式："10%"（百分比，向上取整）或 "3"（绝对数）。
// 空/非法时回退 25%（min 1）。
func parseBatchSize(batch string, total int) int {
	s := strings.TrimSpace(batch)
	if s == "" {
		return defaultBatchSize(total)
	}
	if strings.HasSuffix(s, "%") {
		p, err := strconv.Atoi(strings.TrimSpace(strings.TrimSuffix(s, "%")))
		if err != nil || p < 0 {
			return defaultBatchSize(total)
		}
		size := (total*p + 99) / 100 // 向上取整
		if size < 1 {
			size = 1
		}
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
	for i := 0; i < len(hosts); i += size {
		end := i + size
		if end > len(hosts) {
			end = len(hosts)
		}
		out = append(out, hosts[i:end])
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
		}
	}
	e.Rep.TaskDone()
	return gateFailed
}

// rollbackBatch 按变更日志逆序回滚一批主机（快照恢复/新建删除）。
// 覆盖文件类变更（copy/template/file）；shell 等过程性变更无法自动回滚。
func (e *Executor) rollbackBatch(ctx context.Context, p *model.Play, runs []*hostRun, stats map[string]*model.Stats) {
	rolled := 0
	for _, hr := range runs {
		hr.mu.Lock()
		acts := append([]module.RollbackAction{}, hr.journal...)
		hr.mu.Unlock()
		if len(acts) == 0 {
			continue
		}
		conn, err := e.Conns.Get(ctx, hr.host)
		if err != nil {
			e.Rep.PlayMsg("%s 回滚失败（连接不可用）: %v", hr.host.Name, err)
			continue
		}
		rolled++
		// 逆序恢复：后发生的变更先回滚
		for i := len(acts) - 1; i >= 0; i-- {
			a := acts[i]
			var script string
			switch a.Kind {
			case "restore":
				script = fmt.Sprintf("mkdir -p -- %s && cp -a -- %s %s",
					shellquote.Quote(pathDir(a.Path)), shellquote.Quote(a.Shadow), shellquote.Quote(a.Path))
			case "remove":
				script = fmt.Sprintf("rm -rf -- %s", shellquote.Quote(a.Path))
			default:
				continue
			}
			res := &model.TaskResult{
				Host: hr.host.Name, Task: "auto-rollback", Module: "rollback",
				Msg: a.Kind + " " + a.Path,
			}
			out, err := conn.Exec(ctx, connection.ExecRequest{Script: script, TimeoutMs: 30_000})
			switch {
			case err != nil:
				res.Failed = true
				res.Msg += " 失败: " + err.Error()
			case out.Code != 0:
				res.Failed = true
				res.Msg += fmt.Sprintf(" 失败 rc=%d: %s", out.Code, strings.TrimSpace(out.Stderr))
			default:
				res.Changed = true
			}
			e.recordResult(hr, res, stats, false)
			e.Rep.HostResult(hr.host.Name, res)
		}
	}
	e.Rep.PlayMsg("自动回滚完成：%d 台主机按快照恢复（过程性变更如 shell 无法自动回滚）", rolled)
}

func pathDir(p string) string {
	if i := strings.LastIndexByte(p, '/'); i > 0 {
		return p[:i]
	}
	return "/"
}
