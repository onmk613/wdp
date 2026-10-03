package web

// 主机台账的基础 CRUD/探活端点（批量导入、纳管、升级等重操作在
// inventory_ops/enroll/sshinstall/upgrade 各自文件）。

import (
	"fmt"
	"net/http"
	"strings"

	"wdp/internal/store"
)

// ---- 主机台账 ----

func (s *Server) handleListHosts(w http.ResponseWriter, r *http.Request) {
	// 无分页参数 = 老语义全量（exec 圈选、CSV 预检等服务端内部调用）
	if !r.URL.Query().Has("page") && !r.URL.Query().Has("page_size") {
		hosts, err := s.st.ListHosts(r.URL.Query().Get("q"))
		if err != nil {
			s.writeInternal(w, err)
			return
		}
		writeJSON(w, http.StatusOK, s.filterHosts(r, verbHostView, hosts))
		return
	}
	pp := parsePage(r)
	q := r.URL.Query().Get("q")
	perm := s.permsOf(permUser(r))
	if perm.global[verbHostView] {
		// 全局权限：SQL 层直接分页（快路径）
		items, total, err := s.st.ListHostsPage(q, pp.Page, pp.PageSize)
		if err != nil {
			s.writeInternal(w, err)
			return
		}
		writeJSON(w, http.StatusOK, pagedResp[*store.Host]{Items: items, Total: total, Page: pp.Page, PageSize: pp.PageSize})
		return
	}
	// 作用域裁剪在池/组/标签上，SQL 化会与权限模型耦合——过滤后内存
	// 分页（搜索词已先在 SQL 收窄；主机量级为千行，可接受）
	hosts, err := s.st.ListHosts(q)
	if err != nil {
		s.writeInternal(w, err)
		return
	}
	hosts = s.filterHosts(r, verbHostView, hosts)
	total := int64(len(hosts))
	lo, hi := pp.offset(), pp.offset()+pp.PageSize
	if lo > len(hosts) {
		lo = len(hosts)
	}
	if hi > len(hosts) {
		hi = len(hosts)
	}
	writeJSON(w, http.StatusOK, pagedResp[*store.Host]{Items: hosts[lo:hi], Total: total, Page: pp.Page, PageSize: pp.PageSize})
}

// createHostReq 是 POST /api/hosts 的请求体。不再裸解码 store.Host：
// 整体解码会让客户端注入 ID/Status/CreatedAt/AllowPlaintext 等服务端
// 字段（AllowPlaintext 是通道信任开关，注入即绕过 handleUpdateHost 的
// host:enroll 门控）。字段不带 json tag：响应序列化已统一 snake_case，
// 请求侧同口径；编码器大小写不敏感，既有的小写 name/address 形态同样
// 命中。ID/状态/时间戳/AllowPlaintext 一律服务端自定，新建主机恒为明文
// 关闭，开启只能走 PUT 的 host:enroll 门。
type createHostReq struct {
	Name      string
	Address   string
	AgentPort int
	Pools     []string
	Groups    []string
	Labels    string // JSON 对象文本（与 store.Host.Labels 同口径）
}

func (s *Server) handleCreateHost(w http.ResponseWriter, r *http.Request) {
	var req createHostReq
	if !decodeJSON(w, r, &req) {
		return
	}
	// 校验与批量导入（handleImportHosts）同口径：name 必填且字符集同
	// safeName（裸解码入库会让含 "/"、空格等字符的名字直接进台账，而逐行
	// 校验的导入拒绝同名——两条建档路径一套标准；非法名字也无法作为逐主
	// 机证书文件名安全落盘）；address 必填；端口 0 = 默认 7602，越界拒绝
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	if req.Name != safeName(req.Name) {
		writeError(w, http.StatusBadRequest, "name has invalid characters (allowed: letters, digits, . _ -)")
		return
	}
	if strings.TrimSpace(req.Address) == "" {
		writeError(w, http.StatusBadRequest, "address is required")
		return
	}
	if req.AgentPort < 0 || req.AgentPort > 65535 {
		writeError(w, http.StatusBadRequest, "agent_port out of range (1-65535)")
		return
	}
	h := store.Host{
		Name: req.Name, Address: req.Address, AgentPort: req.AgentPort,
		Pools: req.Pools, Groups: req.Groups, Labels: req.Labels,
	}
	id, err := s.st.CreateHost(&h)
	if err != nil {
		// 重名是用户可见的 400；其余业务校验（labels 格式、池/组名）走
		// writeStoreErr 同口径回 400，DB/IO 走 500 脱敏——裸 SQL 错误串
		// 会暴露内部表结构
		if store.IsUniqueErr(err) {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("主机 %q 已存在", h.Name))
			return
		}
		s.writeStoreErr(w, err)
		return
	}
	h.ID = id
	s.audit(r, "create", "host", h.Name, h.Address)
	writeJSON(w, http.StatusCreated, h)
}

func (s *Server) handleGetHost(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	h, ok := s.checkHost(w, r, verbHostView, id)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, h)
}

func (s *Server) handleUpdateHost(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	cur, ok := s.checkHost(w, r, verbHostEdit, id)
	if !ok {
		return
	}
	var h store.Host
	if !decodeJSON(w, r, &h) {
		return
	}
	// 新归属是写入语义：scope 级 host:edit 只能把主机改到授权覆盖的池/组/
	// 标签内（与批量 assign 同口径；matchScope"任一命中"不够——那会把
	// 主机挪进任意池扩大可见面，或借标签间接扩大 label 型授权面）
	if bad := s.scopeWriteCheck(r, verbHostEdit, h.Pools, h.Groups, labelKeySlice(h.Labels)); bad != "" {
		writeError(w, http.StatusForbidden, "forbidden: target "+bad+"s outside your host:edit scope")
		return
	}
	// allow_plaintext 是通道信任模型开关（置 true 即让该主机的执行/部署
	// 走明文 HTTP），收敛到 host:enroll：host:edit 只改台账不改通道
	if h.AllowPlaintext != cur.AllowPlaintext && !s.permsOf(permUser(r)).canVerb(verbHostEnroll) {
		writeError(w, http.StatusForbidden, "forbidden: changing allow_plaintext requires "+verbHostEnroll)
		return
	}
	if err := s.st.UpdateHost(id, &h); err != nil {
		s.writeStoreErr(w, err)
		return
	}
	updated, err := s.st.GetHost(id)
	if err != nil {
		s.writeInternal(w, err)
		return
	}
	s.audit(r, "update", "host", updated.Name, "地址/端口/池/组/标签")
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) handleProbe(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	h, ok := s.checkHost(w, r, verbHostView, id)
	if !ok {
		return
	}
	result := probeHost(r.Context(), h, s.probeClientFor(h))
	_ = s.st.SetHostStatus(h.ID, result.Status, result.Build, probeModulesJSON(result))
	writeJSON(w, http.StatusOK, result)
}
