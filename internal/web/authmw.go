package web

// 鉴权与登录：requireAuth 会话门卫（cookie 会话；GET 支持 HTTP Basic 供
// Prometheus 等程序化抓取）、登录失败限速、登录/登出/me 端点。

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const (
	loginMaxFails   = 5
	loginLockWindow = 5 * time.Minute
	// loginMaxIPFails 是同一来源 IP 的跨用户名失败上限（按用户名限速之外
	// 的第二道闸）：否则换用户名喷洒可无限尝试。
	loginMaxIPFails = 20
	// loginMaxKeys 是限速表条目上限（防随机用户名撑爆内存）。
	loginMaxKeys = 4096
	// loginMaxUserFails 是纯用户名维度（跨来源 IP）的失败上限：信任反代
	// 部署下来源 IP 取自 XFF，客户端伪造 XFF 即可轮换 IP 绕开前两个维度，
	// 用户名维度是不依赖 IP 的兜底闸。
	loginMaxUserFails = 5
)

// loginAttempt 一个限速键（IP+用户名，或纯 IP）的失败记录。
type loginAttempt struct {
	fails    int
	lastFail time.Time
	lockedTo time.Time
}

// bcryptCost 是控制台口令散列成本。控制台可执行任意远程命令，凭据
// 离线爆破的收益极高，DefaultCost(10) 偏松；12 的单次开销约数百毫秒，
// 对交互登录无感，对爆破是数量级抬升。存量散列仍按各自 cost 校验，
// 修改密码/建用户时自然升级。
const bcryptCost = 12

// dummyPasswordHash 是用户不存在时用来"陪跑"一次 bcrypt 的固定散列。
// 不跑的话，未知账号会因 `||` 短路秒回，与已知账号的数十毫秒形成可测量
// 差异 —— 足以枚举出系统里有哪些账号。
var dummyPasswordHash = mustBcryptHash("wdp-nonexistent-user-placeholder")

func mustBcryptHash(pw string) []byte {
	h, err := bcrypt.GenerateFromPassword([]byte(pw), bcryptCost)
	if err != nil {
		// 正常不会失败；真失败也不能让登录路径 panic
		return []byte("$2a$10$invalidinvalidinvalidinvalidinvalidinvalidinvalidinvalidinva")
	}
	return h
}

// loginLocked 查询键是否处于锁定期。
func (s *Server) loginLocked(key string) bool {
	s.loginMu.Lock()
	defer s.loginMu.Unlock()
	at, ok := s.loginFails[key]
	return ok && time.Now().Before(at.lockedTo)
}

// loginRecordFail 记一次失败；连续失败达 maxFails 即进入锁定期。
//
// 两个维度用不同阈值：IP+用户名（loginMaxFails，精确到账号）与纯 IP
// （loginMaxIPFails，跨用户名的喷洒）。IP 维度阈值更高，避免同一 NAT
// 出口下几个用户各错几次就把整个出口锁死。
//
// 表容量有上限：键含用户名，攻击者用随机用户名喷洒即可让表无界增长
// （内存 DoS）。达到上限后先清理已过窗的条目，仍满则拒绝新建键
// （既有键继续计数，攻击者无法靠"刷满表"让锁定失效）。
func (s *Server) loginRecordFail(key string, maxFails int) {
	now := time.Now()
	s.loginMu.Lock()
	defer s.loginMu.Unlock()
	at, ok := s.loginFails[key]
	if !ok {
		if len(s.loginFails) >= loginMaxKeys {
			s.pruneLoginFailsLocked(now)
			if len(s.loginFails) >= loginMaxKeys {
				return
			}
		}
		at = &loginAttempt{}
		s.loginFails[key] = at
	}
	at.fails++
	at.lastFail = now
	if at.fails >= maxFails {
		at.lockedTo = now.Add(loginLockWindow)
		at.fails = 0
	}
}

// pruneLoginFailsLocked 清理已过窗的限速条目（调用方持锁）。
func (s *Server) pruneLoginFailsLocked(now time.Time) {
	for k, at := range s.loginFails {
		if now.After(at.lockedTo) && now.Sub(at.lastFail) > loginLockWindow {
			delete(s.loginFails, k)
		}
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
	// 失败锁定：登录端点在认证之前，无失败限速即凭据 stuffing 自由尝试。
	// 三个维度：IP+用户名（精确）、纯 IP（跨用户名喷洒）、纯用户名（跨
	// IP——伪造 XFF 轮换来源时的兜底）。
	ip := s.remoteIP(r)
	lockKey := ip + "|" + req.User
	ipKey := "ip|" + ip
	userKey := "user|" + req.User
	if s.loginLocked(lockKey) || s.loginLocked(ipKey) || s.loginLocked(userKey) {
		writeError(w, http.StatusTooManyRequests, "too many failed attempts, retry later")
		return
	}
	u, err := s.st.UserByName(req.User)
	// 用户不存在与密码错误统一口径，不泄露账号存在性。未知账号也跑一次
	// bcrypt（对固定占位散列）：否则 `||` 短路会让未知账号秒回，与已知
	// 账号的数十毫秒形成可测量差异，足以枚举账号。
	hash := dummyPasswordHash
	if err == nil && u.PasswordHash != "" {
		hash = []byte(u.PasswordHash)
	}
	pwOK := bcrypt.CompareHashAndPassword(hash, []byte(req.Password)) == nil
	if err != nil || !pwOK {
		s.loginRecordFail(lockKey, loginMaxFails)
		s.loginRecordFail(ipKey, loginMaxIPFails)
		s.loginRecordFail(userKey, loginMaxUserFails)
		s.auditEntry(req.User, ip, "login_failed", "session", req.User, "登录失败（凭据错误或用户不存在）")
		writeError(w, http.StatusUnauthorized, "invalid credentials")
		return
	}
	if u.Disabled {
		s.loginRecordFail(lockKey, loginMaxFails)
		s.loginRecordFail(ipKey, loginMaxIPFails)
		s.loginRecordFail(userKey, loginMaxUserFails)
		s.auditEntry(req.User, ip, "login_failed", "session", req.User, "登录失败（账号已禁用）")
		writeError(w, http.StatusForbidden, "account disabled")
		return
	}
	s.loginReset(lockKey)
	s.loginReset(userKey)
	token, err := s.sessions.issue(req.User)
	if err != nil {
		s.writeInternal(w, err)
		return
	}
	s.auditEntry(req.User, s.remoteIP(r), "login", "session", req.User, "登录成功")
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: token, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteStrictMode, MaxAge: int(sessionTTL.Seconds()),
		Secure: s.requestIsHTTPS(r), // TLS 部署（原生或信任反代）下防 token 明文外泄
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
			// JSON 为主；multipart 用于应用 tgz 上传。空 Content-Type 一并
			// 拒绝：无体 POST（fetch 缺省不带 CT）可被无 CT 跨站请求触达。
			//
			// Content-Type 只是第一道：multipart/form-data 是跨站表单**可以**
			// 发送的类型，只靠它挡不住 CSRF。真正的防线是 SameSite=Strict
			// 会话 cookie + 这里的 Origin 同源校验（纵深防御，不依赖浏览器
			// 对 SameSite 的实现与代理是否改写）。
			if ct := r.Header.Get("Content-Type"); ct == "" ||
				(!strings.HasPrefix(ct, "application/json") && !strings.HasPrefix(ct, "multipart/form-data")) {
				writeError(w, http.StatusUnsupportedMediaType, "JSON content-type required")
				return
			}
			if !sameOriginRequest(r) {
				writeError(w, http.StatusForbidden, "cross-origin request rejected")
				return
			}
		}
		h(w, r.WithContext(context.WithValue(r.Context(), ctxUser{}, user)))
	}
}

// sameOriginRequest 校验状态变更请求的 Origin/Referer 与 Host 同源。
// Origin 缺失时放行（同源 fetch 与部分代理不发 Origin，Referer 亦可能被
// 隐私设置剥掉）——这是纵深防御的补充，主防线仍是 SameSite=Strict。
func sameOriginRequest(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	if origin == "null" {
		return false // 沙箱/数据 URL 发起的跨站请求
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}

// securityHeaders 是控制台的响应头基线。控制台可执行远程命令、删除主机、
// 发布应用：不允许被任意站点 iframe 嵌套（点击劫持），也不允许被当作
// 其它类型嗅探执行。
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Frame-Options", "DENY")
		h.Set("Content-Security-Policy", "frame-ancestors 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		// HSTS：浏览器此后强制 https 访问（防协议降级窃取会话 cookie）。
		// 仅对 https 到达的请求设置——明文部署（本机开发）下设置无意义，
		// 还会在同端口复用场景把 http 流量错误钉死
		if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}
		next.ServeHTTP(w, r)
	})
}

// basicAuthUser 校验 HTTP Basic 凭据（bcrypt，与登录同源）。禁用账号与
// 登录端点同口径拒绝（否则禁用只挡得住会话路径，Basic 仍可进）。
// 失败同样计入登录限速：Basic 路径无节流时，公网暴露下的任意 GET 都是
// 无锁定、无审计的凭据爆破面（登录端点有 5 次/5 分钟锁定而这里没有，
// 等于防线绕行）。键加 "basic|" 前缀与登录端点区分计数。
func (s *Server) basicAuthUser(r *http.Request) (string, bool) {
	user, pass, ok := r.BasicAuth()
	if !ok || user == "" {
		return "", false
	}
	ip := s.remoteIP(r)
	lockKey := "basic|" + ip + "|" + user
	ipKey := "basic|ip|" + ip
	userKey := "basic|user|" + user
	if s.loginLocked(lockKey) || s.loginLocked(ipKey) || s.loginLocked(userKey) {
		return "", false
	}
	u, err := s.st.UserByName(user)
	// 未知账号同样跑一次 bcrypt（占位散列）：时序一致，不泄露账号存在性
	hash := dummyPasswordHash
	if err == nil && u.PasswordHash != "" {
		hash = []byte(u.PasswordHash)
	}
	pwOK := bcrypt.CompareHashAndPassword(hash, []byte(pass)) == nil
	if err != nil || u.Disabled || !pwOK {
		s.loginRecordFail(lockKey, loginMaxFails)
		s.loginRecordFail(ipKey, loginMaxIPFails)
		s.loginRecordFail(userKey, loginMaxUserFails)
		s.auditEntry(user, ip, "login_failed", "session", user, "Basic 认证失败（凭据错误/用户不存在/账号禁用）")
		return "", false
	}
	s.loginReset(lockKey)
	s.loginReset(userKey)
	return user, true
}
