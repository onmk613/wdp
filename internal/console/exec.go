package console

// 远程命令执行的领域编排：并发（5）在主机上执行脚本、逐主机结果落
// run_tasks。从 web 迁入；传输侧的 host 模型构建（mTLS scheme 探测）经
// 回调注入，SSE 通知经 notify 注入。

import (
	"context"
	"sync"
	"time"

	"wdp/internal/conn"
	"wdp/internal/conn/agentc"
	"wdp/internal/model"
	"wdp/internal/store"
)

// ExecHostResult 一台主机的执行结果（API 响应形态）。
type ExecHostResult struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Code   int    `json:"code"`
	Stdout string `json:"stdout"`
	Stderr string `json:"stderr"`
	Err    string `json:"err,omitempty"`
}

// ExecService 远程命令执行领域服务。
type ExecService struct {
	Store *store.Store
	// HostModel 由传输层注入：台账行 → executor 连接模型（含 mTLS
	// scheme 探活——依赖 server 的 CA 材料，属传输侧）。
	HostModel func(ctx context.Context, h *store.Host) *model.Host
}

// ExecOnHosts 并发（5）在主机上执行脚本；逐主机结果落 run_tasks，每条
// 落库后调用 notify（web 侧发 SSE 事件）。
func (e *ExecService) ExecOnHosts(ctx context.Context, hosts []*store.Host, script string, timeoutSec int, runID int64, notify func()) []ExecHostResult {
	var (
		mu      sync.Mutex
		wg      sync.WaitGroup
		sem     = make(chan struct{}, 5)
		results = make([]ExecHostResult, 0, len(hosts))
		dc      = &conn.Defaults{Conn: "agent"}
	)
	for _, h := range hosts {
		wg.Add(1)
		go func(h *store.Host) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			res := ExecHostResult{ID: h.ID, Name: h.Name}
			// HostModel 内含探活定传输（幂等健康检查，避免执行成功但
			// 响应失败时重试导致重复执行）
			ac := agentc.New(e.HostModel(ctx, h), dc)
			defer ac.Close()
			cctx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second)
			defer cancel()
			out, err := ac.Exec(cctx, conn.ExecRequest{Script: script, TimeoutMs: int64(timeoutSec) * 1000})
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				res.Err = err.Error()
			} else {
				res.Code, res.Stdout, res.Stderr = out.Code, out.Stdout, out.Stderr
			}
			status := "ok"
			detail := ""
			if res.Err != "" {
				status = "unreachable"
				detail = res.Err
			} else if out.Code != 0 {
				status = "failed"
				detail = res.Stderr
			}
			_ = e.Store.AddRunTask(&store.RunTask{
				RunID: runID, Play: "exec", Task: "script", Module: "shell",
				Host: h.Name, Status: status, Detail: Truncate(detail+"\n--- stdout ---\n"+res.Stdout, 16<<10),
			})
			if notify != nil {
				notify()
			}
			results = append(results, res)
		}(h)
	}
	wg.Wait()
	return results
}
