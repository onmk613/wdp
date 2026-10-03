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
	"slices"
	"strings"
	"time"

	"wdp/internal/store"
	"wdp/internal/worker"
)

// ---- 池 / 组 / 标签注册表（同构 CRUD 收敛）----

// registryResource 描述一类注册表资源的 CRUD 差异点：池/组的
// List/Create/Delete 与标签的 List/Delete 完全同构，收敛为按表分发的
// 通用实现（见 registryList/registryCreate/registryDelete）。路由注册与
// 权限校验不动（routes.go），六个具名 handler 退化为薄委托。
//
// 标签的 Create 不入表：键值语义（key/value + 多键兼容路径）与响应形态
// （{"id","key"} / 多键 {"ok":true}）都不同，硬塞进通用实现需要按资源
// 类型开特例分支，可读性反而低于现状，故保留专用 handleCreateLabel。
type registryResource struct {
	auditName string // 审计对象类型（pool/group/label）
	list      func(st *store.Store) (any, error)
	create    func(st *store.Store, name, note string, hostIDs []int64) (int64, error) // 标签为 nil（专用 handler）
	del       func(st *store.Store, id int64) error
	delDetail string // 删除审计附注（各资源成员/归属联动语义不同）
}

var (
	registryPool = registryResource{
		auditName: "pool",
		list:      func(st *store.Store) (any, error) { return st.ListPools() },
		create: func(st *store.Store, name, note string, ids []int64) (int64, error) {
			return st.CreatePool(name, note, ids)
		},
		del:       func(st *store.Store, id int64) error { return st.DeletePool(id) },
		delDetail: "成员归属一并解除",
	}
	registryGroup = registryResource{
		auditName: "group",
		list:      func(st *store.Store) (any, error) { return st.ListGroups() },
		create: func(st *store.Store, name, note string, ids []int64) (int64, error) {
			return st.CreateGroup(name, note, ids)
		},
		del:       func(st *store.Store, id int64) error { return st.DeleteGroup(id) },
		delDetail: "成员归属一并解除",
	}
	registryLabel = registryResource{
		auditName: "label",
		list:      func(st *store.Store) (any, error) { return st.ListLabels() },
		del:       func(st *store.Store, id int64) error { return st.DeleteLabel(id) },
		delDetail: "从主机移除该键",
	}
)

// registryList 通用列表（路由层 requireAuth：全员可见）。
func (s *Server) registryList(w http.ResponseWriter, res registryResource) {
	list, err := res.list(s.st)
	if err != nil {
		s.writeInternal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// registryCreate 通用新建（池/组同构）。store 层已把重名/命名校验翻译为
// 可读错误（dupErr/validScopeName），均为用户输入问题，直接 400。
func (s *Server) registryCreate(w http.ResponseWriter, r *http.Request, res registryResource) {
	var req struct {
		Name    string  `json:"name"`
		Note    string  `json:"note"`
		HostIDs []int64 `json:"host_ids"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	id, err := res.create(s.st, req.Name, req.Note, req.HostIDs)
	if err != nil {
		s.writeStoreErr(w, err)
		return
	}
	s.audit(r, "create", res.auditName, req.Name, fmt.Sprintf("%d 台主机划入", len(req.HostIDs)))
	writeJSON(w, http.StatusCreated, map[string]any{"id": id, "name": req.Name})
}

// registryDelete 通用删除：成员/归属联动语义的差异由 delDetail 描述。
func (s *Server) registryDelete(w http.ResponseWriter, r *http.Request, res registryResource) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := res.del(s.st, id); err != nil {
		s.writeStoreErr(w, err)
		return
	}
	s.audit(r, "delete", res.auditName, fmt.Sprint(id), res.delDetail)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// 六个具名 handler：路由表（routes.go）的绑定点，薄委托到通用实现。

func (s *Server) handleListPools(w http.ResponseWriter, _ *http.Request) {
	s.registryList(w, registryPool)
}
func (s *Server) handleCreatePool(w http.ResponseWriter, r *http.Request) {
	s.registryCreate(w, r, registryPool)
}
func (s *Server) handleDeletePool(w http.ResponseWriter, r *http.Request) {
	s.registryDelete(w, r, registryPool)
}

func (s *Server) handleListGroups(w http.ResponseWriter, _ *http.Request) {
	s.registryList(w, registryGroup)
}
func (s *Server) handleCreateGroup(w http.ResponseWriter, r *http.Request) {
	s.registryCreate(w, r, registryGroup)
}
func (s *Server) handleDeleteGroup(w http.ResponseWriter, r *http.Request) {
	s.registryDelete(w, r, registryGroup)
}

func (s *Server) handleListLabels(w http.ResponseWriter, _ *http.Request) {
	s.registryList(w, registryLabel)
}
func (s *Server) handleDeleteLabel(w http.ResponseWriter, r *http.Request) {
	s.registryDelete(w, r, registryLabel)
}

// ---- 标签新建（键值语义，专用）----

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
				s.writeStoreErr(w, err)
				return
			}
			s.audit(r, "create", "label", k, v)
		}
		writeJSON(w, http.StatusCreated, map[string]any{"ok": true})
		return
	}
	id, err := s.st.CreateLabel(req.Key, req.Note, req.HostIDs, req.Value)
	if err != nil {
		s.writeStoreErr(w, err)
		return
	}
	s.audit(r, "create", "label", req.Key, req.Value)
	writeJSON(w, http.StatusCreated, map[string]any{"id": id, "key": req.Key})
}

// ---- 删除即退役 ----

// retireAgent 通知 agent 自清理退出。mTLS（ctl 客户端证书）优先；明文
// http 兜底仅限台账声明明文的主机（手工添加的回环 agent）。返回是否成功
// 退役与失败原因（失败不阻断台账删除，由调用方转 warning）。
func (s *Server) retireAgent(ctx context.Context, h *store.Host) (bool, string) {
	if s.cam != nil {
		err := postShutdown(ctx, s.cam.tlsClient, "https", h)
		if err == nil {
			return true, ""
		}
		// 纳管主机不降级明文（信任模型同 agentHostModel：签发过证书的主机
		// 一律走 mTLS，"TLS 失败回落明文"是可被中间人主动触发的降级）。
		// 失败只记 warning，落账删除照常继续
		if s.useTLS(h) {
			s.logger.Warn("retire agent over mTLS failed (no plaintext fallback for TLS host)",
				"host", h.Name, "address", h.Address, "err", err)
			return false, err.Error()
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
	if err != nil {
		s.writeStoreErr(w, err)
		return
	}
	retired, warn := s.retireAgent(r.Context(), h)
	if err := s.st.DeleteHost(id); err != nil {
		// 并发删除（确认框双击/批量与单删竞争）时 GetHost 已过、行已被
		// 另一请求删掉 → ErrNotFound。按 404「已删除」回给前端而非 500
		// internal server error（GetHost 与 DeleteHost 之间无事务，这条
		// 路径真实可达——日志曾见 internal error err="not found"）
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "host already deleted")
			return
		}
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

// batchConcurrency 批量探活/批量删除的并发度：逐台串行时单台最坏 10s
// 超时随台数线性累加，百台批量的总时长不可用。固定 worker 池
// （worker.BoundedForeach）：逐台起 goroutine 只约束工作并发、约束不了
// goroutine 创建，大批量会驻留海量阻塞在信号量上的协程。
const batchConcurrency = 8

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
	// 池/组（matchScope"任一命中"不够——那会把主机挪进任意池扩大可见面）。
	// 标签同口径：labels 驱动 label 型授权面（按"主机带该键"匹配），不校验
	// 会允许作用域用户给主机盖上任意 label 键、间接扩大 label 型授权的
	// 覆盖面（handleUpdateHost 已是同口径）
	if req.Action == "assign" {
		var tp, tg []string
		if req.SetPools {
			tp = req.Pools
		}
		if req.SetGroups {
			tg = req.Groups
		}
		tk := make([]string, 0, len(req.Labels))
		for k := range req.Labels {
			tk = append(tk, k)
		}
		slices.Sort(tk)
		if !s.scopeWriteAllowed(r, verbHostEdit, tp, tg, tk) {
			writeError(w, http.StatusForbidden, "forbidden: assign target pools/groups/labels outside your host:edit scope")
			return
		}
	}
	// 逐台校验/执行按 action 拆开（batchResolveHost 共用前置），行为口径
	// 与原单循环一致
	var results []BatchResult
	switch req.Action {
	case "probe":
		results = s.batchProbeHosts(p, needVerb, &req)
	case "delete":
		results = s.batchDeleteHosts(r, p, needVerb, &req)
	default: // assign（action 合法性已在上方 switch 收敛）
		results = s.batchAssignHosts(r, p, needVerb, &req)
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

// batchResolveHost 批量动作的逐台前置（校验）：取主机 + 作用域校验；
// 失败时返回已填 Detail 的结果（ok=false，调用方计入 results 不中断）。
func (s *Server) batchResolveHost(p *userPerms, needVerb string, id int64) (*store.Host, BatchResult, bool) {
	h, err := s.st.GetHost(id)
	if err != nil {
		return nil, BatchResult{ID: id, OK: false, Detail: "host not found"}, false
	}
	if !p.canHost(needVerb, h.Pools, h.Groups, h.Labels) {
		return nil, BatchResult{ID: id, Name: h.Name, OK: false, Detail: "forbidden: outside your scope"}, false
	}
	return h, BatchResult{ID: id, Name: h.Name}, true
}

// batchProbeHosts probe 动作（执行）：有限并发探活 + 在线状态落库。
// 探活挂 background ctx 脱离请求生命周期（对齐 exec/upgrade/sshinstall
// 的 background() 口径，动机见 httpx.background 注释）：r.Context() 会随
// 客户端断连取消，把整批探活拦腰打断——而探活结果本来就要落库，不随
// 断连作废。结果按请求顺序返回（槽位预分配，worker 只回填自己的槽）。
// 落库前判 ctx.Err()（与 worker.Prober 同口径）：server 关停瞬间在途
// 探测以失败收场，据此写库会把整批标成离线。
func (s *Server) batchProbeHosts(p *userPerms, needVerb string, req *BatchRequest) []BatchResult {
	results := make([]BatchResult, len(req.IDs))
	var (
		pending []*store.Host // 待探活主机
		slots   []int         // 对应 results 下标
	)
	for i, id := range req.IDs {
		h, fail, ok := s.batchResolveHost(p, needVerb, id)
		if !ok {
			results[i] = fail
			continue
		}
		results[i] = BatchResult{ID: id, Name: h.Name}
		pending = append(pending, h)
		slots = append(slots, i)
	}
	ctx := s.background()
	_ = worker.BoundedForeach(ctx, pending, batchConcurrency, func(ctx context.Context, k int, h *store.Host) error {
		pr := probeHost(ctx, h, s.probeClientFor(h))
		res := &results[slots[k]]
		res.OK = pr.Status == "online"
		res.Detail = pr.Status + " " + pr.Error
		if ctx.Err() != nil {
			return nil // 关停不落库（响应槽位照填，尽力给到结论）
		}
		_ = s.st.SetHostStatus(h.ID, pr.Status, pr.Build, probeModulesJSON(pr))
		return nil
	})
	return results
}

// batchDeleteHosts delete 动作（执行）：退役 agent（不可达仅告警）→ 删
// 台账 → 回收差分快照与闸门锁。retireAgent 保持挂请求 ctx 不动（与单删
// handleDeleteHost 同口径：退役是尽力而为的附带动作，不脱离请求执行）；
// 因此 BoundedForeach 的取消截断也按请求生命周期生效——客户端断连后
// 余下主机不再继续删（原实现"先起满 goroutine"的断连续删是派发副产品
// 而非承诺，半批删除用户不可见反而更难对账，重试批次对已删台会得到
// 「已被其他请求删除」的 OK）。
func (s *Server) batchDeleteHosts(r *http.Request, p *userPerms, needVerb string, req *BatchRequest) []BatchResult {
	// 有界并发（batchConcurrency）：retire 对不可达主机要等满连接超时，
	// 此前逐台串行让百台级批量删除卡到分钟级——实测 120 台不可达主机
	// 无法在请求超时内完成。结果按 ID 声明序回填（并发完成序不影响响应
	// 次序）。
	results := make([]BatchResult, len(req.IDs))
	var (
		pending []*store.Host // 待删除主机
		slots   []int         // 对应 results 下标
	)
	for k, id := range req.IDs {
		h, fail, ok := s.batchResolveHost(p, needVerb, id)
		if !ok {
			results[k] = fail
			continue
		}
		results[k] = BatchResult{ID: id, Name: h.Name}
		pending = append(pending, h)
		slots = append(slots, k)
	}
	_ = worker.BoundedForeach(r.Context(), pending, batchConcurrency, func(ctx context.Context, k int, h *store.Host) error {
		id := h.ID
		// 单台退役限时 10s：不可达不该拖垮整批（退役本就是尽力而为）
		rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		retired, warn := s.retireAgent(rctx, h)
		res := &results[slots[k]]
		if err := s.st.DeleteHost(id); err != nil {
			// 同单删：并发把行删掉时 ErrNotFound 不算失败，「已被删除」
			if errors.Is(err, store.ErrNotFound) {
				*res = BatchResult{ID: id, Name: h.Name, OK: true, Detail: "已被其他请求删除"}
				return nil
			}
			*res = BatchResult{ID: id, Name: h.Name, Detail: err.Error()}
			return nil
		}
		// 同单删：回收差分快照与闸门锁
		s.monitor.Forget(id)
		s.gate.Forget(id)
		res.OK = true
		res.Retired = retired
		if warn != "" {
			res.Detail = "agent 退役失败: " + warn
		}
		return nil
	})
	return results
}

// batchAssignHosts assign 动作（执行）：归属/标签逐台写入。
func (s *Server) batchAssignHosts(r *http.Request, p *userPerms, needVerb string, req *BatchRequest) []BatchResult {
	results := make([]BatchResult, 0, len(req.IDs))
	for _, id := range req.IDs {
		h, fail, ok := s.batchResolveHost(p, needVerb, id)
		if !ok {
			results = append(results, fail)
			continue
		}
		res := BatchResult{ID: id, Name: h.Name}
		// replace_labels 整体替换：被替换掉的旧键也是写入面——scope 用户
		// 不应能拆掉自己不覆盖的 label 绑定（他人/未来的 label 型授权会
		// 随该键消失而静默失效）。逐台判定：各主机现有键不同
		if req.ReplaceLabels {
			if removed := removedLabelKeys(h.Labels, req.Labels); len(removed) > 0 &&
				!s.scopeWriteAllowed(r, verbHostEdit, nil, nil, removed) {
				res.Detail = "forbidden: replaced labels outside your host:edit scope"
				results = append(results, res)
				continue
			}
		}
		n, err := s.st.BatchAssign([]int64{id}, req.Pools, req.Groups, req.SetPools, req.SetGroups, req.Labels, req.ReplaceLabels)
		res.OK = err == nil && n == 1
		if err != nil {
			res.Detail = err.Error()
		}
		results = append(results, res)
	}
	return results
}

// hostPort 供直连 URL 拼接（IPv6 安全）。
func hostPort(h *store.Host) string {
	return net.JoinHostPort(h.Address, fmt.Sprint(h.AgentPort))
}

// removedLabelKeys replace_labels 整体替换时将被移除的旧键（主机现有键 −
// 请求新键集合），排序保证输出稳定。
func removedLabelKeys(cur string, next map[string]string) []string {
	old := labelKeys(cur)
	out := make([]string, 0, len(old))
	for k := range old {
		if _, ok := next[k]; !ok {
			out = append(out, k)
		}
	}
	slices.Sort(out)
	return out
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
