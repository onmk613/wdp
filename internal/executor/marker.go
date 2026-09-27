package executor

import (
	"bytes"
	"context"
	"fmt"
	"sync"

	"wdp/internal/chart"
	"wdp/internal/conn"
	"wdp/internal/model"
	"wdp/internal/shellquote"
)

// markerExecTimeoutMs 是 marker 目录预建/清除脚本的单次远端执行超时：
// 只跑 mkdir/rm 一类本地快操作，10s 足够，不让单台故障主机拖住整个
// run 的收尾（marker 处置在 play 全部任务之后）。
const markerExecTimeoutMs = 10_000

// writeMarkers 部署成功后写 release marker（best-effort：失败仅告警不中断）。
// 逐主机经 forks 有界并发（千台规模下串行 Get→mkdir→Upload 的尾延迟
// 明显）；每台独立判定，成功台数聚合后播报，口径与串行版一致。
func (e *Executor) writeMarkers(ctx context.Context, hosts []*model.Host, ch *chart.Chart) {
	if !ch.MarkerEnabled() {
		return
	}
	path := ch.MarkerPath()
	content, err := ch.MarkerContent(e.Opts.WdpVersion, e.Opts.Values, e.Opts.Phase)
	if err != nil {
		// 不落盘空/坏 marker：宁可本轮无 marker（告警可见），不可让
		// uninstall/status/drift 依据损坏
		e.Rep.PlayMsg("warning: release marker content build failed (%v); marker not written", err)
		return
	}
	written := e.markerFanOut(ctx, hosts, func(ctx context.Context, cn conn.Conn) bool {
		if out, bad := cn.Exec(ctx, conn.ExecRequest{
			Script: fmt.Sprintf("mkdir -p -- %s", shellquote.Quote(pathDir(path))), TimeoutMs: markerExecTimeoutMs,
		}); bad != nil || out.Code != 0 {
			return false
		}
		return cn.UploadFile(ctx, path, bytes.NewReader(content), 0o600) == nil
	})
	if written > 0 {
		e.Rep.PlayMsg("release marker written to %d hosts: %s", written, path)
	} else if len(hosts) > 0 {
		e.Rep.PlayMsg("warning: release marker write failed (uninstall/status will be unavailable): %s", path)
	}
}

// removeMarkers 卸载成功后清除 release marker。只删 marker 文件本身，
// 目录用 rmdir 收敛（仅当为空时生效）——绝不对 <marker_dir>/<name> 整体
// rm -rf：name/marker_dir 已在加载入口校验，此处再按"仅文件"收紧，
// 双重防线避免目录内混入无关文件时被连带删除。
func (e *Executor) removeMarkers(ctx context.Context, hosts []*model.Host, ch *chart.Chart) {
	if !ch.MarkerEnabled() {
		return // no_marker chart 从未写 marker，卸载不应执行 rm/rmdir
	}
	marker := ch.MarkerPath()
	script := fmt.Sprintf("rm -f -- %s; rmdir -- %s 2>/dev/null; [ ! -e %s ]",
		shellquote.Quote(marker), shellquote.Quote(pathDir(marker)), shellquote.Quote(marker))
	done := e.markerFanOut(ctx, hosts, func(ctx context.Context, cn conn.Conn) bool {
		out, bad := cn.Exec(ctx, conn.ExecRequest{Script: script, TimeoutMs: markerExecTimeoutMs})
		return bad == nil && out.Code == 0
	})
	if done > 0 {
		e.Rep.PlayMsg("release marker removed from %d hosts", done)
	}
}

// markerFanOut 对逐主机执行 fn（marker 写入/清除共用）：与 fanOut 同一
// 固定 worker 池模式（数量 = forks，存活 worker 恒为 min(forks, 主机数)），
// 每台独立建连与判定，失败仅不计入（best-effort 语义由调用方按成功台数
// 播报）。结果只聚合计数，播报留在主 goroutine（Reporter 实现不承诺
// 并发安全）。主机列表内各主机互不相同，单条连接不会被两个 worker
// 同时复用。
func (e *Executor) markerFanOut(ctx context.Context, hosts []*model.Host, fn func(ctx context.Context, cn conn.Conn) bool) int {
	if len(hosts) == 0 {
		return 0
	}
	workers := max(1, min(e.Opts.Forks, len(hosts)))
	jobs := make(chan *model.Host)
	var wg sync.WaitGroup
	var mu sync.Mutex
	done := 0
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for h := range jobs {
				cn, err := e.Conns.Get(ctx, h)
				if err != nil {
					continue
				}
				if fn(ctx, cn) {
					mu.Lock()
					done++
					mu.Unlock()
				}
			}
		}()
	}
	for _, h := range hosts {
		jobs <- h
	}
	close(jobs)
	wg.Wait()
	return done
}
