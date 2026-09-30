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

// TestSSHUnitFile systemd 单元内容（含控制端客户端证书 pin）。
func TestSSHUnitFile(t *testing.T) {
	s, _ := newEnrollServer(t)
	u := s.sshUnitFile(7700)
	for _, want := range []string{
		"ExecStart=/usr/local/bin/wdp agent --listen 0.0.0.0:7700",
		"--ca /etc/wdp/ca.crt", "--cert /etc/wdp/agent.crt", "--key /etc/wdp/agent.key",
		"--log-file /var/log/wdp-agent.log", "Restart=always", "WantedBy=multi-user.target",
		// 客户端证书 pin：全网共用一张 ctl 证书时的可撤销点
		"--pin-client-fp sha256:",
	} {
		if !strings.Contains(u, want) {
			t.Fatalf("unit 缺 %q:\n%s", want, u)
		}
	}
	// 证书 DER 指纹与公钥（SPKI）指纹都要写入：后者跨续期存活
	if pins := s.clientPins(); len(pins) != 2 || pins[0] == pins[1] {
		t.Fatalf("应同时写入证书与公钥两种指纹: %v", pins)
	}
	// pin 必须逐个重复传参：--pin-client-fp 是 StringArray，逗号拼接会被
	// agent 当单个指纹解析失败，新装 agent 启动即退（P0 回归锁）
	pins := s.clientPins()
	if n := strings.Count(u, "--pin-client-fp"); n != len(pins) {
		t.Fatalf("应有 %d 个独立 --pin-client-fp，实际 %d:\n%s", len(pins), n, u)
	}
	for _, p := range pins {
		if !strings.Contains(u, "--pin-client-fp "+p) {
			t.Fatalf("unit 缺独立 pin %q:\n%s", p, u)
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

// TestSSHInstallRefusesPlaintext 回归：SSH 推装请求体携带 root 级凭据
// （密码/私钥口令），明文 HTTP 下必须与纳管命令下发同一口径默认拒绝；
// 可信内网显式打开 AllowPlaintextEnroll 后放行（推进到参数校验）。
func TestSSHInstallRefusesPlaintext(t *testing.T) {
	st := newEnrollOnlyStore(t)
	s, err := New(st, Options{AdminUser: "admin", CADir: t.TempDir()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	token := loginSession(t, s)

	rec := do(t, s.Handler(), "POST", "/api/agents/install", map[string]any{"address": "10.0.0.1"}, &token)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("明文下默认应拒绝接收 SSH 凭据: %d %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "plaintext") {
		t.Fatalf("拒绝理由应指明明文通道: %s", rec.Body)
	}

	s.opts.AllowPlaintextEnroll = true
	rec = do(t, s.Handler(), "POST", "/api/agents/install", map[string]any{}, &token)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "address") {
		t.Fatalf("显式打开开关后应放行门禁并推进到参数校验: %d %s", rec.Code, rec.Body)
	}
}
