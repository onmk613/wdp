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
	"strconv"
	"strings"
	"sync"
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
	// clients 按 SNI/校验名缓存 mTLS 客户端：证书校验必须按台账主机
	// 身份进行（见 probeClientFor），逐主机建 Transport 会丢掉连接池，
	// 故按校验名复用。
	clientMu sync.Mutex
	clients  map[string]*http.Client
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
		// 显式给有效期：默认自动档只有 30 天（相对 CA 剩余寿命），而
		// ctl 到期后全部 https 探活/执行失败，运维现场表现为"agent 全
		// 不可达"——续期请见下面的自动延期
		if _, _, _, err := ca.Issue(ca.IssueOptions{Dir: caDir, Profile: ca.ProfileClient, Days: ctlCertDays}, "ctl"); err != nil {
			return nil, fmt.Errorf("issue control cert: %w", err)
		}
	} else if err != nil {
		return nil, err
	} else if err := renewCtlIfNeeded(ctlCert, ctlKey, caPath, keyPath); err != nil {
		// 续期失败不阻断启动：证书可能仍在有效期内，先让控制台可用并
		// 打日志（运维可在到期前用 wdp ca renew 手工处理）
		fmt.Fprintf(os.Stderr, "[wdp server] warning: control certificate renewal failed: %v\n", err)
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
	tr, err := newCtlTransport(pemBytes, ctlCertPEM, ctlKeyPEM)
	if err != nil {
		return nil, err
	}
	m.tlsClient = &http.Client{Timeout: 10 * time.Second, Transport: tr}
	return m, nil
}

// newCtlTransport 按信任链 PEM 构造控制端 mTLS Transport：bootstrapCA 与
// tlsClientFor 的断言失败兜底共用同一配方（单一来源，避免两处漂移）。
func newCtlTransport(caPEM, certPEM, keyPEM []byte) (*http.Transport, error) {
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("load control cert: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("parse CA pem failed")
	}
	return &http.Transport{
		Proxy: nil, // agent 直连，不走环境代理（HTTP_PROXY 可能把请求带向无关服务）
		TLSClientConfig: &tls.Config{
			MinVersion:   tls.VersionTLS12,
			RootCAs:      pool,
			Certificates: []tls.Certificate{cert},
			// 链校验由 RootCAs 完成；主机名（SNI/SAN）校验同样保留——
			// 逐主机客户端的 ServerName 由 probeClientFor/agentHostModel
			// 按台账地址设置。此前这里关掉校验再手动补链校验，等于
			// "任何本 CA 签发的证书都收"：任一台 agent 沦陷即可冒充
			// 任意其它主机（截获含 become 密码的 plan、伪造执行结果）。
		},
	}, nil
}

// clientPins 返回写进 agent 单元的控制端客户端证书 pin 指纹（可多个）；
// DER+SPKI 双指纹的动机见 unit.go 的 agentUnitFile。
func (s *Server) clientPins() []string {
	if s.cam == nil {
		return nil
	}
	pins := []string{}
	if fp, err := ca.FingerprintFile(s.cam.ctlCert); err == nil {
		pins = append(pins, fp)
	}
	if fp, err := ca.FingerprintPublicKeyFile(s.cam.ctlCert); err == nil {
		pins = append(pins, fp)
	}
	return pins
}

// ctlCertDays 是控制端客户端证书的有效期天数（显式签发，避免默认 30 天）。
const ctlCertDays = 365

// ctlRenewBefore 是"剩余寿命低于该值即自动续期"的阈值。续期保留密钥对，
// 因此写进 agent 单元的公钥（SPKI）pin 指纹不受影响。
const ctlRenewBefore = 30 * 24 * time.Hour

// renewCtlIfNeeded 检查控制端客户端证书剩余寿命，不足则自动续期。
// 不续期的后果：到期后 server→agent 的全部 mTLS 连接失败，现场只能靠
// 重建 CA 恢复（agent 侧 pin 与信任链都指向旧 CA）。
func renewCtlIfNeeded(ctlCert, ctlKey, caCert, caKey string) error {
	info, err := ca.Inspect(ctlCert)
	if err != nil {
		return fmt.Errorf("inspect control certificate: %w", err)
	}
	left := time.Until(info.NotAfter)
	if left > ctlRenewBefore {
		return nil
	}
	// OutPath 必须显式给原件路径：缺省是"当前工作目录 + 文件名"，
	// 既会把原件改名搬走，又把新件写到进程 CWD
	if _, _, _, err := ca.Renew(ca.RenewOptions{
		CertPath: ctlCert, KeyPath: ctlKey, CACertPath: caCert, CAKeyPath: caKey,
		OutPath: ctlCert, Days: ctlCertDays,
	}); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "[wdp server] control certificate renewed (was expiring in %s)\n", left.Round(time.Hour))
	return nil
}

// tlsClientFor 返回按 serverName 校验服务端证书的 mTLS 客户端（同名校验
// 复用同一 Transport，保留连接池）。serverName 为空 = 按连接地址校验
// （标准 TLS 行为）。
func (m *caMaterial) tlsClientFor(serverName string) *http.Client {
	m.clientMu.Lock()
	defer m.clientMu.Unlock()
	if m.clients == nil {
		m.clients = map[string]*http.Client{}
	}
	if c, ok := m.clients[serverName]; ok {
		return c
	}
	// comma-ok 断言：Transport 为 nil 或被替换为其它 RoundTripper 实现时，
	// 盲断言会把 nil 交给 Clone() 直接 panic。兜底用 newCtlTransport 按
	// caMaterial 自持的 PEM 现做同配方 Transport——不能回退
	// http.DefaultTransport：那会同时丢掉 mTLS 客户端证书与禁用环境代理
	// 两张安全底牌（丢证书校验等于任一 agent 可冒充其它主机）。
	base, ok := m.tlsClient.Transport.(*http.Transport)
	if !ok {
		var err error
		if base, err = newCtlTransport(m.caPEM, m.ctlCertPEM, m.ctlKeyPEM); err != nil {
			// PEM 损坏连兜底配方都建不出来：退回共享 tlsClient（证书按
			// 连接地址校验）。绝不返回 nil——调用方把 nil 当"明文探测"，
			// 对纳管主机是被注释明确禁止的安全降级
			m.clients[serverName] = m.tlsClient
			return m.tlsClient
		}
	}
	tr := base.Clone()
	tr.TLSClientConfig = tr.TLSClientConfig.Clone()
	tr.TLSClientConfig.ServerName = serverName
	c := &http.Client{Timeout: m.tlsClient.Timeout, Transport: tr}
	m.clients[serverName] = c
	return c
}

// agentHostModel 构造 server 侧发起的 agent 连接主机模型。
//
// 传输方式由**台账 + CA 状态**决定，而不是探活结果：签发过逐主机证书的
// 主机一律走 mTLS，探不通就让它探不通（executor 报 UNREACHABLE）。
// 此前"探活失败即回落明文"是可被主动触发的降级——中间人阻断 TLS 后
// 自己以明文应答，控制台就把脚本、become 密码、制品与 agent 二进制
// 全部明文送出去。未纳管（从未签发证书）的手工台账主机才允许明文。
//
// 证书校验按台账地址（ServerName）进行，不再"只验链不验名"：纳管签发的
// SAN 里本就含该地址，只验链会让任一台持本 CA 证书的 agent 可以冒充任意
// 其它主机（截获含 become 密码的 plan、伪造执行结果与 facts）。
func (s *Server) agentHostModel(h *store.Host) *model.Host {
	m := &model.Host{Name: h.Name, Address: h.Address, AgentPort: h.AgentPort, Conn: "agent"}
	if s.useTLS(h) {
		m.TLS = true
		m.CAData = s.cam.caPEM
		m.CertData = s.cam.ctlCertPEM
		m.KeyData = s.cam.ctlKeyPEM
		m.TLSServerName = hostNameOf(h.Address)
	}
	return m
}

// agentScheme 返回与该主机通信实际使用的 scheme（展示/指标用）。
func (s *Server) agentScheme(h *store.Host) string {
	if s.useTLS(h) {
		return "https"
	}
	return "http"
}

// hostNameOf 去掉可能的端口，得到用于证书校验的裸主机名。
func hostNameOf(addr string) string {
	if h, _, err := net.SplitHostPort(addr); err == nil && h != "" {
		return h
	}
	return addr
}

// useTLS 报告与控制台与该主机通信是否必须走 mTLS：
// 启用了纳管信任链（CA 就绪）且台账未显式声明明文。
//
// 判据放在台账字段上而不是"探活结果"或"证书文件是否存在"：
//   - 探活结果可被中间人操纵（阻断 TLS 后自行明文应答 → 通道降级）；
//   - 证书文件名随台账改名漂移，用存在性判断会在改名后静默降级明文。
func (s *Server) useTLS(h *store.Host) bool {
	return s.cam != nil && h != nil && !h.AllowPlaintext
}

// probeClientFor 返回该主机探活应使用的 HTTP 客户端：需要 mTLS 时返回
// 校验名 = 台账地址的客户端，否则返回 nil（调用方走明文探测）。
func (s *Server) probeClientFor(h *store.Host) *http.Client {
	if !s.useTLS(h) {
		return nil
	}
	return s.cam.tlsClientFor(hostNameOf(h.Address))
}

// enrollScriptModel 是下发脚本的模板数据。
type enrollScriptModel struct {
	Base      string // server 外部可达基址（https://host:port）
	Token     string
	CAFP      string // ca.crt 文件 sha256 hex
	UnitName  string
	AgentPort int // token 绑定的 agent 监听端口（store 层已保证 <=0 缺省 7602）
	// UnitBody 是 systemd 单元内容（含控制端客户端证书 pin 指纹）。
	// 预渲染成字符串传入：pin 指纹来自 server 状态（ctl.crt），
	// 模板函数闭包拿不到。
	UnitBody string
}

// enrollScriptTmpl 下发脚本（POSIX sh；目标机需要 curl 与 systemd）。
// unit 体与 SSH 推装共用 agentUnitFile；Base/Token 等插值在执行前完成
// 引用（Base 经 shellquote，token/指纹为 server 生成的 hex 无需转义）。
var enrollScriptTmpl = template.Must(template.New("enroll").Parse(`#!/bin/sh
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
# 完整性校验：向 server 取该平台的 sha256 并与下载物比对。通道已强制
# TLS，这一步防的是传输损坏、缓存错版与中间盒改写
curl -fsSL --cacert "$TMP/ca.crt" "$BASE/enroll/$TOKEN/binary-sha256/$PLATFORM" -o "$TMP/wdp.sha256"
WANT=$(tr -d ' \n\r' < "$TMP/wdp.sha256")
GOT=$(sha256sum "$TMP/wdp" | awk '{print $1}')
[ -n "$WANT" ] && [ "$WANT" = "$GOT" ] || { echo "agent binary sha256 mismatch (expected $WANT, got $GOT)" >&2; exit 1; }
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
{{ .UnitBody }}UNIT_EOF

systemctl daemon-reload
systemctl enable --now "$UNIT"
sleep 1
systemctl is-active --quiet "$UNIT" || { systemctl status "$UNIT" >&2 || true; exit 1; }

# 6. 完成（消费 token、写入 server 台账）——agent_port 用 token 绑定值，
# 使签发时指定的 agent_port 真正生效（此前硬编码 7602，API 字段形同虚设）
curl -fsSL --cacert "$TMP/ca.crt" -X POST -H 'Content-Type: application/json' \
  -d '{"agent_port":{{ .AgentPort }}}' "$BASE/enroll/$TOKEN/done" >/dev/null

echo "wdp agent enrolled and running (unit $UNIT)"
`))

func (s *Server) routesEnroll() {
	s.mux.HandleFunc("POST /api/enroll-tokens", s.requirePerm(verbHostEnroll, s.handleCreateEnrollToken))
	s.mux.HandleFunc("GET /enroll/{token}/script.sh", s.handleEnrollScript)
	s.mux.HandleFunc("GET /enroll/{token}/ca", s.handleEnrollCA)
	s.mux.HandleFunc("GET /enroll/{token}/binary/{platform}", s.handleEnrollBinary)
	s.mux.HandleFunc("GET /enroll/{token}/binary-sha256/{platform}", s.handleEnrollBinarySHA256)
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
	// TTL 上限：一次性凭证不应可签出数年有效期（token 会经 URL 留在反代
	// 日志与 shell 历史里，窗口越长泄露后的可利用时间越长）。超限显式 400
	// 拒绝而不是静默钳到 24h——调用方（前端/脚本）拿到的是自己请求的 TTL
	// 还是暗中缩水的 TTL，应当由响应明确告知。
	if ttl > 24*time.Hour {
		writeError(w, http.StatusBadRequest, "ttl_min exceeds the 24h cap (1440)")
		return
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
//
// 通道必须经 TLS（原生或可信反代）：下发脚本会以 root 执行，明文通道下
// 中间人替换脚本即可直接拿到目标机 root，而脚本内嵌的 CA 指纹与脚本
// **同源同通道**，对主动 MITM 零价值（它只防传输损坏）。确需明文引导
// （可信内网/离线演示）必须由运维显式打开 AllowPlaintextEnroll，且每次
// 生成命令都会记一条审计。
func (s *Server) advertiseBase(r *http.Request) (string, error) {
	if s.opts.AdvertiseURL != "" {
		base := strings.TrimRight(s.opts.AdvertiseURL, "/")
		if !s.opts.AllowPlaintextEnroll && !strings.HasPrefix(base, "https://") {
			return "", errors.New("advertise URL must be https:// (set --allow-plaintext-enroll to override on a trusted network)")
		}
		return base, nil
	}
	host := r.Host
	if h, port, err := net.SplitHostPort(r.Host); err == nil {
		// 端口串同样会原样进入 curl | sudo sh 提示串：非数字/超界（可含
		// 引号、元字符）必须拒绝，host 部分校验管不到这里
		if p, perr := strconv.Atoi(port); perr != nil || p < 1 || p > 65535 {
			return "", errors.New("invalid Host header")
		}
		host = h
	}
	if net.ParseIP(host) == nil && !hostHeaderNameRe.MatchString(host) {
		return "", errors.New("invalid Host header")
	}
	// 采信可信反代的 X-Forwarded-Proto（TLS 终止在反代时 r.TLS 为空）
	if !s.requestIsHTTPS(r) && !s.opts.AllowPlaintextEnroll {
		return "", errors.New("refusing to hand out an enroll command over plaintext HTTP: " +
			"enable --tls-cert/--tls-key, terminate TLS at a trusted proxy (--trust-proxy), " +
			"set --advertise https://…, or explicitly opt in with --allow-plaintext-enroll on a trusted network")
	}
	scheme := "http"
	if s.requestIsHTTPS(r) {
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
	// AgentPort 兜底缺省：store 层写入时已保证 <=0 → 7602，这里再防一手
	// 旧库遗留的 0/负值行（unit 的 --listen 与 done 回报都不接受非法端口）
	port := t.AgentPort
	if port <= 0 {
		port = 7602
	}
	_ = enrollScriptTmpl.Execute(w, &enrollScriptModel{
		Base: shellquote.Quote(base), Token: t.Token, CAFP: s.cam.caFP, UnitName: agentUnitName,
		AgentPort: port,
		UnitBody:  agentUnitFile("$BIN_DIR", "$ETC_DIR", "$LOG_FILE", port, s.clientPins()),
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

// agentBinarySHA256 返回指定平台 agent 二进制的 sha256（供脚本内嵌校验）。
// platform 为空或未知时返回空串（脚本跳过校验，不阻断纳管）。
func (s *Server) agentBinarySHA256(platform string) string {
	if platform == "" || !platformKeyRe.MatchString(platform) {
		return ""
	}
	path, ok := s.binResolver(platform)
	if !ok {
		return ""
	}
	sum, err := fileSHA256Hex(path)
	if err != nil {
		s.logger.Warn("enroll: hash agent binary", "path", path, "err", err)
		return ""
	}
	return sum
}

// fileSHA256Hex 流式计算文件 sha256。
func fileSHA256Hex(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
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

// handleEnrollBinarySHA256 返回该平台二进制的 sha256（纯文本），供目标机
// 在下载后做完整性校验。摘要与二进制同通道取回：它防的是传输损坏/缓存
// 错版/中间盒改写，抗主动 MITM 靠的是强制 TLS（见 advertiseBase）。
func (s *Server) handleEnrollBinarySHA256(w http.ResponseWriter, r *http.Request) {
	if s.enrollTokenGate(w, r) == nil {
		return
	}
	platform := r.PathValue("platform")
	sum := s.agentBinarySHA256(platform)
	if sum == "" {
		writeError(w, http.StatusNotFound, fmt.Sprintf("no binary for platform %s", platform))
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(w, sum+"\n")
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
	// 主机名碰撞校验必须在 ClaimEnrollToken **之前**：claim 一旦落库，
	// ClaimHost 即非空，被拒的请求仍能通过私钥交付的"已 claim"门卫。
	// token 未绑定主机名（名字完全由 claim 决定）时，不允许撞上台账既有
	// 主机——否则后续证书交付会把既有主机的证书/私钥交给 claim 方（私钥
	// 可冒充该 agent），done 还会改写台账地址。同名同址是重装，放行；
	// 同名异址拒绝。token 显式绑定主机名（管理员指定该主机重装/迁移）
	// 不受此限。
	tok := r.PathValue("token")
	pre, err := s.st.GetEnrollToken(tok)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "unknown enroll token")
		return
	}
	if err != nil {
		s.writeInternal(w, err)
		return
	}
	if pre.HostName == "" && req.Hostname != "" {
		name := safeName(req.Hostname)
		if existing, gerr := s.st.GetHostByName(name); gerr == nil && existing.Address != s.remoteIP(r) {
			s.logger.Warn("enroll claim refused: name collision",
				"name", name, "ledger_address", existing.Address, "claim_from", s.remoteIP(r))
			s.auditEntry(name, s.remoteIP(r), "enroll", "host", name,
				"claim 被拒绝：主机名与既有台账冲突（绑定该主机名重新生成 token）")
			writeError(w, http.StatusConflict,
				"host name already exists with a different address; re-enroll with a token bound to this host name")
			return
		}
	}
	t, err := s.st.ClaimEnrollToken(tok, req.Hostname, s.remoteIP(r))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "unknown enroll token")
		return
	}
	if err != nil {
		// 业务错误（过期/已用/被他人 claim）是合法 410；其余是 DB 故障，
		// 不得按 410 透出原始错误串（err.Error() 常带内部细节，见 httpx 口径）
		if errors.Is(err, store.ErrTokenUsed) || errors.Is(err, store.ErrTokenExpired) || store.IsBizErr(err) {
			writeError(w, http.StatusGone, err.Error())
			return
		}
		s.writeInternal(w, err)
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

// issueHostCert 为凭证对应主机签发服务端证书。已存在的同名证书只有在
// 其 SAN 覆盖本次 claim 身份（来源 IP 或 hostname）时才跳过——那才是
// 真正的"同机重装"幂等；不覆盖则按孤儿证书处理，重新签发覆盖。不做
// 归属校验的一刀切跳过会把既有主机的证书/私钥交付给撞名的 claim 方。
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
		if certCoversClaim(crt, t) {
			return nil
		}
		// 孤儿证书（台账主机已删/改名残留）：覆盖重签，匹配本次 claim 身份
		s.logger.Warn("enroll: reissuing host cert (existing cert does not cover claim identity)",
			"name", safeName(name), "claim_address", t.ClaimAddress, "claim_host", t.ClaimHost)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
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

// certCoversClaim 报告既有证书的 SAN 是否覆盖本次 claim 身份（来源 IP
// 或 hostname 任一命中即可——重装机器的 IP 或主机名至少一个保持不变）。
func certCoversClaim(crtPath string, t *store.EnrollToken) bool {
	info, err := ca.Inspect(crtPath)
	if err != nil {
		// 证书损坏读不出 SAN：按不覆盖处理（重签），宁可换证书也不误交付
		return false
	}
	for _, s := range append(append([]string{}, info.DNSNames...), info.IPs...) {
		if t.ClaimAddress != "" && s == t.ClaimAddress {
			return true
		}
		if t.ClaimHost != "" && s == t.ClaimHost {
			return true
		}
	}
	return false
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
	// 交付物必须回到 claim 的同一来源：token 会经 URL 出现在反代/网关
	// 访问日志与 shell 历史里，不绑定来源就等于"拿到日志即可取走逐主机
	// 私钥"（私钥能冒充该 agent，也能解开控制台与它的 mTLS 会话）。
	if t.ClaimAddress != "" && t.ClaimAddress != s.remoteIP(r) {
		s.logger.Warn("enroll delivery refused: source mismatch",
			"token", shortToken(t.Token), "claim", t.ClaimAddress, "from", s.remoteIP(r))
		writeError(w, http.StatusForbidden, "enroll delivery must come from the claiming host")
		return
	}
	// 私钥只在"证书尚未交付"的窗口内可取：done 之前任意次重试都可以拿到
	// 证书（安装可重跑），但私钥一旦被取走就不该再由 URL 里的 token 换出。
	if ext == ".key" && t.KeyDeliveredAt != "" {
		writeError(w, http.StatusGone, "host key already delivered; re-enroll with a fresh token")
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
	if ext == ".key" {
		// 私钥一旦交付即作废该路径：token 会留在访问日志与 shell 历史里，
		// 只靠 TTL 与来源绑定仍嫌宽（重装请重新生成 token）
		if err := s.st.MarkEnrollKeyDelivered(t.ID); err != nil {
			s.logger.Warn("enroll: mark key delivered", "token", shortToken(t.Token), "err", err)
		}
		s.auditEntry(name, s.remoteIP(r), "enroll", "host", safeName(name), "逐主机私钥已交付（一次性）")
	}
}

// handleEnrollDone 消费 token、落账，返回台账名。
func (s *Server) handleEnrollDone(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AgentPort int `json:"agent_port"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	// 守卫先于消费（消费前的快照做校验）：拒绝路径不能把 token 烧掉——
	// 目标机修正环境后重试 done 应仍可用。claim 身份落库后不可再变，
	// 消费前快照上的守卫结论对消费后的 t 同样成立。
	pre, err := s.st.GetEnrollToken(r.PathValue("token"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "unknown enroll token")
		return
	}
	if err != nil {
		s.writeInternal(w, err)
		return
	}
	// 从未 claim 过的 token 没有 claim 身份，落账既无名字也无地址——
	// 与证书交付同口径 410（被拒的撞名 claim 不会落 ClaimHost）。
	if pre.ClaimHost == "" && pre.ClaimAddress == "" {
		writeError(w, http.StatusGone, "claim the token first")
		return
	}
	// 落账前的最后一道同名异址守卫（claim 与 done 之间台账可能变化）：
	// 未绑定主机名的 token 不允许改写既有主机的地址。显式绑定的 token
	// 是管理员对该主机的重装/迁移授权，放行。
	if pre.HostName == "" {
		name := pre.HostName
		if name == "" {
			name = pre.ClaimHost
		}
		if name == "" {
			name = pre.ClaimAddress
		}
		if existing, gerr := s.st.GetHostByName(safeName(name)); gerr == nil && existing.Address != pre.ClaimAddress {
			s.logger.Warn("enroll done refused: would hijack ledger address",
				"name", safeName(name), "ledger_address", existing.Address, "claim_address", pre.ClaimAddress)
			writeError(w, http.StatusConflict,
				"host name already exists with a different address; re-enroll with a token bound to this host name")
			return
		}
	}
	t, err := s.st.ConsumeEnrollToken(r.PathValue("token"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "unknown enroll token")
		return
	}
	if err != nil {
		if errors.Is(err, store.ErrTokenUsed) || errors.Is(err, store.ErrTokenExpired) || store.IsBizErr(err) {
			writeError(w, http.StatusGone, err.Error())
			return
		}
		s.writeInternal(w, err)
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
	// 从未 claim 过的 token 没有 claim 身份，落账既无名字也无地址——
	// 与证书交付同口径 410（被拒的撞名 claim 不会落 ClaimHost）。
	if t.ClaimHost == "" && t.ClaimAddress == "" {
		writeError(w, http.StatusGone, "claim the token first")
		return
	}
	// 落账前的最后一道同名异址守卫（claim 与 done 之间台账可能变化）：
	// 未绑定主机名的 token 不允许改写既有主机的地址。显式绑定的 token
	// 是管理员对该主机的重装/迁移授权，放行。
	if t.HostName == "" {
		if existing, gerr := s.st.GetHostByName(safeName(name)); gerr == nil && existing.Address != t.ClaimAddress {
			s.logger.Warn("enroll done refused: would hijack ledger address",
				"name", safeName(name), "ledger_address", existing.Address, "claim_address", t.ClaimAddress)
			writeError(w, http.StatusConflict,
				"host name already exists with a different address; re-enroll with a token bound to this host name")
			return
		}
	}
	id, err := s.st.UpsertHostByName(safeName(name), t.ClaimAddress, port)
	if err != nil {
		s.writeInternal(w, err)
		return
	}
	s.logger.Info("host enrolled", "name", name, "address", t.ClaimAddress, "id", id)
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "name": safeName(name), "address": t.ClaimAddress})
}

// shortToken 截断 token 供日志标识（token 可能短于 8 字符：直接切片会越界）。
func shortToken(tok string) string {
	if len(tok) > 8 {
		return tok[:8]
	}
	return tok
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
// 见 Options.TrustedProxies）时才采信 X-Forwarded-For，且取**最后一段**：
// nginx 等反代的常见配置是追加语义（$proxy_add_x_forwarded_for），客户端
// 可自带任意伪造的首段，而最后一段由我们直连的可信代理追加，等于它
// 看到的真实对端地址。取首段会让审计 IP、纳管 ClaimAddress/证书 SAN、
// 登录限速键全部可被客户端伪造。非 IP 段（伪造垃圾）整体放弃采信。
func (s *Server) remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if peer := net.ParseIP(host); peer != nil {
		for _, n := range s.trustedProxies {
			if n.Contains(peer) {
				if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
					last := xff
					if i := strings.LastIndexByte(xff, ','); i >= 0 {
						last = xff[i+1:]
					}
					if ip := net.ParseIP(strings.TrimSpace(last)); ip != nil {
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

// requestIsHTTPS 判断请求经 TLS 到达：原生 TLS（r.TLS）或直连对端为
// 信任代理且 X-Forwarded-Proto 为 https（采信范围与 remoteIP 的 XFF
// 同口径：非信任对端的头可任意伪造）。会话 cookie 的 Secure 属性据此
// 设置——明文 HTTP 部署下不设（否则 cookie 永不回传），TLS 部署下
// 防 token 经明文 http:// 同域请求外泄。
func (s *Server) requestIsHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer := net.ParseIP(host)
	if peer == nil {
		return false
	}
	for _, n := range s.trustedProxies {
		if n.Contains(peer) {
			return strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
		}
	}
	return false
}

func mustTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339, s)
	return t
}
