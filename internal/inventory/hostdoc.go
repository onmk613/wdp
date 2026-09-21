package inventory

// 主机条目字段表（`wdp schema host` 数据源）。与 buildHost 的字段 switch
// 同包放置：文档长在解析器旁边。基线键（hostKeys）的对账在
// TestHostFieldsReconcile；连接包注册的专属键（agent）在 cli 包的
// 集成对账测试锁定（那里 blank import 了全部连接驱动，HostKeys() 全集
// 可得）。

import (
	"maps"
	"slices"

	"wdp/internal/model"
)

// HostKeys 返回当前已注册的全部主机条目连接参数键（含连接包经
// RegisterHostKeys 注册的；排序稳定）。非此清单的键进入主机变量域。
func HostKeys() []string {
	hostKeysMu.RLock()
	defer hostKeysMu.RUnlock()
	return slices.Sorted(maps.Keys(hostKeys))
}

// HostFieldSections 返回 inventory 主机条目字段（连接参数键）的分组表。
// 未列出的键一律视为主机变量（模板经 .hostvars 访问）。
func HostFieldSections() []model.FieldSection {
	return []model.FieldSection{
		{
			Title: "地址与通道",
			Fields: []model.FieldDoc{
				{Name: "host", Type: "string", Default: "主机名", Desc: "实际连接地址（IP 或域名）", GoField: "Address"},
				{Name: "conn", Type: "string", Default: "wdp.cfg [run].conn（agent）", Desc: "连接类型：agent | local | ssh（引导用）", GoField: "Conn"},
			},
			Example: `
web1: {host: 10.0.0.11, conn: agent}
`,
		},
		{
			Title: "SSH 通道（conn: ssh，引导用）",
			Fields: []model.FieldDoc{
				{Name: "port", Type: "int", Default: "22", Desc: "SSH 端口（1..65535，非法值报错）", GoField: "Port"},
				{Name: "user", Type: "string", Default: "root", Desc: "SSH 用户", GoField: "User"},
				{Name: "password", Type: "string", Default: "密钥认证", Desc: "SSH 密码（支持 \"env:VAR\" 引用环境变量，避免清单明文）", GoField: "Password"},
				{Name: "password_env", Type: "string", Default: "-", Desc: "SSH 密码环境变量名", GoField: "PasswordEnv"},
				{Name: "key_path", Type: "string", Default: "~/.ssh/id_ed25519 等", Desc: "私钥路径", GoField: "KeyPath"},
				{Name: "key_passphrase", Type: "string", Default: "-", Desc: "私钥口令（支持 \"env:VAR\"）", GoField: "KeyPassphrase"},
				{Name: "key_passphrase_env", Type: "string", Default: "-", Desc: "私钥口令环境变量名", GoField: "KeyPassphraseEnv"},
				{Name: "host_key_check", Type: "bool", Default: "true", Desc: "校验主机指纹（known_hosts）；新主机先 ssh-keyscan 采集指纹", GoField: "HostKeyCheck"},
				{Name: "known_hosts", Type: "string", Default: "~/.ssh/known_hosts", Desc: "known_hosts 路径", GoField: "KnownHosts"},
				{Name: "connect_timeout", Type: "int", Default: "10", Desc: "连接超时秒数", GoField: "ConnectTimeoutSec"},
			},
			Example: `
bootstrap: {host: 10.0.0.5, conn: ssh, user: deploy, key_path: ~/.ssh/deploy_ed25519}
`,
		},
		{
			Title: "提权口令",
			Fields: []model.FieldDoc{
				{Name: "become_password", Type: "string", Default: "免密 sudo", Desc: "sudo 密码（支持 \"env:VAR\"；未设置时假定免密 sudo）", GoField: "BecomePassword"},
				{Name: "become_password_env", Type: "string", Default: "-", Desc: "sudo 密码环境变量名", GoField: "BecomePasswordEnv"},
			},
			Example: `
oldbox: {host: 10.0.0.9, become_password_env: SUDO_PW}
`,
		},
		{
			Title: "agent 通道（conn: agent）",
			Fields: []model.FieldDoc{
				{Name: "agent_url", Type: "string", Default: "Address:agent_port", Desc: "agent 服务地址，如 http://10.0.0.1:7602", GoField: "AgentURL"},
				{Name: "agent_port", Type: "int", Default: "wdp.cfg [agent].port（7602）", Desc: "agent 端口（agent_url 为空时用 Address:agent_port）", GoField: "AgentPort"},
				{Name: "tls", Type: "bool", Default: "false", Desc: "启用 HTTPS（公网/系统证书池场景）", GoField: "TLS"},
				{Name: "ca_file", Type: "string", Default: "系统证书池", Desc: "mTLS：验证 agent 服务端的 CA 证书", GoField: "CAFile"},
				{Name: "cert_file", Type: "string", Default: "-", Desc: "mTLS：控制端客户端证书", GoField: "CertFile"},
				{Name: "key_file", Type: "string", Default: "-", Desc: "mTLS：控制端客户端私钥", GoField: "KeyFile"},
				{Name: "insecure_skip_verify", Type: "bool", Default: "false", Desc: "跳过全部服务端证书校验（链+主机名；明确声明的降级）", GoField: "InsecureSkipVerify"},
				{Name: "tls_skip_host_verify", Type: "bool", Default: "false", Desc: "仅跳过主机名（SAN）校验，保留 CA 链校验（NAT/端口转发下 SAN 与连接地址不符时用）", GoField: "TLSSkipHostVerify"},
				{Name: "tls_server_name", Type: "string", Default: "host 字段", Desc: "显式覆盖主机名校验目标（证书 SAN 为其它名称时指定）", GoField: "TLSServerName"},
			},
			Example: `
web2:
  host: 10.0.0.12
  conn: agent
  agent_url: https://10.0.0.12:7602
  ca_file: ca.crt
  cert_file: me.crt      # 控制端客户端证书（wdp ca issue 产物）
  key_file: me.key
`,
		},
	}
}
