package web

// SSH 推装 agent：适用于 server→目标机单向可达（目标机无法回连 server，
// 纳管拉取模式不可用）的网络。流程与 enroll 互补：
//
//	server --SSH--> 目标机：探测架构 → 上传二进制与 CA → 目标机本地
//	                       gencsr 生成密钥 → server 回签证书并回传 → systemd
//	server --mTLS--> agent ：health 验证 → 落账
//
// 私钥在目标机本地生成（docs/20）：不经 SSH 传输、不落 server 磁盘。
// SSH 凭据由请求显式提供、仅内存态使用，不落库。verify_host_key 缺省
// true（与 inventory 路径的安全默认一致）：推装通道承载 SSH 密码/私钥
// 口令认证、agent 二进制与证书的下发，中间人不仅可截获该主机自己的
// 证书，还能截获 SSH 凭据、替换二进制（持久化 RCE）。确需关闭时显式
// 传 false（受控内网的知情选择），生产建议预先在 server 侧 known_hosts
// 采集指纹。

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"wdp/internal/agent"
	"wdp/internal/agentbin"
	"wdp/internal/ca"
	"wdp/internal/conn"
	"wdp/internal/conn/sshc"
	"wdp/internal/model"
	"wdp/internal/store"
)

// SSHInstallRequest 是一次 SSH 推装的参数（凭据不落库）。
type SSHInstallRequest struct {
	Name          string `json:"name"`           // 台账名（空 = address）
	Address       string `json:"address"`        // SSH/agent 可达地址（必填）
	SSHPort       int    `json:"ssh_port"`       // SSH 端口（0 = 22）
	User          string `json:"user"`           // SSH 用户（空 = root）
	Password      string `json:"password"`       // SSH 密码（与 key_path 二选一）
	KeyPath       string `json:"key_path"`       // 私钥路径（空 = 密码或 server 默认密钥发现）
	KeyPassphrase string `json:"key_passphrase"` // 私钥口令
	// 指针类型区分「未传」（缺省 true，安全默认）与「显式 false」
	//（受控内网的知情选择）。此前 bool 零值 false 是不安全默认：CLI
	// inventory 路径默认校验指纹，Web 推装路径却默认放行。
	VerifyHostKey *bool `json:"verify_host_key"`
	AgentPort     int   `json:"agent_port"` // agent 监听端口（0 = 7602）
}

// sshUnitFile 生成 systemd 单元（与 enroll 脚本同款，公共实现见 unit.go）。
func (s *Server) sshUnitFile(agentPort int) string {
	return agentUnitFile("/usr/local/bin", "/etc/wdp", "/var/log/wdp-agent.log", agentPort, s.clientPins())
}

// handleSSHInstall 经 SSH 在目标机安装常驻 agent 并落账。
func (s *Server) handleSSHInstall(w http.ResponseWriter, r *http.Request) {
	if s.cam == nil {
		writeError(w, http.StatusServiceUnavailable, "enrollment CA not configured (start with --data)")
		return
	}
	// 请求体携带 root 级 SSH 凭据（密码/私钥口令）：明文 HTTP 下等同把
	// 凭据广播给链路窃听者。与纳管命令下发（advertiseBase）同一门禁口径
	// ——可信内网必须显式打开 AllowPlaintextEnroll。
	if !s.requestIsHTTPS(r) && !s.allowPlaintextEnroll() {
		writeError(w, http.StatusBadRequest, "refusing to accept SSH credentials over plaintext HTTP: "+
			"enable --tls-cert/--tls-key, terminate TLS at a trusted proxy (--trust-proxy), "+
			"or explicitly opt in with --allow-plaintext-enroll on a trusted network")
		return
	}
	var req SSHInstallRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Address) == "" {
		writeError(w, http.StatusBadRequest, "address is required")
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = req.Address
	}
	name = safeName(name)
	agentPort := req.AgentPort
	if agentPort <= 0 {
		agentPort = 7602
	}

	// SSH 连接参数（凭据仅内存态）
	sshPort := req.SSHPort
	if sshPort <= 0 {
		sshPort = 22
	}
	user := req.User
	if user == "" {
		user = "root"
	}
	verifyHostKey := true // 安全默认：未显式传 false 时校验主机指纹
	if req.VerifyHostKey != nil {
		verifyHostKey = *req.VerifyHostKey
	}
	host := &model.Host{
		Name: name, Address: req.Address, Port: sshPort, User: user,
		Password: req.Password, KeyPath: req.KeyPath, KeyPassphrase: req.KeyPassphrase,
		HostKeyCheck: verifyHostKey, ConnectTimeoutSec: 10,
	}

	// 执行脱离请求生命周期（同 exec/upgrade 口径），动机见 background()
	ctx, cancel := context.WithTimeout(s.background(), 10*time.Minute)
	defer cancel()

	result, err := s.sshInstall(ctx, host, name, agentPort)
	if err != nil {
		s.logger.Error("ssh install failed", "host", req.Address, "err", err)
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	s.audit(r, "install", "host", fmt.Sprint(result["name"]), fmt.Sprintf("SSH 推装上线 %s（%s）", result["address"], result["platform"]))
	writeJSON(w, http.StatusCreated, result)
}

// sshInstall 执行安装全流程（连接 → 探测 → 上传 → systemd → 验证 → 落账）。
func (s *Server) sshInstall(ctx context.Context, host *model.Host, name string, agentPort int) (map[string]any, error) {
	ssh := sshc.New(host)
	if err := ssh.Connect(ctx); err != nil {
		return nil, fmt.Errorf("ssh connect %s@%s:%d: %w", host.User, host.Address, host.Port, err)
	}
	defer ssh.Close()

	// 1. 平台探测 → 二进制（server 可执行文件同级的 bin 目录产物）
	out, err := sshExec(ctx, ssh, "uname -sm", 15*time.Second)
	if err != nil || out.Code != 0 {
		return nil, fmt.Errorf("detect platform: %v (code %d)", err, out.Code)
	}
	platform := agentbin.FromUname(out.Stdout)
	if platform == "" {
		return nil, fmt.Errorf("cannot detect platform (uname -sm: %q); unsupported target", strings.TrimSpace(out.Stdout))
	}
	binPath, ok := s.binResolver(platform)
	if !ok {
		return nil, fmt.Errorf("no binary for platform %s (run the server from a build.sh bin directory)", platform)
	}

	// 2. 上传二进制与 CA，目标机本地生成密钥并出 CSR——私钥不经 SSH
	//    传输、不落 server 磁盘（docs/20；gencsr 幂等：重装复用既有钥匙）
	if err := s.sshUpload(ctx, ssh, binPath, "/usr/local/bin/wdp", 0o755); err != nil {
		return nil, err
	}
	if out, err := sshExec(ctx, ssh, "mkdir -p /etc/wdp", 30*time.Second); err != nil || out.Code != 0 {
		return nil, fmt.Errorf("mkdir /etc/wdp: %v (code %d)", err, out.Code)
	}
	caPEM, err := os.ReadFile(s.cam.caPath)
	if err != nil {
		return nil, err
	}
	if err := s.sshUploadBytes(ctx, ssh, caPEM, "/etc/wdp/ca.crt", 0o644); err != nil {
		return nil, err
	}
	const targetCSR = "/tmp/wdp-enroll.csr"
	if out, err := sshExec(ctx, ssh,
		"/usr/local/bin/wdp agent gencsr --key /etc/wdp/agent.key --csr "+targetCSR, 30*time.Second); err != nil || out.Code != 0 {
		detail := ""
		if out.Stderr != "" {
			detail = ": " + out.Stderr
		}
		return nil, fmt.Errorf("generate key/CSR on target: %v (code %d)%s", err, out.Code, detail)
	}
	var csrBuf bytes.Buffer
	if err := ssh.DownloadFile(ctx, targetCSR, &csrBuf); err != nil {
		return nil, fmt.Errorf("download CSR: %w", err)
	}
	_, _ = sshExec(ctx, ssh, "rm -f "+targetCSR, 10*time.Second)

	// 3. 签发证书（SAN = 地址 + 台账名，由 server 决定；CSR 自报身份
	//    一律忽略）并回传——server 全程只见公钥
	csr, err := ca.ParseCSR(csrBuf.Bytes())
	if err != nil {
		return nil, fmt.Errorf("target CSR: %w", err)
	}
	sans := []string{host.Address}
	if name != host.Address {
		sans = append(sans, name)
	}
	crt, _, err := ca.SignCSR(ca.SignCSROptions{
		Dir: filepath.Join(s.cam.dir, "hosts"), CACertPath: s.cam.caPath, CAKeyPath: s.cam.caKeyPath,
		SANs: sans, Profile: ca.ProfileServer, Days: DefaultAgentCertDays,
	}, csr, name)
	if err != nil {
		return nil, fmt.Errorf("issue host cert: %w", err)
	}
	if err := s.sshUpload(ctx, ssh, crt, "/etc/wdp/agent.crt", 0o644); err != nil {
		return nil, err
	}

	// 4. systemd 装配（enable + restart：enable 只设自启不启动，restart
	// 统一拉起——enable --now 先起一进程再 restart 杀掉重启，日志里每次
	// 安装都会出现两对 "log file attached"/"listening" 启动行）
	script := fmt.Sprintf("cat > /etc/systemd/system/%[2]s.service <<'WDP_UNIT_EOF'\n%[1]sWDP_UNIT_EOF\nsystemctl daemon-reload && systemctl enable %[2]s && systemctl restart %[2]s && sleep 1 && systemctl is-active --quiet %[2]s",
		s.sshUnitFile(agentPort), agentUnitName)
	if out, err := sshExec(ctx, ssh, script, 60*time.Second); err != nil || out.Code != 0 {
		detail := ""
		if out.Stderr != "" {
			detail = ": " + out.Stderr
		}
		return nil, fmt.Errorf("install systemd unit: %v (code %d)%s", err, out.Code, detail)
	}

	// 5. server→agent 验证（mTLS 探活客户端）
	h := &store.Host{Name: name, Address: host.Address, AgentPort: agentPort}
	if res := probeHost(ctx, h, s.probeClientFor(h)); res.Status != "online" {
		return nil, fmt.Errorf("agent installed but health check failed: %s", res.Error)
	}

	// 6. 落账 + 置在线
	id, err := s.st.UpsertHostByName(name, host.Address, agentPort)
	if err != nil {
		return nil, err
	}
	_ = s.st.SetHostStatus(id, "online", agent.BuildVersion(), "")
	s.logger.Info("agent installed via ssh", "name", name, "address", host.Address, "platform", platform, "id", id)
	return map[string]any{"id": id, "name": name, "address": host.Address, "agent_port": agentPort, "status": "online", "platform": platform}, nil
}

// sshUpload 上传本地文件（临时文件 + 原子改名语义由 sshc 保证）。
func (s *Server) sshUpload(ctx context.Context, ssh *sshc.Conn, src, dst string, mode os.FileMode) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := ssh.UploadFile(ctx, dst, f, mode); err != nil {
		return fmt.Errorf("upload %s: %w", dst, err)
	}
	return nil
}

func (s *Server) sshUploadBytes(ctx context.Context, ssh *sshc.Conn, b []byte, dst string, mode os.FileMode) error {
	if err := ssh.UploadFile(ctx, dst, strings.NewReader(string(b)), mode); err != nil {
		return fmt.Errorf("upload %s: %w", dst, err)
	}
	return nil
}

// sshExec 带超时执行一段脚本。
func sshExec(ctx context.Context, ssh *sshc.Conn, script string, timeout time.Duration) (conn.ExecResult, error) {
	return ssh.Exec(ctx, conn.ExecRequest{Script: script, TimeoutMs: conn.Timeout(timeout)})
}
