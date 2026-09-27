package web

// 证书远程换证：web 持有逐主机证书对（<caDir>/hosts/<name>.crt|.key），
// Renew 保留私钥重签（SAN 不变、NotAfter 延后），再把新证书经 mTLS 推给
// agent 的 POST /cert 热更换——agent 免重装/免重启换证。agent 离线时新
// 证书已在 server 侧就位（agent 重启后即加载），在线推送只差一步补推。

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"time"

	"wdp/internal/ca"
	"wdp/internal/store"
)

// handleRenewHostCert 重签指定主机的 agent 服务端证书并推送热更换。
// 权限口径与推装 agent 一致（host:enroll——同属纳管生命周期操作）。
func (s *Server) handleRenewHostCert(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	h, ok := s.checkHost(w, r, verbHostEnroll, id)
	if !ok {
		return
	}
	if s.cam == nil {
		writeError(w, http.StatusBadRequest, "enrollment CA is not enabled on this server")
		return
	}
	crt, key := s.hostCertPaths(h.Name)
	if _, err := os.Stat(crt); err != nil {
		// 只有"确实没有证书"才是 404：Stat 的其他失败（权限/IO）是
		// server 侧问题，回 500 而不是伪装成"未纳管"
		if !errors.Is(err, os.ErrNotExist) {
			s.writeInternal(w, fmt.Errorf("stat host cert: %w", err))
			return
		}
		writeError(w, http.StatusNotFound, "host certificate not found (was this host enrolled via the web console?)")
		return
	}
	newCrt, _, fp, err := ca.Renew(ca.RenewOptions{
		CertPath: crt, KeyPath: key, OutPath: crt,
		CACertPath: s.cam.caPath, CAKeyPath: s.cam.caKeyPath,
		Days: DefaultAgentCertDays,
	})
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeError(w, http.StatusNotFound, "host certificate or key not found (was this host enrolled via the web console?)")
			return
		}
		s.writeInternal(w, fmt.Errorf("renew host cert: %w", err))
		return
	}
	pemBytes, err := os.ReadFile(newCrt)
	if err != nil {
		s.writeInternal(w, err)
		return
	}
	notAfter := certNotAfter(pemBytes)

	pushed, pushErr := s.pushCertToAgent(r.Context(), h, pemBytes)
	detail := "证书已重签"
	if pushed {
		detail += "并推送 agent 热更换生效"
	} else if pushErr != nil {
		detail += "；agent 推送失败（" + pushErr.Error() + "），新证书已在 server 侧就位，agent 重启后生效"
	} else {
		detail += "；agent 未启用 mTLS 或不可达，新证书已在 server 侧就位，agent 重启后生效"
	}
	s.audit(r, "renew", "host", h.Name, detail)

	resp := map[string]any{
		"renewed":     true,
		"pushed":      pushed,
		"not_after":   notAfter.Format(time.RFC3339),
		"fingerprint": fp,
		"message":     detail,
	}
	if pushErr != nil {
		// 重签成功、推送失败不是 5xx：证书本体已续期，客户端按 message 提示
		resp["push_error"] = pushErr.Error()
	}
	writeJSON(w, http.StatusOK, resp)
}

// pushCertToAgent 把新证书 PEM 经 mTLS 推给 agent 的 POST /cert（热更换）。
// 返回 (是否成功, 失败原因)；agent 未启用 mTLS（探活退回 http）时不尝试
// 推送，返回 (false, nil)——调用方以"重签成功、重启后生效"口径提示。
func (s *Server) pushCertToAgent(ctx context.Context, h *store.Host, certPEM []byte) (bool, error) {
	if !s.useTLS(h) {
		return false, nil
	}
	url := fmt.Sprintf("https://%s/cert", net.JoinHostPort(h.Address, fmt.Sprint(h.AgentPort)))
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodPost, url, bytes.NewReader(certPEM))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/x-pem-file")
	resp, err := s.cam.tlsClient.Do(req)
	if err != nil {
		return false, fmt.Errorf("agent unreachable: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("agent rejected the certificate (HTTP %d)", resp.StatusCode)
	}
	return true, nil
}

// certNotAfter 解析 PEM 证书链的叶子到期时刻（解析失败返回零值，调用方
// 容忍——续期主体已由 Renew 的返回值保证）。
func certNotAfter(certPEM []byte) time.Time {
	blk, _ := pem.Decode(certPEM)
	if blk == nil {
		return time.Time{}
	}
	leaf, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		return time.Time{}
	}
	return leaf.NotAfter
}
