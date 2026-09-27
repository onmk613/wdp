package console

// 远程命令执行的领域编排：并发（[run].forks，缺省 5）在主机上执行脚本、
// 逐主机结果落 run_tasks。传输侧的 host 模型构建（mTLS scheme 探测）经
// 回调注入，SSE 通知经 notify 注入。

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"wdp/internal/config"
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
	// Logger 可选的告警通道（run_tasks 落库失败等审计面异常）；nil 时
	// 退回 slog.Default()，保证未接线的调用方也有输出。
	Logger *slog.Logger
}

// logger 归一日志句柄（见 Logger 字段注释）。
func (e *ExecService) logger() *slog.Logger {
	if e.Logger != nil {
		return e.Logger
	}
	return slog.Default()
}

// ExecOnHosts 并发（[run].forks，缺省 5）在主机上执行脚本；逐主机结果落
// run_tasks，每条落库后调用 notify（web 侧发 SSE 事件）。结果按 hosts
// 声明序返回——并发完成序不影响 API 响应次序。
func (e *ExecService) ExecOnHosts(ctx context.Context, hosts []*store.Host, script string, timeoutSec int, runID int64, notify func()) []ExecHostResult {
	// timeoutSec<=0 会让 context.WithTimeout(ctx, 0) 立即到期——每台主机
	// 必失败。钳制到 web 层的默认值 120s（web.ExecRequest 的 timeout_sec
	// 注释口径），防御调用方漏归一。
	if timeoutSec <= 0 {
		timeoutSec = 120
	}
	var (
		mu      sync.Mutex
		wg      sync.WaitGroup
		sem     = make(chan struct{}, config.Current().Forks())
		results = make([]ExecHostResult, len(hosts))
		dc      = &conn.Defaults{Conn: "agent"}
	)
	for i, h := range hosts {
		wg.Add(1)
		go func(i int, h *store.Host) {
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
			// 临界区内只做按下标写入（各 goroutine 写各自下标，锁仅为
			// 保证切片写入的可见性）：落库与 SSE 通知是可能阻塞的 I/O，
			// 留在锁内会把并发执行串行化在审计写上
			mu.Lock()
			if err != nil {
				res.Err = err.Error()
			} else {
				res.Code, res.Stdout, res.Stderr = out.Code, out.Stdout, out.Stderr
			}
			results[i] = res
			mu.Unlock()
			status := "ok"
			detail := ""
			if res.Err != "" {
				status = "unreachable"
				detail = res.Err
			} else if out.Code != 0 {
				status = "failed"
				detail = res.Stderr
			}
			// run_tasks 是执行审计面：落库失败（磁盘满/库锁）不能让执行
			// 结果丢失，但也不能无声——至少留告警供事后核对
			if terr := e.Store.AddRunTask(&store.RunTask{
				RunID: runID, Play: "exec", Task: "script", Module: "shell",
				Host: h.Name, Status: status, Detail: Truncate(detail+"\n--- stdout ---\n"+res.Stdout, 16<<10),
			}); terr != nil {
				e.logger().Warn("run_tasks audit write failed",
					"run_id", runID, "host", h.Name, "err", terr)
			}
			if notify != nil {
				notify()
			}
		}(i, h)
	}
	wg.Wait()
	return results
}
