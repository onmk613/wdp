package web

// 池/组/标签注册表端点、主机批量操作与"删除即退役"。
//
// 删除即退役：DELETE 主机前先向该机 agent 发 POST /shutdown（mTLS ctl
// 客户端优先，明文 http 兜底）——agent 自清理二进制/证书/systemd 单元。
// agent 不可达时不阻断台账删除，只在结果里带 warning。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"wdp/internal/store"
)

// ---- 池 ----

func (s *Server) handleListPools(w http.ResponseWriter, _ *http.Request) {
	list, err := s.st.ListPools()
	if err != nil {
		s.writeInternal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleCreatePool(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name    string  `json:"name"`
		Note    string  `json:"note"`
		HostIDs []int64 `json:"host_ids"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	id, err := s.st.CreatePool(req.Name, req.Note, req.HostIDs)
	if err != nil {
		// store 层已把重名/命名校验翻译为可读错误（dupErr/validScopeName），
		// 均为用户输入问题，直接 400
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.audit(r, "create", "pool", req.Name, fmt.Sprintf("%d 台主机划入", len(req.HostIDs)))
	writeJSON(w, http.StatusCreated, map[string]any{"id": id, "name": req.Name})
}

func (s *Server) handleDeletePool(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := s.st.DeletePool(id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "pool not found")
			return
		}
		s.writeInternal(w, err)
		return
	}
	s.audit(r, "delete", "pool", fmt.Sprint(id), "成员归属一并解除")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---- 组 ----

func (s *Server) handleListGroups(w http.ResponseWriter, _ *http.Request) {
	list, err := s.st.ListGroups()
	if err != nil {
		s.writeInternal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleCreateGroup(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name    string  `json:"name"`
		Note    string  `json:"note"`
		HostIDs []int64 `json:"host_ids"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	id, err := s.st.CreateGroup(req.Name, req.Note, req.HostIDs)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.audit(r, "create", "group", req.Name, fmt.Sprintf("%d 台主机划入", len(req.HostIDs)))
	writeJSON(w, http.StatusCreated, map[string]any{"id": id, "name": req.Name})
}

func (s *Server) handleDeleteGroup(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := s.st.DeleteGroup(id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "group not found")
			return
		}
		s.writeInternal(w, err)
		return
	}
	s.audit(r, "delete", "group", fmt.Sprint(id), "成员归属一并解除")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---- 标签 ----

func (s *Server) handleListLabels(w http.ResponseWriter, _ *http.Request) {
	list, err := s.st.ListLabels()
	if err != nil {
		s.writeInternal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleCreateLabel(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Key     string            `json:"key"`
		Note    string            `json:"note"`
		Value   string            `json:"value"`
		HostIDs []int64           `json:"host_ids"`
		Labels  map[string]string `json:"labels"` // 兼容一次多键
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Key == "" && len(req.Labels) == 0 {
		writeError(w, http.StatusBadRequest, "label key is required")
		return
	}
	if req.Key == "" { // 多键路径：逐键注册（与单键路径同口径逐键审计）
		for k, v := range req.Labels {
			if _, err := s.st.CreateLabel(k, req.Note, req.HostIDs, v); err != nil {
				writeError(w, http.StatusBadRequest, err.Error())
				return
			}
			s.audit(r, "create", "label", k, v)
		}
		writeJSON(w, http.StatusCreated, map[string]any{"ok": true})
		return
	}
	id, err := s.st.CreateLabel(req.Key, req.Note, req.HostIDs, req.Value)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.audit(r, "create", "label", req.Key, req.Value)
	writeJSON(w, http.StatusCreated, map[string]any{"id": id, "key": req.Key})
}

func (s *Server) handleDeleteLabel(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := s.st.DeleteLabel(id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "label not found")
			return
		}
		s.writeInternal(w, err)
		return
	}
	s.audit(r, "delete", "label", fmt.Sprint(id), "从主机移除该键")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---- 删除即退役 ----

// retireAgent 通知 agent 自清理退出。mTLS（ctl 客户端证书）优先，明文
// http 兜底（手工添加的回环 agent）。返回是否成功退役与失败原因。
func (s *Server) retireAgent(ctx context.Context, h *store.Host) (bool, string) {
	if s.cam != nil {
		if err := postShutdown(ctx, s.cam.tlsClient, "https", h); err == nil {
			return true, ""
		}
	}
	if err := postShutdown(ctx, plainProbeClient(), "http", h); err != nil {
		return false, err.Error()
	}
	return true, ""
}

func postShutdown(ctx context.Context, client *http.Client, scheme string, h *store.Host) error {
	ctx2, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	url := fmt.Sprintf("%s://%s/shutdown", scheme, hostPort(h))
	req, err := http.NewRequestWithContext(ctx2, http.MethodPost, url, bytes.NewReader([]byte("{}")))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("agent shutdown: HTTP %d", resp.StatusCode)
	}
	return nil
}

// handleDeleteHost 升级：先退役 agent（不可达仅告警），再删台账。
func (s *Server) handleDeleteHost(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	h, err := s.st.GetHost(id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "host not found")
		return
	}
	if err != nil {
		s.writeInternal(w, err)
		return
	}
	retired, warn := s.retireAgent(r.Context(), h)
	if err := s.st.DeleteHost(id); err != nil {
		s.writeInternal(w, err)
		return
	}
	// 主机已删：差分快照与执行闸门锁一并回收
	s.monitor.Forget(id)
	s.gate.Forget(id)
	s.logger.Info("host deleted", "name", h.Name, "address", h.Address, "agent_retired", retired)
	s.audit(r, "delete", "host", h.Name, fmt.Sprintf("agent 退役=%v 地址 %s", retired, h.Address))
	resp := map[string]any{"ok": true, "name": h.Name, "agent_retired": retired}
	if warn != "" {
		resp["warning"] = "agent 退役失败（已从台账删除）: " + warn
	}
	writeJSON(w, http.StatusOK, resp)
}

// ---- 批量操作 ----

// BatchRequest 批量动作。assign 的 pools/groups 为 null=不改、非 null 即
// 整体替换（空数组=清除）；labels 追加合并（replace_labels=true 时整体替换）。
type BatchRequest struct {
	IDs           []int64           `json:"ids"`
	Action        string            `json:"action"` // probe | delete | assign
	Pools         []string          `json:"pools"`
	Groups        []string          `json:"groups"`
	SetPools      bool              `json:"set_pools"`
	SetGroups     bool              `json:"set_groups"`
	Labels        map[string]string `json:"labels"`
	ReplaceLabels bool              `json:"replace_labels"`
}

// BatchResult 单主机结果（Row 仅批量建档时填写，= CSV 行号，0 起）。
type BatchResult struct {
	Row     int    `json:"row,omitempty"`
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	OK      bool   `json:"ok"`
	Detail  string `json:"detail,omitempty"`
	Retired bool   `json:"retired,omitempty"`
}

func (s *Server) handleBatchHosts(w http.ResponseWriter, r *http.Request) {
	var req BatchRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if len(req.IDs) == 0 {
		writeError(w, http.StatusBadRequest, "ids is empty")
		return
	}
	// 按 action 定权限：probe=view / assign=edit（均按主机作用域）/
	// delete=host:delete（全局）；越权主机逐台标失败不中断
	p := s.permsOf(permUser(r))
	var needVerb string
	switch req.Action {
	case "probe":
		needVerb = verbHostView
	case "assign":
		needVerb = verbHostEdit
	case "delete":
		needVerb = verbHostDelete
	default:
		writeError(w, http.StatusBadRequest, "unknown action "+req.Action)
		return
	}
	if !p.canVerb(needVerb) {
		writeError(w, http.StatusForbidden, "forbidden: requires "+needVerb)
		return
	}
	// assign 的目标归属是写入语义：scope 级 host:edit 只能改到授权覆盖的
	// 池/组（matchScope"任一命中"不够——那会把主机挪进任意池扩大可见面）
	if req.Action == "assign" {
		var tp, tg []string
		if req.SetPools {
			tp = req.Pools
		}
		if req.SetGroups {
			tg = req.Groups
		}
		if !s.scopeWriteAllowed(r, verbHostEdit, tp, tg, nil) {
			writeError(w, http.StatusForbidden, "forbidden: assign target pools/groups outside your host:edit scope")
			return
		}
	}
	results := make([]BatchResult, 0, len(req.IDs))
	for _, id := range req.IDs {
		h, err := s.st.GetHost(id)
		if err != nil {
			results = append(results, BatchResult{ID: id, OK: false, Detail: "host not found"})
			continue
		}
		if !p.canHost(needVerb, h.Pools, h.Groups, h.Labels) {
			results = append(results, BatchResult{ID: id, Name: h.Name, OK: false, Detail: "forbidden: outside your scope"})
			continue
		}
		res := BatchResult{ID: id, Name: h.Name}
		switch req.Action {
		case "probe":
			pr := probeHost(r.Context(), h, s.probeClientFor(h))
			_ = s.st.SetHostStatus(id, pr.Status)
			res.OK = pr.Status == "online"
			res.Detail = pr.Status + " " + pr.Error
		case "delete":
			retired, warn := s.retireAgent(r.Context(), h)
			if err := s.st.DeleteHost(id); err != nil {
				res.Detail = err.Error()
				break
			}
			// 主机已删：差分快照与执行闸门锁一并回收
			s.monitor.Forget(id)
			s.gate.Forget(id)
			res.OK = true
			res.Retired = retired
			if warn != "" {
				res.Detail = "agent 退役失败: " + warn
			}
		case "assign":
			n, err := s.st.BatchAssign([]int64{id}, req.Pools, req.Groups, req.SetPools, req.SetGroups, req.Labels, req.ReplaceLabels)
			res.OK = err == nil && n == 1
			if err != nil {
				res.Detail = err.Error()
			}
		default:
			writeError(w, http.StatusBadRequest, "unknown action "+req.Action)
			return
		}
		results = append(results, res)
	}
	okN := 0
	for _, res := range results {
		if res.OK {
			okN++
		}
	}
	s.audit(r, "batch_"+req.Action, "host", fmt.Sprintf("%d 台", len(req.IDs)), fmt.Sprintf("成功 %d / %d", okN, len(results)))
	writeJSON(w, http.StatusOK, map[string]any{"results": results, "ok": okN, "failed": len(results) - okN})
}

// hostPort 供直连 URL 拼接（IPv6 安全）。
func hostPort(h *store.Host) string {
	return net.JoinHostPort(h.Address, fmt.Sprint(h.AgentPort))
}

// ---- 批量建档（CSV 导入的服务端入口）----

// ImportHost 是批量建档的单主机条目（与控制台 CSV 模版列一一对应）。
type ImportHost struct {
	Name      string            `json:"name"`       // 空 = address
	Address   string            `json:"address"`    // 必填
	AgentPort int               `json:"agent_port"` // 0 = 7602
	Pools     []string          `json:"pools"`
	Groups    []string          `json:"groups"`
	Labels    map[string]string `json:"labels"`
}

const importHostsMax = 1000

// handleImportHosts 逐台建档并返回逐台结果（不中断后续行；重名等错误
// 记入 detail）。池/组按名落账（新名自动生效，与单台添加一致）。
func (s *Server) handleImportHosts(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Hosts []ImportHost `json:"hosts"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if len(req.Hosts) == 0 {
		writeError(w, http.StatusBadRequest, "hosts is empty")
		return
	}
	if len(req.Hosts) > importHostsMax {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("too many hosts: %d (max %d)", len(req.Hosts), importHostsMax))
		return
	}
	results := make([]BatchResult, 0, len(req.Hosts))
	for i, ih := range req.Hosts {
		row := BatchResult{Row: i + 1}
		reject := func(detail string) {
			row.Name, row.OK, row.Detail = strings.TrimSpace(ih.Name), false, detail
			results = append(results, row)
		}
		name := strings.TrimSpace(ih.Name)
		if name == "" {
			name = strings.TrimSpace(ih.Address)
		}
		row.Name = name
		// 地址必填；台账名与手工/纳管路径同口径（safeName 字符集，非法
		// 拒绝该行——静默替换会产生查不到的别名）；端口范围 1..65535
		if strings.TrimSpace(ih.Address) == "" {
			reject("address is required")
			continue
		}
		if name != safeName(name) {
			reject("name has invalid characters (allowed: letters, digits, . _ -)")
			continue
		}
		if ih.AgentPort < 0 || ih.AgentPort > 65535 {
			reject("agent_port out of range (1-65535)")
			continue
		}
		labels := "{}"
		if len(ih.Labels) > 0 {
			if b, err := json.Marshal(ih.Labels); err == nil {
				labels = string(b)
			}
		}
		id, err := s.st.CreateHost(&store.Host{
			Name: name, Address: ih.Address, AgentPort: ih.AgentPort,
			Pools: ih.Pools, Groups: ih.Groups, Labels: labels,
		})
		if err != nil {
			row.OK, row.Detail = false, err.Error()
			results = append(results, row)
			continue
		}
		row.ID, row.OK = id, true
		results = append(results, row)
	}
	okN := 0
	for _, res := range results {
		if res.OK {
			okN++
		}
	}
	s.audit(r, "import", "host", fmt.Sprintf("%d 台", len(req.Hosts)), fmt.Sprintf("成功 %d / %d", okN, len(results)))
	writeJSON(w, http.StatusOK, map[string]any{"results": results, "ok": okN, "failed": len(results) - okN})
}
