package web

// 权限模型：角色（粗）+ 作用域追加授权（细）。
//   - 权限点 verb = "resource:action"（host:view / run:execute …），与
//     端点一一对应；
//   - 角色给出一组**全局**权限点（admin/operator/viewer 内置）；
//   - user_scopes 在角色之上**叠加**按作用域的授权：{verb, kind, value}，
//     kind 为 pool/group/label（主机类按主机的池/组/标签匹配，应用类按
//     应用 scope 匹配）。叠加模型只增不减：要把某人限制在某池，给他
//     viewer 角色 + 该池的 host:edit 追加授权。
//   - 执行交集：应用执行/远程命令的目标 = 请求选择器 ∩ run:execute
//     允许的主机集合（全局权限 = 全部），空集拒绝。

import (
	"encoding/json"
	"errors"
	"net/http"
	"sort"

	"wdp/internal/store"
)

// 权限点全集（前端 api.ts 镜像一份）。
const (
	verbHostView    = "host:view" // 主机列表/详情/facts/指标/趋势/探活
	verbHostEdit    = "host:edit" // 改主机（含批量设置）
	verbHostDelete  = "host:delete"
	verbHostEnroll  = "host:enroll" // 手动添加/导入/SSH 推装/纳管凭证
	verbHostUpgrade = "host:upgrade"
	verbRunExec     = "run:execute" // 应用执行 + 远程命令（按目标主机作用域）
	verbRunView     = "run:view"
	verbRunDelete   = "run:delete"
	verbAppView     = "app:view"
	verbAppCreate   = "app:create"
	verbAppEdit     = "app:edit" // 编辑器保存/设默认版本/应用元数据
	verbAppUpload   = "app:upload"
	verbAppScope    = "app:scope" // 版本作用域变更（权限调整语义，admin/operator）
	verbAppDelete   = "app:delete"
	verbRegistry    = "registry:manage"
	verbAuditView   = "audit:view"
	verbUserManage  = "user:manage"
)

var allVerbs = []string{
	verbHostView, verbHostEdit, verbHostDelete, verbHostEnroll, verbHostUpgrade,
	verbRunExec, verbRunView, verbRunDelete,
	verbAppView, verbAppCreate, verbAppEdit, verbAppUpload, verbAppScope, verbAppDelete,
	verbRegistry, verbAuditView, verbUserManage,
}

// scopeableVerbs 可按作用域追加的权限点（其余只能全局授予）。
var scopeableVerbs = map[string]bool{
	verbHostView: true, verbHostEdit: true, verbRunExec: true,
	verbAppView: true, verbAppEdit: true, verbAppUpload: true,
}

// 内置角色。admin 隐含一切；空角色（迁移前旧行）按 operator 收敛。
var builtinRoles = map[string][]string{
	"admin": allVerbs,
	"operator": {
		verbHostView, verbHostEdit, verbHostEnroll, verbHostUpgrade,
		verbRunExec, verbRunView, verbRunDelete,
		verbAppView, verbAppCreate, verbAppEdit, verbAppUpload, verbAppScope,
		verbRegistry,
	},
	"viewer": {verbHostView, verbAppView, verbRunView},
}

func roleVerbs(role string) map[string]bool {
	verbs, ok := builtinRoles[role]
	if !ok {
		verbs = builtinRoles["operator"]
	}
	m := make(map[string]bool, len(verbs))
	for _, v := range verbs {
		m[v] = true
	}
	return m
}

// userPerms 是一个用户的完整权限视图（角色展开 + 追加授权）。
type userPerms struct {
	role   string
	global map[string]bool        // 全局权限点
	scoped map[string][]*scopeSel // verb → 追加的作用域列表
}

type scopeSel struct{ kind, value string }

func (p *userPerms) isAdmin() bool { return p.role == "admin" }

// canVerb 有没有这个权限点（全局或作用域），菜单可见性用。
func (p *userPerms) canVerb(verb string) bool {
	return p.global[verb] || len(p.scoped[verb]) > 0
}

// canHost 主机类判定：全局权限直接过；否则按主机的池/组/标签匹配任一
// 作用域（label 匹配 = 主机带该键）。
func (p *userPerms) canHost(verb string, pools, groups []string, labels string) bool {
	if p.global[verb] {
		return true
	}
	return matchScope(p.scoped[verb], pools, groups, labelKeys(labels))
}

// canApp 应用类判定：按应用的池/组/标签 scope 匹配。
func (p *userPerms) canApp(verb string, pools, groups []string, labels string) bool {
	if p.global[verb] {
		return true
	}
	return matchScope(p.scoped[verb], pools, groups, labelKeys(labels))
}

// labelKeys 解析标签 JSON 的键集合（label 作用域 = 带该键即匹配，与
// 执行选择器同口径）。
func labelKeys(labels string) map[string]bool {
	out := map[string]bool{}
	if labels == "" {
		return out
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(labels), &m); err != nil {
		return out
	}
	for k := range m {
		out[k] = true
	}
	return out
}

func matchScope(sels []*scopeSel, pools, groups []string, labels map[string]bool) bool {
	for _, sc := range sels {
		switch sc.kind {
		case "":
			return true // 追加授权里显式的"全部"
		case "pool":
			if contains(pools, sc.value) {
				return true
			}
		case "group":
			if contains(groups, sc.value) {
				return true
			}
		case "label":
			if labels[sc.value] {
				return true
			}
		}
	}
	return false
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// ---- Server 侧缓存 ----

// permsOf 取用户权限视图（进程内缓存；用户/授权变更时失效）。
func (s *Server) permsOf(user string) *userPerms {
	s.permMu.RLock()
	p, ok := s.permCache[user]
	s.permMu.RUnlock()
	if ok {
		return p
	}
	p = &userPerms{global: map[string]bool{}, scoped: map[string][]*scopeSel{}}
	if u, err := s.st.UserByName(user); err == nil {
		p.role = u.Role
		if u.Disabled {
			return p // 禁用 = 无任何权限（会话也应已被踢）
		}
		p.global = roleVerbs(u.Role)
		if u.Role == "admin" {
			return p
		}
		if scopes, err := s.st.UserScopes(u.ID); err == nil {
			for _, sc := range scopes {
				if !scopeableVerbs[sc.Verb] {
					continue // 不可作用域化的忽略
				}
				p.scoped[sc.Verb] = append(p.scoped[sc.Verb], &scopeSel{kind: sc.Kind, value: sc.Value})
			}
			// 覆盖语义：某权限点一旦有作用域行，取代该点的全局授予——
			// 既可提权（viewer + host:edit@pool），也可收窄（把 viewer 的
			// host:view 限定到某池，列表/详情随之裁剪）
			for v := range p.scoped {
				delete(p.global, v)
			}
		}
	}
	s.permMu.Lock()
	s.permCache[user] = p
	s.permMu.Unlock()
	return p
}

// invalidatePerms 用户/授权变更后失效缓存。
func (s *Server) invalidatePerms(user string) {
	s.permMu.Lock()
	delete(s.permCache, user)
	s.permMu.Unlock()
}

// permsSummary /api/me 与登录响应带的权限摘要（前端菜单/按钮门控）。
type permsSummary struct {
	Verbs  []string                     `json:"verbs"`  // 拥有的权限点（全局或作用域）
	Global []string                     `json:"global"` // 全局权限点
	Scoped map[string][]*storeScopeJSON `json:"scoped"` // verb → 作用域列表
}

type storeScopeJSON struct {
	Kind  string `json:"kind"`
	Value string `json:"value"`
}

func (p *userPerms) summary() *permsSummary {
	out := &permsSummary{Scoped: map[string][]*storeScopeJSON{}}
	seen := map[string]bool{}
	for v := range p.global {
		out.Global = append(out.Global, v)
		if !seen[v] {
			out.Verbs = append(out.Verbs, v)
			seen[v] = true
		}
	}
	for v, sels := range p.scoped {
		if !seen[v] && len(sels) > 0 {
			out.Verbs = append(out.Verbs, v)
			seen[v] = true
		}
		for _, sc := range sels {
			out.Scoped[v] = append(out.Scoped[v], &storeScopeJSON{Kind: sc.kind, Value: sc.value})
		}
	}
	sort.Strings(out.Verbs)
	sort.Strings(out.Global)
	return out
}

// ---- 中间件与判定辅助 ----

// requirePerm 全局权限点门控（403）；ctxUser 之后的链路里取 perms。
func (s *Server) requirePerm(verb string, h http.HandlerFunc) http.HandlerFunc {
	return s.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		user, _ := r.Context().Value(ctxUser{}).(string)
		if !s.permsOf(user).canVerb(verb) {
			writeError(w, http.StatusForbidden, "forbidden: requires "+verb)
			return
		}
		h(w, r)
	})
}

// permHost403 单资源作用域校验失败统一口径（404 不泄露存在性——但列表
// 已按 scope 裁剪，越权多半是 URL 直达，403 更便于前端提示）。
func permHost403(w http.ResponseWriter) {
	writeError(w, http.StatusForbidden, "forbidden: host outside your scope")
}

func permApp403(w http.ResponseWriter) {
	writeError(w, http.StatusForbidden, "forbidden: app outside your scope")
}

// ---- 判定辅助（handler 内使用） ----

// permUser 从 context 取当前用户。
func permUser(r *http.Request) string {
	user, _ := r.Context().Value(ctxUser{}).(string)
	return user
}

// filterHosts 列表按作用域裁剪（全局权限原样返回）。
func (s *Server) filterHosts(r *http.Request, verb string, hosts []*store.Host) []*store.Host {
	p := s.permsOf(permUser(r))
	if p.global[verb] {
		return hosts
	}
	out := make([]*store.Host, 0, len(hosts))
	for _, h := range hosts {
		if p.canHost(verb, h.Pools, h.Groups, h.Labels) {
			out = append(out, h)
		}
	}
	return out
}

// checkHost 取主机并校验作用域（verb 不可用或不在范围内 → 已写响应，
// 返回 false）。
func (s *Server) checkHost(w http.ResponseWriter, r *http.Request, verb string, id int64) (*store.Host, bool) {
	h, err := s.st.GetHost(id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "host not found")
		return nil, false
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return nil, false
	}
	if !s.permsOf(permUser(r)).canHost(verb, h.Pools, h.Groups, h.Labels) {
		permHost403(w)
		return nil, false
	}
	return h, true
}

// checkApp 取应用并校验作用域。
func (s *Server) checkApp(w http.ResponseWriter, r *http.Request, verb string, id int64) (*store.App, bool) {
	a, err := s.st.GetApp(id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "app not found")
		return nil, false
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return nil, false
	}
	if !s.permsOf(permUser(r)).canApp(verb, a.Pools, a.Groups, a.Labels) {
		permApp403(w)
		return nil, false
	}
	return a, true
}

// hostScopeSet verb 允许的主机 ID 集合（nil = 不限）。执行交集用：
// 请求目标 ∩ 该集合为空即拒绝。
func (s *Server) hostScopeSet(r *http.Request, verb string) map[int64]bool {
	p := s.permsOf(permUser(r))
	if p.global[verb] {
		return nil
	}
	set := map[int64]bool{}
	for _, sc := range p.scoped[verb] {
		if sc.kind == "" {
			return nil // 追加授权里显式"全部"
		}
		hosts, err := s.st.HostsBySelector(sc.kind, sc.value, nil)
		if err != nil {
			// 静默跳过会把该作用域的授权悄悄清零，留日志供排查
			s.logger.Warn("hostScopeSet: resolve selector", "kind", sc.kind, "value", sc.value, "err", err)
			continue
		}
		for _, h := range hosts {
			set[h.ID] = true
		}
	}
	return set
}

// scopeWriteAllowed 变更 scope 写入校验：调用者要把应用/版本的池/组/标签
// scope（或主机归属）改成给定值时，要求其授权覆盖**全部**目标值——全局
// verb 或持有 verbAppScope（不可作用域化，管理员级）可任意设置；否则逐项
// 必须落在其 verb 的作用域集合内。防 app:edit@poolA 借编辑器把 scope 扩
// 到任意池（matchScope 是"任一命中即过"，不能用于写入校验）。
func (s *Server) scopeWriteAllowed(r *http.Request, verb string, pools, groups, labelKs []string) bool {
	p := s.permsOf(permUser(r))
	if p.global[verb] || p.global[verbAppScope] {
		return true
	}
	for _, sc := range p.scoped[verb] {
		if sc.kind == "" {
			return true // 追加授权里显式的"全部"
		}
	}
	var coverPool, coverGroup, coverLabel map[string]bool
	for _, sc := range p.scoped[verb] {
		switch sc.kind {
		case "pool":
			if coverPool == nil {
				coverPool = map[string]bool{}
			}
			coverPool[sc.value] = true
		case "group":
			if coverGroup == nil {
				coverGroup = map[string]bool{}
			}
			coverGroup[sc.value] = true
		case "label":
			if coverLabel == nil {
				coverLabel = map[string]bool{}
			}
			coverLabel[sc.value] = true
		}
	}
	for _, v := range pools {
		if !coverPool[v] {
			return false
		}
	}
	for _, v := range groups {
		if !coverGroup[v] {
			return false
		}
	}
	for _, v := range labelKs {
		if !coverLabel[v] {
			return false
		}
	}
	return true
}

// intersectHosts 执行交集：目标主机 ∩ verb 允许集合；返回裁剪后的主机
// 与是否发生裁剪（发生裁剪且结果为空由调用方拒绝）。
func intersectHosts(hosts []*store.Host, allowed map[int64]bool) []*store.Host {
	if allowed == nil {
		return hosts
	}
	out := make([]*store.Host, 0, len(hosts))
	for _, h := range hosts {
		if allowed[h.ID] {
			out = append(out, h)
		}
	}
	return out
}
