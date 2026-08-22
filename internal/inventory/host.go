package inventory

import (
	"fmt"
	"sync"

	"wdp/internal/config"
	"wdp/internal/model"
)

// hostKeys 是主机条目中连接参数键的白名单（其余键进入 Vars）。
// 内置基线为 SSH 通道与通用键；各连接包经 RegisterHostKeys 在 init 中
// 注册自己的专属键（与 connection.RegisterFactory 同一 blank-import 路径），
// 新增连接类型无需改动本包。
var (
	hostKeysMu sync.RWMutex
	hostKeys   = map[string]bool{
		"host": true, "port": true, "user": true, "password": true, "password_env": true,
		"key_path": true, "key_passphrase": true, "key_passphrase_env": true,
		"conn":           true,
		"host_key_check": true, "known_hosts": true, "connect_timeout": true,
		"become_password": true, "become_password_env": true,
	}
)

// RegisterHostKeys 注册连接类型的专属主机条目键（由连接实现包 init 调用）。
func RegisterHostKeys(keys ...string) {
	hostKeysMu.Lock()
	defer hostKeysMu.Unlock()
	for _, k := range keys {
		hostKeys[k] = true
	}
}

// isHostKey 判断键是否为连接参数键。
func isHostKey(k string) bool {
	hostKeysMu.RLock()
	defer hostKeysMu.RUnlock()
	return hostKeys[k]
}

func buildHost(name string, vars map[string]any, cfg *config.Config) (*model.Host, error) {
	// 连接默认值取调用方显式传入的 wdp.cfg [ssh] 配置（主机条目未显式指定的键生效；
	// 组合根传 config.Current()，测试与内联构造传 nil 即内置默认）
	if cfg == nil {
		cfg = &config.Config{}
	}
	h := &model.Host{
		Name:              name,
		Vars:              map[string]any{},
		Conn:              "ssh",
		Port:              22,
		User:              cfg.SSHUser(),
		HostKeyCheck:      cfg.SSHHostKeyCheck(),
		KnownHosts:        cfg.SSH.KnownHosts,
		ConnectTimeoutSec: cfg.SSHConnectTimeout(),
	}
	for k, v := range vars {
		if !isHostKey(k) {
			h.Vars[k] = v
			continue
		}
		switch k {
		case "host":
			h.Address = fmt.Sprint(v)
		case "port":
			h.Port = toInt(v, 22)
		case "user":
			h.User = fmt.Sprint(v)
		case "password":
			h.Password = fmt.Sprint(v)
		case "password_env":
			h.PasswordEnv = fmt.Sprint(v)
		case "key_path":
			h.KeyPath = fmt.Sprint(v)
		case "key_passphrase":
			h.KeyPassphrase = fmt.Sprint(v)
		case "key_passphrase_env":
			h.KeyPassphraseEnv = fmt.Sprint(v)
		case "conn":
			h.Conn = fmt.Sprint(v)
		case "agent_url":
			h.AgentURL = fmt.Sprint(v)
		case "agent_port":
			h.AgentPort = toInt(v, 0)
		case "host_key_check":
			// 严格解析：非布尔值直接报错（静默当 false 会关闭指纹校验）
			b, err := model.ParseBool(v)
			if err != nil {
				return nil, fmt.Errorf("host_key_check: %w", err)
			}
			h.HostKeyCheck = b
		case "known_hosts":
			h.KnownHosts = fmt.Sprint(v)
		case "connect_timeout":
			h.ConnectTimeoutSec = toInt(v, cfg.SSHConnectTimeout())
		case "ca_file":
			h.CAFile = fmt.Sprint(v)
		case "cert_file":
			h.CertFile = fmt.Sprint(v)
		case "key_file":
			h.KeyFile = fmt.Sprint(v)
		case "binary_path":
			h.BinaryPath = fmt.Sprint(v)
		case "keep_agent":
			b, err := model.ParseBool(v)
			if err != nil {
				return nil, fmt.Errorf("keep_agent: %w", err)
			}
			h.KeepAgent = b
		case "become_password":
			h.BecomePassword = fmt.Sprint(v)
		case "become_password_env":
			h.BecomePasswordEnv = fmt.Sprint(v)
		case "tls":
			b, err := model.ParseBool(v)
			if err != nil {
				return nil, fmt.Errorf("tls: %w", err)
			}
			h.TLS = b
		case "insecure_skip_verify":
			b, err := model.ParseBool(v)
			if err != nil {
				return nil, fmt.Errorf("insecure_skip_verify: %w", err)
			}
			h.InsecureSkipVerify = b
		case "tls_skip_host_verify":
			b, err := model.ParseBool(v)
			if err != nil {
				return nil, fmt.Errorf("tls_skip_host_verify: %w", err)
			}
			h.TLSSkipHostVerify = b
		case "tls_server_name":
			h.TLSServerName = fmt.Sprint(v)
		}
	}
	if h.Address == "" {
		h.Address = name
	}
	return h, nil
}

func toInt(v any, def int) int {
	switch x := v.(type) {
	case int:
		return x
	case float64:
		return int(x)
	case string:
		var n int
		if _, err := fmt.Sscanf(x, "%d", &n); err == nil {
			return n
		}
	}
	return def
}
