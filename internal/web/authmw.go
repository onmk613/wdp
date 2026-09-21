package web

// 鉴权与登录：requireAuth 会话门卫（cookie 会话；GET 支持 HTTP Basic 供
// Prometheus 等程序化抓取）、登录失败限速、登录/登出/me 端点。

import (
	"context"
	"net/http"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const (
	loginMaxFails   = 5
	loginLockWindow = 5 * time.Minute
)

// loginAttempt 一个 IP+用户名 的失败记录。
type loginAttempt struct {
	fails    int
	lastFail time.Time
	lockedTo time.Time
}

// loginLocked 查询键是否处于锁定期。
func (s *Server) loginLocked(key string) bool {
	s.loginMu.Lock()
	defer s.loginMu.Unlock()
	at, ok := s.loginFails[key]
	return ok && time.Now().Before(at.lockedTo)
}

// loginRecordFail 记一次失败；连续失败达阈值进入锁定。
func (s *Server) loginRecordFail(key string) {
	s.loginMu.Lock()
	defer s.loginMu.Unlock()
	at, ok := s.loginFails[key]
	if !ok {
		at = &loginAttempt{}
		s.loginFails[key] = at
	}
	at.fails++
	at.lastFail = time.Now()
	if at.fails >= loginMaxFails {
		at.lockedTo = time.Now().Add(loginLockWindow)
		at.fails = 0
	}
}

// loginReset 成功登录清零该键的失败计数。
func (s *Server) loginReset(key string) {
	s.loginMu.Lock()
	delete(s.loginFails, key)
	s.loginMu.Unlock()
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	// 跨站表单可以 text/plain 携带 JSON 体（无需预检），登录必须同样
	// 强制 JSON Content-Type（decodeJSON 不查 CT）
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		writeError(w, http.StatusUnsupportedMediaType, "JSON content-type required")
		return
	}
	var req struct{ User, Password string }
	if !decodeJSON(w, r, &req) {
		return
	}
	// 失败锁定：登录端点在认证之前，无失败限速即凭据 stuffing 自由尝试
	lockKey := s.remoteIP(r) + "|" + req.User
	if s.loginLocked(lockKey) {
		writeError(w, http.StatusTooManyRequests, "too many failed attempts, retry later")
		return
	}
	u, err := s.st.UserByName(req.User)
	// 用户不存在与密码错误统一口径，不泄露账号存在性
	if err != nil || bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(req.Password)) != nil {
		s.loginRecordFail(lockKey)
		s.auditEntry(req.User, s.remoteIP(r), "login_failed", "session", req.User, "登录失败（凭据错误或用户不存在）")
		writeError(w, http.StatusUnauthorized, "invalid credentials")
		return
	}
	if u.Disabled {
		s.loginRecordFail(lockKey)
		s.auditEntry(req.User, s.remoteIP(r), "login_failed", "session", req.User, "登录失败（账号已禁用）")
		writeError(w, http.StatusForbidden, "account disabled")
		return
	}
	s.loginReset(lockKey)
	token, err := s.sessions.issue(req.User)
	if err != nil {
		s.writeInternal(w, err)
		return
	}
	s.auditEntry(req.User, s.remoteIP(r), "login", "session", req.User, "登录成功")
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: token, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteStrictMode, MaxAge: int(sessionTTL.Seconds()),
	})
	p := s.permsOf(req.User)
	writeJSON(w, http.StatusOK, map[string]any{"user": req.User, "role": p.role, "perms": p.summary()})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	user, _ := r.Context().Value(ctxUser{}).(string)
	if c, err := r.Cookie(sessionCookie); err == nil {
		s.sessions.revoke(c.Value)
	}
	s.audit(r, "logout", "session", user, "登出")
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	user, _ := r.Context().Value(ctxUser{}).(string)
	p := s.permsOf(user)
	writeJSON(w, http.StatusOK, map[string]any{"user": user, "role": p.role, "perms": p.summary()})
}

// ctxUser 会话用户注入 context 的键类型。
type ctxUser struct{}

// requireAuth 会话门卫：cookie 会话优先；GET 支持 HTTP Basic（Prometheus
// 等程序化抓取 /api/hosts/{id}/metrics 用，浏览器不适用）。变更类路由
// 同时要求 JSON 请求体（CSRF 面：SameSite=Strict + 跨站表单无法带 JSON
// Content-Type）。
func (s *Server) requireAuth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var user string
		c, err := r.Cookie(sessionCookie)
		if err == nil {
			if u, ok := s.sessions.lookup(c.Value); ok {
				user = u
			}
		}
		if user == "" && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
			if u, ok := s.basicAuthUser(r); ok {
				user = u
			}
		}
		if user == "" {
			// Basic 质询提示只发给非 JSON 调用方（Prometheus 抓取等）。
			// 浏览器 fetch 一旦收到 401+WWW-Authenticate 就会弹原生登录
			// 框——页面内 JSON API（/api/me 探测等）绝不能带（曾致登录页
			// 刷新/退出后必弹原生认证框）
			if r.Method == http.MethodGet || r.Method == http.MethodHead {
				if !strings.Contains(r.Header.Get("Accept"), "application/json") {
					w.Header().Set("WWW-Authenticate", `Basic realm="wdp"`)
				}
			}
			writeError(w, http.StatusUnauthorized, "authentication required")
			return
		}
		switch r.Method {
		case http.MethodPost, http.MethodPut, http.MethodDelete:
			// JSON 为主；multipart 用于应用 tgz 上传（跨站表单无法携带
			// SameSite=Strict 会话，CSRF 防线仍然成立）
			if ct := r.Header.Get("Content-Type"); ct != "" &&
				!strings.HasPrefix(ct, "application/json") && !strings.HasPrefix(ct, "multipart/form-data") {
				writeError(w, http.StatusUnsupportedMediaType, "JSON content-type required")
				return
			}
		}
		h(w, r.WithContext(context.WithValue(r.Context(), ctxUser{}, user)))
	}
}

// basicAuthUser 校验 HTTP Basic 凭据（bcrypt，与登录同源）。禁用账号与
// 登录端点同口径拒绝（否则禁用只挡得住会话路径，Basic 仍可进）。
func (s *Server) basicAuthUser(r *http.Request) (string, bool) {
	user, pass, ok := r.BasicAuth()
	if !ok || user == "" {
		return "", false
	}
	u, err := s.st.UserByName(user)
	if err != nil || u.Disabled || bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(pass)) != nil {
		return "", false
	}
	return user, true
}
