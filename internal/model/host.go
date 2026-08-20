package model

import (
	"fmt"
	"os"
	"strings"
)

// Host 描述一台受控主机及其连接参数。
// 特殊连接参数从 inventory 主机条目中提取，其余键进入 Vars。
type Host struct {
	Name             string // inventory 中的主机名（inventory_hostname）
	Address          string // 实际连接地址，缺省等于 Name
	Port             int    // SSH 端口，缺省 22
	User             string // SSH 用户，缺省 root
	Password         string // SSH 密码认证（可选，支持 "env:VAR" 引用）
	PasswordEnv      string // SSH 密码环境变量（避免 inventory 明文）
	KeyPath          string // 私钥路径（可选，缺省尝试 ~/.ssh/id_ed25519、id_rsa）
	KeyPassphrase    string // 私钥口令（可选，支持 "env:VAR"）
	KeyPassphraseEnv string // 私钥口令环境变量

	HostKeyCheck      bool   // 校验主机指纹（known_hosts）
	KnownHosts        string // known_hosts 路径（缺省 ~/.ssh/known_hosts）
	ConnectTimeoutSec int    // 连接超时秒数（缺省 10）

	Conn      string // 连接类型：ssh | agent | local | push
	AgentURL  string // conn=agent 时的 agent 服务地址，如 http://10.0.0.1:7602
	AgentPort int    // agent/push 端口（AgentURL 为空时用 Address:AgentPort）

	// agent 通道 TLS 与认证（inventory 主机条目直接配置）
	TLS                bool   // 启用 HTTPS（公网/系统池证书场景）
	InsecureSkipVerify bool   // 跳过服务端证书校验（明确声明的降级）
	Token              string // token 认证（支持 "env:VAR"）
	TokenEnv           string // token 环境变量
	CAFile             string // mTLS：CA 证书（验证 agent 服务端；缺省信任系统证书池）
	CertFile           string // mTLS：控制端客户端证书
	KeyFile            string // mTLS：控制端客户端私钥

	// push 临时 agent（conn: push）
	BinaryPath string // 自举用的 wdp 二进制（缺省 os.Executable()）
	KeepAgent  bool   // 执行完保留临时 agent（调试用）

	BecomePassword    string // sudo 密码（可选，支持 "env:VAR"；缺省免密 sudo）
	BecomePasswordEnv string // sudo 密码环境变量

	Vars map[string]any // 主机级变量（含所属组变量合并后的最终结果）
}

// Secret 解析敏感信息：envKey 优先，其次 direct（支持 "env:VAR" 前缀引用）。
func Secret(direct, envKey string) string {
	if envKey != "" {
		return os.Getenv(envKey)
	}
	if rest, ok := strings.CutPrefix(direct, "env:"); ok {
		return os.Getenv(rest)
	}
	return direct
}

// ParseBool 严格解析布尔配置值：接受 bool、字符串 true/false/yes/no/on/off/1/0
// （大小写不敏感，兼容 YAML 1.1 惯用写法）与数值 0/1。
// 其它值返回错误——布尔配置静默当 false 是安全漏洞源
// （host_key_check: yes 会悄悄关闭指纹校验、no_log: yes 会泄露输出）。
func ParseBool(v any) (bool, error) {
	switch x := v.(type) {
	case bool:
		return x, nil
	case int:
		switch x {
		case 0:
			return false, nil
		case 1:
			return true, nil
		}
	case float64: // YAML 解析器可能产出浮点
		switch x {
		case 0:
			return false, nil
		case 1:
			return true, nil
		}
	case string:
		switch strings.ToLower(strings.TrimSpace(x)) {
		case "true", "yes", "on", "1":
			return true, nil
		case "false", "no", "off", "0":
			return false, nil
		}
	}
	return false, fmt.Errorf("无法解析为布尔值: %v", v)
}

// Clone 深拷贝主机（变量单独一份，供每主机独立变量域使用）。
func (h *Host) Clone() *Host {
	nh := *h
	if h.Vars != nil {
		nh.Vars = make(map[string]any, len(h.Vars))
		for k, v := range h.Vars {
			nh.Vars[k] = v
		}
	}
	return &nh
}

// Group 是主机组。
type Group struct {
	Name      string
	HostNames []string // 成员主机名（构建期使用，保持确定顺序）
	Hosts     []*Host
	Vars      map[string]any
	Children  []string // 子组名
}
