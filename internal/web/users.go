package web

// 用户与会话管理（user:manage）：账号 CRUD、角色、禁用、重置密码、
// 追加授权（scope）编辑、在线会话查看/踢下线。所有变更埋审计。

import (
	"net/http"
	"strings"

	"golang.org/x/crypto/bcrypt"

	"wdp/internal/store"
)

// userJSON 对外形态（不含散列）。
type userJSON struct {
	ID        int64              `json:"ID"`
	Name      string             `json:"Name"`
	Role      string             `json:"Role"`
	Disabled  bool               `json:"Disabled"`
	CreatedAt string             `json:"CreatedAt"`
	Scopes    []*store.UserScope `json:"Scopes"`
	Online    bool               `json:"Online"`
}

func (s *Server) handleListUsers(w http.ResponseWriter, _ *http.Request) {
	s.listUsersReply(w)
}

func (s *Server) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name     string `json:"name"`
		Password string `json:"password"`
		Role     string `json:"role"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "user name is required")
		return
	}
	if len(req.Password) < 8 {
		writeError(w, http.StatusBadRequest, "password must be at least 8 characters")
		return
	}
	if _, ok := builtinRoles[req.Role]; !ok {
		req.Role = "viewer" // 新用户默认 viewer：共享环境最小权限起步
	}
	if _, err := s.st.UserByName(req.Name); err == nil {
		writeError(w, http.StatusBadRequest, "user already exists")
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		s.writeInternal(w, err)
		return
	}
	if err := s.st.CreateUser(req.Name, string(hash), req.Role); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.audit(r, "create", "user", req.Name, "角色 "+req.Role)
	s.listUsersReply(w)
}

func (s *Server) handleUpdateUser(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	target, err := s.st.UserByID(id)
	if err != nil {
		writeError(w, http.StatusNotFound, "user not found")
		return
	}
	var req struct {
		Role     string `json:"role"`
		Disabled *bool  `json:"disabled"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Role != "" {
		if _, ok := builtinRoles[req.Role]; !ok {
			writeError(w, http.StatusBadRequest, "invalid role (admin | operator | viewer)")
			return
		}
	}
	// 最后一个 admin 不可降级/禁用（否则系统失去管理者）
	lastAdmin, _ := s.st.CountAdmins()
	demote := req.Role != "" && req.Role != "admin" && target.Role == "admin" && !target.Disabled
	disable := req.Disabled != nil && *req.Disabled && target.Role == "admin" && !target.Disabled
	if (demote || disable) && lastAdmin <= 1 {
		writeError(w, http.StatusBadRequest, "cannot demote/disable the last admin")
		return
	}
	if err := s.st.UpdateUser(id, req.Role, req.Disabled); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.invalidatePerms(target.Name)
	// 禁用立即生效：踢掉全部在线会话
	if req.Disabled != nil && *req.Disabled {
		s.sessions.revokeUser(target.Name)
	}
	detail := ""
	if req.Role != "" {
		detail = "角色 → " + req.Role
	}
	if req.Disabled != nil {
		if detail != "" {
			detail += "，"
		}
		if *req.Disabled {
			detail += "禁用（已踢下线）"
		} else {
			detail += "启用"
		}
	}
	s.audit(r, "update", "user", target.Name, detail)
	s.listUsersReply(w)
}

func (s *Server) handleResetPassword(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	target, err := s.st.UserByID(id)
	if err != nil {
		writeError(w, http.StatusNotFound, "user not found")
		return
	}
	var req struct {
		Password string `json:"password"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if len(req.Password) < 8 {
		writeError(w, http.StatusBadRequest, "password must be at least 8 characters")
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		s.writeInternal(w, err)
		return
	}
	if err := s.st.SetUserPassword(target.Name, string(hash)); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// 重置密码后旧会话全部失效（凭据已换，旧会话不再可信）
	s.sessions.revokeUser(target.Name)
	s.audit(r, "update", "user", target.Name, "重置密码（旧会话已失效）")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	target, err := s.st.UserByID(id)
	if err != nil {
		writeError(w, http.StatusNotFound, "user not found")
		return
	}
	me, _ := r.Context().Value(ctxUser{}).(string)
	if target.Name == me {
		writeError(w, http.StatusBadRequest, "cannot delete yourself")
		return
	}
	if target.Role == "admin" && !target.Disabled {
		if n, _ := s.st.CountAdmins(); n <= 1 {
			writeError(w, http.StatusBadRequest, "cannot delete the last admin")
			return
		}
	}
	if err := s.st.DeleteUser(id); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.sessions.revokeUser(target.Name)
	s.invalidatePerms(target.Name)
	s.audit(r, "delete", "user", target.Name, "")
	s.listUsersReply(w)
}

// handleSetUserScopes 整体替换某用户的追加授权。
func (s *Server) handleSetUserScopes(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	target, err := s.st.UserByID(id)
	if err != nil {
		writeError(w, http.StatusNotFound, "user not found")
		return
	}
	var req struct {
		Scopes []*store.UserScope `json:"scopes"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	for _, sc := range req.Scopes {
		if !scopeableVerbs[sc.Verb] {
			writeError(w, http.StatusBadRequest, "verb "+sc.Verb+" is not scopeable")
			return
		}
		switch sc.Kind {
		case "":
			sc.Value = ""
		case "pool", "group", "label":
			if sc.Value == "" {
				writeError(w, http.StatusBadRequest, "scope value is required for kind "+sc.Kind)
				return
			}
		default:
			writeError(w, http.StatusBadRequest, "invalid scope kind (pool | group | label)")
			return
		}
	}
	if err := s.st.ReplaceUserScopes(id, req.Scopes); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.invalidatePerms(target.Name)
	s.audit(r, "update", "user", target.Name, "追加授权变更")
	scopes, _ := s.st.UserScopes(id)
	writeJSON(w, http.StatusOK, scopes)
}

// ---- 会话管理 ----

type sessionJSON struct {
	Token   string `json:"token"` // 掩码（前 8 位）
	User    string `json:"user"`
	Expires string `json:"expires"`
}

func (s *Server) handleListSessions(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.sessions.list())
}

func (s *Server) handleKillSession(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	if token == "" {
		writeError(w, http.StatusBadRequest, "token is required")
		return
	}
	if !s.sessions.kill(token) {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}
	s.audit(r, "delete", "session", token[:8]+"…", "强制下线")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// listUsersReply 统一返回用户列表（创建/更新/删除后）。
func (s *Server) listUsersReply(w http.ResponseWriter) {
	users, err := s.st.ListUsers()
	if err != nil {
		s.writeInternal(w, err)
		return
	}
	scopesByUser, err := s.st.AllUserScopes() // 单查询取全量：逐用户查是 N+1
	if err != nil {
		s.writeInternal(w, err)
		return
	}
	online := s.sessions.onlineUsers()
	out := make([]*userJSON, 0, len(users))
	for _, u := range users {
		scopes := scopesByUser[u.ID]
		if scopes == nil {
			scopes = []*store.UserScope{} // 保持 JSON 呈现为 [] 而非 null
		}
		out = append(out, &userJSON{
			ID: u.ID, Name: u.Name, Role: u.Role, Disabled: u.Disabled,
			CreatedAt: u.CreatedAt, Scopes: scopes, Online: online[u.Name],
		})
	}
	writeJSON(w, http.StatusOK, out)
}
