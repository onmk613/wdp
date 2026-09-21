package web

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// TestSSHInstallValidation 请求校验与前置条件。
func TestSSHInstallValidation(t *testing.T) {
	s, _ := newEnrollServer(t)
	h := s.Handler()
	token := loginSession(t, s)

	// 未登录拒绝
	if rec := do(t, h, "POST", "/api/agents/install", map[string]any{"address": "10.0.0.1"}, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("未登录应 401: %d", rec.Code)
	}
	// 缺地址
	if rec := do(t, h, "POST", "/api/agents/install", map[string]any{}, &token); rec.Code != http.StatusBadRequest {
		t.Fatalf("缺地址应 400: %d %s", rec.Code, rec.Body)
	}
	// 不可达地址 → 502（连接被拒），不 panic
	rec := do(t, h, "POST", "/api/agents/install", map[string]any{
		"address": "127.0.0.1", "ssh_port": 1, "verify_host_key": false,
	}, &token)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("不可达应 502: %d %s", rec.Code, rec.Body)
	}
	var errResp struct{ Error string }
	json.Unmarshal(rec.Body.Bytes(), &errResp)
	if !strings.Contains(errResp.Error, "ssh connect") {
		t.Fatalf("错误信息应含连接上下文: %s", errResp.Error)
	}
}

// TestSSHUnitFile systemd 单元内容。
func TestSSHUnitFile(t *testing.T) {
	u := sshUnitFile(7700)
	for _, want := range []string{
		"ExecStart=/usr/local/bin/wdp agent --listen 0.0.0.0:7700",
		"--ca /etc/wdp/ca.crt", "--cert /etc/wdp/agent.crt", "--key /etc/wdp/agent.key",
		"--log-file /var/log/wdp-agent.log", "Restart=always", "WantedBy=multi-user.target",
	} {
		if !strings.Contains(u, want) {
			t.Fatalf("unit 缺 %q:\n%s", want, u)
		}
	}
}

// TestSSHInstallRequiresCA 未配置纳管 CA 时明确 503。
func TestSSHInstallRequiresCA(t *testing.T) {
	s, _ := newTestServer(t) // 无 CADir
	token := loginSession(t, s)
	rec := do(t, s.Handler(), "POST", "/api/agents/install", map[string]any{"address": "10.0.0.1"}, &token)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("无 CA 应 503: %d", rec.Code)
	}
}
