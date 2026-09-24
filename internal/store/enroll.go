package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// ---- 纳管凭证 ----

// EnrollToken 是一条纳管凭证（一次性，绑定预定主机名）。
type EnrollToken struct {
	ID           int64
	Token        string
	HostName     string
	AgentPort    int
	CreatedAt    string
	ExpiresAt    string
	UsedAt       string
	ClaimHost    string
	ClaimAddress string
	// KeyDeliveredAt 非空表示逐主机私钥已交付过（私钥不再可经 token 取走）
	KeyDeliveredAt string
}

// ErrTokenUsed 凭证已被成功使用（再用于下载/登记返回 410）。
var ErrTokenUsed = errors.New("enroll token already used")

// ErrTokenExpired 凭证已过 TTL。
var ErrTokenExpired = errors.New("enroll token expired")

// CreateEnrollToken 写入一条纳管凭证（token 由调用方生成）并顺带清理
// 已过期未用的旧凭证。
func (s *Store) CreateEnrollToken(token, hostName string, agentPort int, ttl time.Duration) error {
	if agentPort <= 0 {
		agentPort = 7602
	}
	now := time.Now().UTC()
	// 先清理过期未用凭证，再插入（顺序保证：新建的 token 不会被自身清理）。
	// 清理失败可忽略：只是顺带 GC，漏删的过期凭证仍会被各校验拒绝
	_, _ = s.db.Exec(`DELETE FROM enroll_tokens WHERE used_at = '' AND expires_at < ?`, now.Format(time.RFC3339))
	_, err := s.db.Exec(`INSERT INTO enroll_tokens (token, host_name, agent_port, created_at, expires_at) VALUES (?, ?, ?, ?, ?)`,
		token, hostName, agentPort, now.Format(time.RFC3339), now.Add(ttl).Format(time.RFC3339))
	return err
}

// GetEnrollToken 查询凭证（含使用状态）。
func (s *Store) GetEnrollToken(token string) (*EnrollToken, error) {
	t := &EnrollToken{}
	err := s.db.QueryRow(`SELECT id, token, host_name, agent_port, created_at, expires_at, used_at, claim_host, claim_address, key_delivered_at FROM enroll_tokens WHERE token = ?`, token).
		Scan(&t.ID, &t.Token, &t.HostName, &t.AgentPort, &t.CreatedAt, &t.ExpiresAt, &t.UsedAt, &t.ClaimHost, &t.ClaimAddress, &t.KeyDeliveredAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return t, nil
}

// checkEnrollToken 校验可用性（未使用且未过期），返回带状态错误的语义。
func checkEnrollToken(t *EnrollToken) error {
	if t.UsedAt != "" {
		return ErrTokenUsed
	}
	exp, err := time.Parse(time.RFC3339, t.ExpiresAt)
	if err != nil || time.Now().UTC().After(exp) {
		return ErrTokenExpired
	}
	return nil
}

// ClaimEnrollToken 脚本 claim 阶段：登记执行方 hostname 与来源地址
// （证书 SAN 用）。幂等：同 token 重复 claim 仅在信息一致时放行。
func (s *Store) ClaimEnrollToken(token, host, address string) (*EnrollToken, error) {
	t, err := s.GetEnrollToken(token)
	if err != nil {
		return nil, err
	}
	if err := checkEnrollToken(t); err != nil {
		return nil, err
	}
	if t.ClaimHost == "" {
		// 条件更新防并发窗口：两个不同来源同时 claim 同一空闲 token 时，
		// 无条件 UPDATE 会后写覆盖先写（"换 hostname 再来按重放拒绝"的
		// 检测随之失效）。只允许抢到"仍无 claim"的那次落库，输家按重放
		// 拒绝——与 ConsumeEnrollToken 的双花防护同口径。
		res, err := s.db.Exec(`UPDATE enroll_tokens SET claim_host = ?, claim_address = ? WHERE id = ? AND claim_host = ''`, host, address, t.ID)
		if err != nil {
			return nil, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil, fmt.Errorf("enroll token already claimed by another host")
		}
		t.ClaimHost, t.ClaimAddress = host, address
	} else if t.ClaimHost != host || t.ClaimAddress != address {
		// 幂等仅限信息一致：同 token 换 hostname/来源再来，按凭证被
		// 复制重放拒绝，否则无从察觉 token 泄露
		return nil, fmt.Errorf("enroll token already claimed by %q from %s", t.ClaimHost, t.ClaimAddress)
	}
	return t, nil
}

// ConsumeEnrollToken done 阶段：标记已使用并返回凭证（含 claim 信息）。
func (s *Store) ConsumeEnrollToken(token string) (*EnrollToken, error) {
	t, err := s.GetEnrollToken(token)
	if err != nil {
		return nil, err
	}
	if err := checkEnrollToken(t); err != nil {
		return nil, err
	}
	res, err := s.db.Exec(`UPDATE enroll_tokens SET used_at = ? WHERE id = ? AND used_at = ''`, nowUTC(), t.ID)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// 前置检查与 UPDATE 之间被并发请求抢先标记：按已使用拒绝，杜绝双花
		return nil, ErrTokenUsed
	}
	return t, nil
}

// MarkEnrollKeyDelivered 标记逐主机私钥已交付（幂等；交付后不可再取）。
func (s *Store) MarkEnrollKeyDelivered(id int64) error {
	_, err := s.db.Exec(`UPDATE enroll_tokens SET key_delivered_at = ? WHERE id = ? AND key_delivered_at = ''`, nowUTC(), id)
	return err
}

// ListEnrollTokens 列出未过期凭证（纳管面板展示）。
func (s *Store) ListEnrollTokens() ([]*EnrollToken, error) {
	rows, err := s.db.Query(`SELECT id, token, host_name, agent_port, created_at, expires_at, used_at, claim_host, claim_address, key_delivered_at
		FROM enroll_tokens WHERE expires_at >= ? ORDER BY id DESC`, nowUTC())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*EnrollToken{}
	for rows.Next() {
		t := &EnrollToken{}
		if err := rows.Scan(&t.ID, &t.Token, &t.HostName, &t.AgentPort, &t.CreatedAt, &t.ExpiresAt, &t.UsedAt, &t.ClaimHost, &t.ClaimAddress, &t.KeyDeliveredAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
