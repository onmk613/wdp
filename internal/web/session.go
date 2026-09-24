package web

// 内存会话表：token → {用户, 过期}（滑动窗口，空闲 2h 失效）+ 登录会话
// 的生命周期操作（签发/查询/吊销/清扫）。

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strings"
	"time"
)

// ---- 会话 ----

func (t *sessionTable) issue(user string) (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	token := hex.EncodeToString(buf)
	now := time.Now()
	t.mu.Lock()
	t.sessns[token] = session{user: user, exp: now.Add(sessionTTL), issued: now}
	t.mu.Unlock()
	return token, nil
}

// expired 会话是否已失效：空闲过期（滑动窗口）或超出绝对生命周期。
func expiredSession(s session, now time.Time) bool {
	return now.After(s.exp) || now.After(s.issued.Add(sessionMaxTTL))
}

func (t *sessionTable) lookup(token string) (string, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	s, ok := t.sessns[token]
	if !ok {
		return "", false
	}
	now := time.Now()
	if expiredSession(s, now) {
		delete(t.sessns, token)
		return "", false
	}
	s.exp = now.Add(sessionTTL) // 滑动续期（绝对上限仍以 issued 为锚）
	t.sessns[token] = s
	return s.user, true
}

func (t *sessionTable) revoke(token string) {
	t.mu.Lock()
	delete(t.sessns, token)
	t.mu.Unlock()
}

// revokeUser 踢掉某用户的全部会话（禁用/重置密码后旧会话立即失效）。
func (t *sessionTable) revokeUser(user string) {
	t.mu.Lock()
	for tok, s := range t.sessns {
		if s.user == user {
			delete(t.sessns, tok)
		}
	}
	t.mu.Unlock()
}

// onlineUsers 当前在线用户集合（用户列表页标记用）。
func (t *sessionTable) onlineUsers() map[string]bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := map[string]bool{}
	now := time.Now()
	for _, s := range t.sessns {
		if expiredSession(s, now) {
			continue
		}
		out[s.user] = true
	}
	return out
}

// list 在线会话清单（token 掩码，只露前 8 位）。
func (t *sessionTable) list() []*sessionJSON {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := []*sessionJSON{}
	now := time.Now()
	for tok, s := range t.sessns {
		if expiredSession(s, now) {
			continue
		}
		masked := tok
		if len(masked) > 8 {
			masked = masked[:8] + "…"
		}
		out = append(out, &sessionJSON{Token: masked, User: s.user, Expires: s.exp.Format(time.RFC3339)})
	}
	return out
}

// kill 强制单个会话下线（完整 token，或掩码前缀命中）。
// 前缀多命中（8 hex 碰撞或管理员输入过短）时拒绝而非踢第一个：
// "首个命中"的 map 遍历序不确定，可能踢错人。
func (t *sessionTable) kill(token string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.sessns[token]; ok {
		delete(t.sessns, token)
		return true
	}
	if len(token) >= 8 {
		prefix := token[:8]
		var match string
		n := 0
		for tok := range t.sessns {
			if strings.HasPrefix(tok, prefix) {
				match, n = tok, n+1
			}
		}
		if n == 1 {
			delete(t.sessns, match)
			return true
		}
	}
	return false
}

// sweep 删除全部已过期条目：过期删除原本只在 lookup 命中时发生，浏览器
// 丢 cookie 后条目无人再查，会只增不减（慢性泄漏），需周期清扫兜底。
func (t *sessionTable) sweep() {
	t.mu.Lock()
	now := time.Now()
	for tok, s := range t.sessns {
		if expiredSession(s, now) {
			delete(t.sessns, tok)
		}
	}
	t.mu.Unlock()
}

// sessionSweepLoop 周期清扫过期会话与过期的登录失败限速条目（键由请求
// 方构造，必须有过期回收防表被刷大）。
func (s *Server) sessionSweepLoop(ctx context.Context) {
	t := time.NewTicker(10 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.sessions.sweep()
			s.loginMu.Lock()
			s.pruneLoginFailsLocked(time.Now())
			s.loginMu.Unlock()
		}
	}
}

const sessionCookie = "wdp_session"
