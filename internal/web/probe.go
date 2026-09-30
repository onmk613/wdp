package web

// 探活的传输侧接线：probeHost/ProbeResult/probeLoop 已迁 internal/worker
//（与 HTTP 传输解耦的后台任务）。这里只保留 web 侧的类型别名与注入点，
// facts/inventory/upgrade 等 handler 的调用面不变。

import (
	"context"
	"encoding/json"
	"time"

	"wdp/internal/store"
	"wdp/internal/worker"
)

// ProbeResult 探活结果（worker 包类型的别名：facts 响应与既有测试的引用面不变）。
type ProbeResult = worker.ProbeResult

// probeModulesJSON 把探活顺带抓到的 agent 模块集序列化落库（nil = agent
// 未上报 /info → 空串，SetHostStatus 按「保留最后已知」处理）。
func probeModulesJSON(res worker.ProbeResult) string {
	if res.Modules == nil {
		return ""
	}
	b, err := json.Marshal(res.Modules)
	if err != nil {
		return ""
	}
	return string(b)
}

// probeHost 一次 /health 探测（mTLS 优先、明文回退；实现见 worker 包）。
var probeHost = worker.ProbeHost

// probeEvery 生效探活周期：设置页值优先（>=5s 防误配成打点风暴），
// 回落 flag 缺省。
func (s *Server) probeEvery() time.Duration {
	if d := s.effectiveSettings().ProbeEverySec; d != nil && *d >= 5 {
		return time.Duration(*d) * time.Second
	}
	return s.opts.ProbeEvery
}

// startProber 启动探活循环（停旧起新：设置页改周期后重进即生效）。
func (s *Server) startProber(ctx context.Context) {
	s.proberMu.Lock()
	defer s.proberMu.Unlock()
	if s.proberCancel != nil {
		s.proberCancel()
	}
	pctx, cancel := context.WithCancel(ctx)
	s.proberCancel = cancel
	p := &worker.Prober{
		Store:  s.st,
		Logger: s.logger,
		Every:  s.probeEvery(),
		Probe: func(ctx context.Context, h *store.Host) worker.ProbeResult {
			return worker.ProbeHost(ctx, h, s.probeClientFor(h))
		},
	}
	go p.Run(pctx)
}
