package web

// 主机 facts 详情：对单台主机跑 setup 模块（只读采集）并返回结果。
// 采集逻辑复用引擎的 setup 模块本体（模块改动自动同步，不在 web 侧
// 复制脚本）；连接走 agent 通道——探活定传输（https+mTLS 优先、明文
// 回退），与远程执行同一条路。

import (
	"context"
	"net/http"
	"time"

	"wdp/internal/conn"
	"wdp/internal/conn/agentc"
	"wdp/internal/model"
	"wdp/internal/module"
)

// HostFactsResponse 是主机详情的完整快照（探活 + facts）。
type HostFactsResponse struct {
	Probe ProbeResult    `json:"probe"`
	Facts map[string]any `json:"facts,omitempty"`
	Error string         `json:"error,omitempty"` // facts 采集失败原因（探活结果仍在 probe）
}

// handleHostFacts 采集单台主机的 facts（setup 模块，只读）。
func (s *Server) handleHostFacts(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	h, ok := s.checkHost(w, r, verbHostView, id)
	if !ok {
		return
	}
	resp := HostFactsResponse{Probe: probeHost(r.Context(), h, s.probeClientFor(h))}
	if resp.Probe.Status != "online" {
		resp.Error = "agent 不可达：" + resp.Probe.Error
		writeJSON(w, http.StatusOK, resp)
		return
	}

	setup, found := module.Get("setup")
	if !found {
		writeError(w, http.StatusInternalServerError, "setup module not registered")
		return
	}
	dc := &conn.Defaults{Conn: "agent"}
	ac := agentc.New(s.agentHostModel(h), dc)
	defer ac.Close()
	cctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	rc := &module.RunContext{Ctx: cctx, Conn: ac, Host: &model.Host{Name: h.Name, Address: h.Address, AgentPort: h.AgentPort}}
	res := setup.Run(rc, nil, "")
	if res.Failed {
		resp.Error = res.Msg
		writeJSON(w, http.StatusOK, resp)
		return
	}
	resp.Facts = res.Facts
	writeJSON(w, http.StatusOK, resp)
}
