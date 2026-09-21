package web

// 主机纳管：server 签发一次性 token → 目标机执行下发脚本 → 脚本按
// token 从 server 拉取对应架构二进制与逐主机证书，装配 systemd 常驻
// agent 并回报完成。信任模型：
//
//   - token 即能力凭证：TTL 内有效、成功一次即消费（done 阶段）；
//     下载类端点只要求未用未过期（安装失败可重试，直到成功或过期）
//   - 脚本内嵌 server CA 文件指纹：目标机校验下载到的 ca.crt 未被替换，
//     后续请求经 --cacert 走 TLS——首次下载通道的安全锚
//   - 逐主机证书 SAN = 来源 IP + 执行机 hostname（NAT 内网 IP 与逻辑名
//     双覆盖）；server 自举的控制端客户端证书（ctl）用于后续 mTLS 通信
//     与探活
//
// 二进制来源：server 可执行文件同级的 wdp-<os>-<arch>（build.sh 全平台
// bin 目录，internal/agentbin 同级查找）。

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"text/template"
	"time"

	"wdp/internal/ca"
	"wdp/internal/model"
	"wdp/internal/shellquote"
	"wdp/internal/store"
)

// DefaultEnrollTTL 纳管 token 默认有效期。
const DefaultEnrollTTL = 30 * time.Minute

// DefaultAgentCertDays 纳管签发的 agent 服务端证书有效期（不超 CA）。
const DefaultAgentCertDays = 365

// platformKeyRe 限制二进制下载的平台键（防路径构造）。
var platformKeyRe = regexp.MustCompile(`^[a-z0-9]+_[a-z0-9]+$`)

// caMaterial 是 server 自举的信任链（数据目录 ca/ 下）。
type caMaterial struct {
	dir       string // <data>/ca
	caPath    string // ca.crt
	caKeyPath string // ca.key
	ctlCert   string // ctl.crt（控制端客户端证书）
	ctlKey    string // ctl.key
	caFP      string // ca.crt 文件级 sha256（脚本校验下载完整性用）

	// 内联 PEM（server 发起的 agent 连接注入 model.Host，免重复读盘）
	caPEM, ctlCertPEM, ctlKeyPEM []byte

	tlsClient *http.Client
}

// bootstrapCA 首启自举：<caDir>/ca.crt 不存在则建 CA（默认 10 年），
// ctl 客户端证书缺失则签发。已存在的组织 CA 原样复用。
func bootstrapCA(caDir string, caDays int) (*caMaterial, error) {
	if caDays <= 0 {
		caDays = 3650
	}
	caPath, keyPath := filepath.Join(caDir, ca.DefaultCAFile), filepath.Join(caDir, ca.DefaultKeyFile)
	if _, err := os.Stat(caPath); errors.Is(err, os.ErrNotExist) {
		if _, _, _, err := ca.Init(ca.InitOptions{Dir: caDir, Days: caDays}); err != nil {
			return nil, fmt.Errorf("init CA: %w", err)
		}
	} else if err != nil {
		return nil, err
	}
	ctlCert, ctlKey := filepath.Join(caDir, "ctl.crt"), filepath.Join(caDir, "ctl.key")
	if _, err := os.Stat(ctlCert); errors.Is(err, os.ErrNotExist) {
		if _, _, _, err := ca.Issue(ca.IssueOptions{Dir: caDir, Profile: ca.ProfileClient}, "ctl"); err != nil {
			return nil, fmt.Errorf("issue control cert: %w", err)
		}
	} else if err != nil {
		return nil, err
	}
	pemBytes, err := os.ReadFile(caPath)
	if err != nil {
		return nil, err
	}
	m := &caMaterial{dir: caDir, caPath: caPath, caKeyPath: keyPath, ctlCert: ctlCert, ctlKey: ctlKey, caPEM: pemBytes}
	sum := sha256.Sum256(pemBytes)
	m.caFP = hex.EncodeToString(sum[:])
	ctlCertPEM, err := os.ReadFile(ctlCert)
	if err != nil {
		return nil, err
	}
	ctlKeyPEM, err := os.ReadFile(ctlKey)
	if err != nil {
		return nil, err
	}
	m.ctlCertPEM, m.ctlKeyPEM = ctlCertPEM, ctlKeyPEM

	// 控制端 mTLS 客户端（探活 / 远程执行用）：信任自建 CA + ctl 客户端证书
	cert, err := tls.LoadX509KeyPair(ctlCert, ctlKey)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pemBytes) {
		return nil, errors.New("parse CA pem failed")
	}
	m.tlsClient = &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			Proxy: nil, // agent 直连，不走环境代理（HTTP_PROXY 可能把请求带向无关服务）
			TLSClientConfig: &tls.Config{
				MinVersion:   tls.VersionTLS12,
				RootCAs:      pool,
				Certificates: []tls.Certificate{cert},
				// 只验链不验名：证书 SAN 是纳管时的来源 IP/主机名，NAT 或
				// 后续改地址都可能不一致；链校验保证必为自建 CA 签发
				// （Go 无原生"只验链"开关，关内置校验后手动补链校验）
				InsecureSkipVerify:    true,
				VerifyPeerCertificate: chainOnlyVerifier(pool),
			},
		},
	}
	return m, nil
}

// chainOnlyVerifier 仅做证书链校验（含中间链），不做主机名匹配。
func chainOnlyVerifier(roots *x509.CertPool) func([][]byte, [][]*x509.Certificate) error {
	return func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		if len(rawCerts) == 0 {
			return errors.New("server did not provide a certificate")
		}
		certs := make([]*x509.Certificate, 0, len(rawCerts))
		for _, raw := range rawCerts {
			c, err := x509.ParseCertificate(raw)
			if err != nil {
				return fmt.Errorf("parse server certificate: %w", err)
			}
			certs = append(certs, c)
		}
		opts := x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
		if len(certs) > 1 {
			opts.Intermediates = x509.NewCertPool()
			for _, c := range certs[1:] {
				opts.Intermediates.AddCert(c)
			}
		}
		if _, err := certs[0].Verify(opts); err != nil {
			return fmt.Errorf("server certificate chain verification failed: %w", err)
		}
		return nil
	}
}

// agentHostModel 构造 server 侧发起的 agent 连接主机模型：scheme=https 时
// 注入 ctl mTLS 材料（内联 PEM）与链校验；http 为明文直连（手工加的回环
// agent）。是否走 TLS 由调用方按探活结果决定。
func (s *Server) agentHostModel(h *store.Host, scheme string) *model.Host {
	m := &model.Host{Name: h.Name, Address: h.Address, AgentPort: h.AgentPort, Conn: "agent"}
	if scheme == "https" && s.cam != nil {
		m.TLS = true
		m.CAData = s.cam.caPEM
		m.CertData = s.cam.ctlCertPEM
		m.KeyData = s.cam.ctlKeyPEM
		m.TLSSkipHostVerify = true // 链校验 + 跳过主机名（SAN 为纳管来源 IP/主机名）
	}
	return m
}

// enrollScriptModel 是下发脚本的模板数据。
type enrollScriptModel struct {
	Base     string // server 外部可达基址（http://host:port）
	Token    string
	CAFP     string // ca.crt 文件 sha256 hex
	UnitName string
}

// enrollScriptTmpl 下发脚本（POSIX sh；目标机需要 curl 与 systemd）。
// unit 体与 SSH 推装共用 agentUnitFile；Base/Token 等插值在执行前完成
// 引用（Base 经 shellquote，token/指纹为 server 生成的 hex 无需转义）。
var enrollScriptTmpl = template.Must(template.New("enroll").Funcs(template.FuncMap{
	"unitBody": func() string { return agentUnitFile("$BIN_DIR", "$ETC_DIR", "$LOG_FILE", 7602) },
}).Parse(`#!/bin/sh
# wdp agent 纳管脚本（由 server 生成；token 单次有效）
set -eu

BASE={{ .Base }}
TOKEN='{{ .Token }}'
CA_FP='{{ .CAFP }}'
BIN_DIR=/usr/local/bin
ETC_DIR=/etc/wdp
UNIT={{ .UnitName }}
LOG_FILE=/var/log/wdp-agent.log

[ "$(id -u)" = 0 ] || { echo "run as root (sudo)" >&2; exit 1; }
command -v curl >/dev/null || { echo "curl is required" >&2; exit 1; }

# 平台探测（与 server 二进制目录命名一致）
case "$(uname -s)/$(uname -m)" in
  Linux/x86_64)  PLATFORM=linux_amd64 ;;
  Linux/aarch64) PLATFORM=linux_arm64 ;;
  *) echo "unsupported platform: $(uname -s)/$(uname -m)" >&2; exit 1 ;;
esac

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

# 1. CA（校验文件指纹——首次下载通道的安全锚）
curl -fsSL "$BASE/enroll/$TOKEN/ca" -o "$TMP/ca.crt"
GOT=$(sha256sum "$TMP/ca.crt" | awk '{print $1}')
[ "$GOT" = "$CA_FP" ] || { echo "CA fingerprint mismatch (expected $CA_FP, got $GOT)" >&2; exit 1; }

# 2. 二进制（server bin 目录同级对应平台产物）
curl -fsSL --cacert "$TMP/ca.crt" "$BASE/enroll/$TOKEN/binary/$PLATFORM" -o "$TMP/wdp"
"$TMP/wdp" agent --help >/dev/null 2>&1 || { echo "downloaded binary cannot run on this host" >&2; exit 1; }

# 3. 登记（记录 hostname 与来源地址，供证书 SAN 与落账）
curl -fsSL --cacert "$TMP/ca.crt" -X POST -H 'Content-Type: application/json' \
  -d "{\"hostname\":\"$(uname -n)\",\"platform\":\"$PLATFORM\"}" \
  "$BASE/enroll/$TOKEN/claim" >/dev/null

# 4. 证书三件套
curl -fsSL --cacert "$TMP/ca.crt" "$BASE/enroll/$TOKEN/host-cert" -o "$TMP/agent.crt"
curl -fsSL --cacert "$TMP/ca.crt" "$BASE/enroll/$TOKEN/host-key" -o "$TMP/agent.key"

# 5. 落盘 + systemd 常驻
install -m 0755 "$TMP/wdp" "$BIN_DIR/wdp"
mkdir -p "$ETC_DIR"
install -m 0644 "$TMP/ca.crt" "$ETC_DIR/ca.crt"
install -m 0644 "$TMP/agent.crt" "$ETC_DIR/agent.crt"
install -m 0600 "$TMP/agent.key" "$ETC_DIR/agent.key"

cat > "/etc/systemd/system/$UNIT.service" <<UNIT_EOF
{{ unitBody }}UNIT_EOF

systemctl daemon-reload
systemctl enable --now "$UNIT"
sleep 1
systemctl is-active --quiet "$UNIT" || { systemctl status "$UNIT" >&2 || true; exit 1; }

# 6. 完成（消费 token、写入 server 台账）
curl -fsSL --cacert "$TMP/ca.crt" -X POST -H 'Content-Type: application/json' \
  -d '{"agent_port":7602}' "$BASE/enroll/$TOKEN/done" >/dev/null

echo "wdp agent enrolled and running (unit $UNIT)"
`))

func (s *Server) routesEnroll() {
	s.mux.HandleFunc("POST /api/enroll-tokens", s.requirePerm(verbHostEnroll, s.handleCreateEnrollToken))
	s.mux.HandleFunc("GET /enroll/{token}/script.sh", s.handleEnrollScript)
	s.mux.HandleFunc("GET /enroll/{token}/ca", s.handleEnrollCA)
	s.mux.HandleFunc("GET /enroll/{token}/binary/{platform}", s.handleEnrollBinary)
	s.mux.HandleFunc("POST /enroll/{token}/claim", s.handleEnrollClaim)
	s.mux.HandleFunc("GET /enroll/{token}/host-cert", s.handleEnrollHostCert)
	s.mux.HandleFunc("GET /enroll/{token}/host-key", s.handleEnrollHostKey)
	s.mux.HandleFunc("POST /enroll/{token}/done", s.handleEnrollDone)
}

// handleCreateEnrollToken 生成纳管凭证，返回一键命令。
func (s *Server) handleCreateEnrollToken(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Host   string `json:"host"`
		Port   int    `json:"agent_port"`
		TTLMin int    `json:"ttl_min"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	ttl := DefaultEnrollTTL
	if req.TTLMin > 0 {
		ttl = time.Duration(req.TTLMin) * time.Minute
	}
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		s.writeInternal(w, err)
		return
	}
	token := hex.EncodeToString(buf)
	if err := s.st.CreateEnrollToken(token, req.Host, req.Port, ttl); err != nil {
		s.writeInternal(w, err)
		return
	}
	base, err := s.advertiseBase(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.audit(r, "create", "enroll_token", req.Host, fmt.Sprintf("一次性纳管 token（%d 分钟有效）", int(ttl.Minutes())))
	writeJSON(w, http.StatusCreated, map[string]any{
		"token":      token,
		"expires_in": int(ttl.Minutes()),
		"command":    fmt.Sprintf("curl -fsSL %s/enroll/%s/script.sh | sudo sh", base, token),
	})
}

// hostHeaderNameRe Host 头 host 部分的合法字符集（域名字符；IPv4/IPv6 由
// net.ParseIP 判定）。
var hostHeaderNameRe = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

// advertiseBase 返回脚本可用的 server 基址：显式配置优先，否则取请求
// Host 头（脚本与 API 同源下载）。Host 头内容进入下发脚本（curl | sh
// 消费），host 部分必须是合法 IP/域名字符集——引号/元字符直接报错。
func (s *Server) advertiseBase(r *http.Request) (string, error) {
	if s.opts.AdvertiseURL != "" {
		return strings.TrimRight(s.opts.AdvertiseURL, "/"), nil
	}
	host := r.Host
	if h, _, err := net.SplitHostPort(r.Host); err == nil {
		host = h
	}
	if net.ParseIP(host) == nil && !hostHeaderNameRe.MatchString(host) {
		return "", errors.New("invalid Host header")
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host, nil
}

// enrollTokenGate 公共校验：token 未用未过期，否则 410。
func (s *Server) enrollTokenGate(w http.ResponseWriter, r *http.Request) *store.EnrollToken {
	t, err := s.st.GetEnrollToken(r.PathValue("token"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "unknown enroll token")
		return nil
	}
	if err != nil {
		s.writeInternal(w, err)
		return nil
	}
	if t.UsedAt != "" {
		writeError(w, http.StatusGone, "enroll token already used")
		return nil
	}
	if timeNowUTC().After(mustTime(t.ExpiresAt)) {
		writeError(w, http.StatusGone, "enroll token expired")
		return nil
	}
	return t
}

func (s *Server) handleEnrollScript(w http.ResponseWriter, r *http.Request) {
	t := s.enrollTokenGate(w, r)
	if t == nil {
		return
	}
	base, err := s.advertiseBase(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	_ = enrollScriptTmpl.Execute(w, &enrollScriptModel{
		Base: shellquote.Quote(base), Token: t.Token, CAFP: s.cam.caFP, UnitName: agentUnitName,
	})
}

func (s *Server) handleEnrollCA(w http.ResponseWriter, r *http.Request) {
	if s.enrollTokenGate(w, r) == nil {
		return
	}
	b, err := os.ReadFile(s.cam.caPath)
	if err != nil {
		s.writeInternal(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/x-pem-file")
	_, _ = w.Write(b)
}

// handleEnrollBinary 下发对应平台二进制（server 可执行文件同级）。
func (s *Server) handleEnrollBinary(w http.ResponseWriter, r *http.Request) {
	if s.enrollTokenGate(w, r) == nil {
		return
	}
	platform := r.PathValue("platform")
	if !platformKeyRe.MatchString(platform) {
		writeError(w, http.StatusBadRequest, "invalid platform key")
		return
	}
	path, ok := s.binResolver(platform)
	if !ok {
		writeError(w, http.StatusNotFound, fmt.Sprintf("no binary for platform %s (run the server from a build.sh bin directory)", platform))
		return
	}
	f, err := os.Open(path)
	if err != nil {
		s.writeInternal(w, err)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	_, _ = io.Copy(w, f)
}

// handleEnrollClaim 记录执行机 hostname 与来源地址（幂等）。
func (s *Server) handleEnrollClaim(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Hostname string `json:"hostname"`
		Platform string `json:"platform"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	t, err := s.st.ClaimEnrollToken(r.PathValue("token"), req.Hostname, s.remoteIP(r))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "unknown enroll token")
		return
	}
	if err != nil {
		writeError(w, http.StatusGone, err.Error())
		return
	}
	// claim 首次到达时签发逐主机证书（SAN：来源 IP + hostname + 绑定名）
	if err := s.issueHostCert(t); err != nil {
		s.writeInternal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// hostCertPaths 逐主机证书落盘路径（<caDir>/hosts/<name>.crt|.key）。
func (s *Server) hostCertPaths(name string) (string, string) {
	safe := safeName(name)
	return filepath.Join(s.cam.dir, "hosts", safe+".crt"), filepath.Join(s.cam.dir, "hosts", safe+".key")
}

// issueHostCert 为凭证对应主机签发服务端证书（已存在则跳过——claim 幂等）。
func (s *Server) issueHostCert(t *store.EnrollToken) error {
	name := t.HostName
	if name == "" {
		name = t.ClaimHost
	}
	if name == "" {
		name = t.ClaimAddress
	}
	crt, _ := s.hostCertPaths(name)
	if _, err := os.Stat(crt); err == nil {
		return nil
	}
	sans := []string{}
	if ip := net.ParseIP(t.ClaimAddress); ip != nil {
		sans = append(sans, t.ClaimAddress)
	}
	if t.ClaimHost != "" && t.ClaimHost != t.ClaimAddress {
		sans = append(sans, t.ClaimHost)
	}
	if t.HostName != "" && t.HostName != t.ClaimAddress && t.HostName != t.ClaimHost {
		sans = append(sans, t.HostName)
	}
	if len(sans) == 0 {
		return fmt.Errorf("no SAN available for host cert (claim info empty)")
	}
	_, _, _, err := ca.Issue(ca.IssueOptions{
		Dir: filepath.Join(s.cam.dir, "hosts"), CACertPath: s.cam.caPath, CAKeyPath: s.cam.caKeyPath,
		SANs: sans, Profile: ca.ProfileServer, Days: DefaultAgentCertDays,
	}, safeName(name))
	return err
}

func (s *Server) handleEnrollHostCert(w http.ResponseWriter, r *http.Request) {
	s.serveHostCertFile(w, r, ".crt", "application/x-pem-file")
}

func (s *Server) handleEnrollHostKey(w http.ResponseWriter, r *http.Request) {
	s.serveHostCertFile(w, r, ".key", "application/x-pem-file")
}

func (s *Server) serveHostCertFile(w http.ResponseWriter, r *http.Request, ext, ctype string) {
	t, err := s.st.GetEnrollToken(r.PathValue("token"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "unknown enroll token")
		return
	}
	if err != nil {
		s.writeInternal(w, err)
		return
	}
	if t.UsedAt != "" || timeNowUTC().After(mustTime(t.ExpiresAt)) || t.ClaimHost == "" {
		writeError(w, http.StatusGone, "claim the token first")
		return
	}
	name := t.HostName
	if name == "" {
		name = t.ClaimHost
	}
	if name == "" {
		name = t.ClaimAddress
	}
	b, err := os.ReadFile(filepath.Join(s.cam.dir, "hosts", safeName(name)+ext))
	if err != nil {
		s.writeInternal(w, err)
		return
	}
	w.Header().Set("Content-Type", ctype)
	_, _ = w.Write(b)
}

// handleEnrollDone 消费 token、落账，返回台账名。
func (s *Server) handleEnrollDone(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AgentPort int `json:"agent_port"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	t, err := s.st.ConsumeEnrollToken(r.PathValue("token"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "unknown enroll token")
		return
	}
	if err != nil {
		writeError(w, http.StatusGone, err.Error())
		return
	}
	name := t.HostName
	if name == "" {
		name = t.ClaimHost
	}
	if name == "" {
		name = t.ClaimAddress
	}
	port := req.AgentPort
	if port <= 0 {
		port = t.AgentPort
	}
	id, err := s.st.UpsertHostByName(safeName(name), t.ClaimAddress, port)
	if err != nil {
		s.writeInternal(w, err)
		return
	}
	s.logger.Info("host enrolled", "name", name, "address", t.ClaimAddress, "id", id)
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "name": safeName(name), "address": t.ClaimAddress})
}

// safeName 归一化为可作文件名/台账名的安全串（路径分隔符等替换）。
func safeName(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '.', r == '_':
			return r
		}
		return '_'
	}, s)
}

// remoteIP 取请求来源 IP。仅当直连对端在信任代理列表（回环 + 显式配置，
// 见 Options.TrustedProxies）时才采信 X-Forwarded-For 首段且要求其可解析
// 为 IP：直连场景客户端可任意伪造 XFF，无条件采信会让审计 IP 与纳管
// ClaimAddress/证书 SAN 全部可伪造。
func (s *Server) remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if peer := net.ParseIP(host); peer != nil {
		for _, n := range s.trustedProxies {
			if n.Contains(peer) {
				if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
					first := xff
					if i := strings.IndexByte(xff, ','); i > 0 {
						first = xff[:i]
					}
					if ip := net.ParseIP(strings.TrimSpace(first)); ip != nil {
						return ip.String()
					}
				}
				break
			}
		}
	}
	return host
}

func timeNowUTC() time.Time { return time.Now().UTC() }

func mustTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339, s)
	return t
}

// mtlsProbeClient 返回控制端 mTLS 客户端（未启用纳管时为 nil，探活走明文）。
func (s *Server) mtlsProbeClient() *http.Client {
	if s.cam == nil {
		return nil
	}
	return s.cam.tlsClient
}
