package web

// 探活的传输侧接线：probeHost/ProbeResult/probeLoop 已迁 internal/worker
//（与 HTTP 传输解耦的后台任务）。这里只保留 web 侧的类型别名与注入点，
// facts/inventory/upgrade 等 handler 的调用面不变。

import (
	"context"

	"wdp/internal/store"
	"wdp/internal/worker"
)

// ProbeResult 探活结果（worker 包类型的别名：facts 响应与既有测试的引用面不变）。
type ProbeResult = worker.ProbeResult

// probeHost 一次 /health 探测（mTLS 优先、明文回退；实现见 worker 包）。
var probeHost = worker.ProbeHost

// startProber 启动探活循环（Run 时调用；探测回调带上控制端 mTLS 客户端）。
func (s *Server) startProber(ctx context.Context) {
	p := &worker.Prober{
		Store:  s.st,
		Logger: s.logger,
		Every:  s.opts.ProbeEvery,
		Probe: func(ctx context.Context, h *store.Host) worker.ProbeResult {
			return worker.ProbeHost(ctx, h, s.probeClientFor(h))
		},
	}
	go p.Run(ctx)
}
