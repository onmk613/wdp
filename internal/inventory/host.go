package inventory

import (
	"fmt"

	"wdp/internal/config"
	"wdp/internal/model"
)

// hostKeys 是主机条目中的连接参数键，其余进入 Vars。
var hostKeys = map[string]bool{
	"host": true, "port": true, "user": true, "password": true, "password_env": true,
	"key_path": true, "key_passphrase": true, "key_passphrase_env": true,
	"conn": true, "agent_url": true, "agent_port": true,
	"host_key_check": true, "known_hosts": true, "connect_timeout": true,
	"token": true, "token_env": true, "ca_file": true, "cert_file": true, "key_file": true,
	"binary_path": true, "keep_agent": true,
	"become_password": true, "become_password_env": true,
	"tls": true, "insecure_skip_verify": true,
}

func buildHost(name string, vars map[string]any) (*model.Host, error) {
	// 连接默认值取 wdp.cfg 的 [ssh]（主机条目未显式指定的键生效）
	cfg := config.Current()
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
		if !hostKeys[k] {
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
		case "token":
			h.Token = fmt.Sprint(v)
		case "token_env":
			h.TokenEnv = fmt.Sprint(v)
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
