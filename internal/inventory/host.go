package inventory

import (
	"fmt"
	"sync"

	"strconv"
	"strings"
	"wdp/internal/config"
	"wdp/internal/model"
	"wdp/internal/sshcfg"
)

// hostKeys 是主机条目中连接参数键的白名单（其余键进入 Vars）。
// 内置基线为 SSH 通道与通用键；各连接包经 RegisterHostKeys 在 init 中
// 注册自己的专属键（与 conn.RegisterFactory 同一 blank-import 路径），
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

// buildHost 构建主机对象。连接参数三层合并（高→低）：
//  1. 主机条目键（vars）
//  2. 组级键（groupVars：all.vars < 组 vars 链，与变量域合并同序）
//  3. wdp.cfg [ssh] 默认值 / 内置默认；仍未给出的 user/port/身份文件
//     由 ~/.ssh/config 补全（见 FillFromSSHConfig）
//
// 组级与条目同属 inventory 显式配置，均优先于 ~/.ssh/config。
// groupVars 只提取连接参数键；非键组变量由 applyVars 合入变量域。
func buildHost(name string, vars, groupVars map[string]any, cfg *config.Config) (*model.Host, error) {
	// 连接默认值取调用方显式传入的 wdp.cfg [ssh] 配置（inventory 未显式指定的键生效；
	// 组合根传 config.Current()，测试与内联构造传 nil 即内置默认）
	if cfg == nil {
		cfg = &config.Config{}
	}
	h := &model.Host{
		Name:              name,
		Vars:              map[string]any{},
		Conn:              cfg.DefaultConn(),
		Port:              22,
		User:              cfg.SSHUser(),
		HostKeyCheck:      cfg.SSHHostKeyCheck(),
		KnownHosts:        cfg.SSH.KnownHosts,
		ConnectTimeoutSec: cfg.SSHConnectTimeout(),
	}
	// 连接参数是否由 inventory 显式给出（组级或条目均算；决定 ~/.ssh/config
	// 能否补全：显式键 > ssh config > wdp.cfg/内置默认，对齐 OpenSSH 优先级）
	explicitUser, explicitPort, explicitKeyPath := false, false, false
	applyKeys := func(k string, v any) error {
		switch k {
		case "host":
			h.Address = fmt.Sprint(v)
		case "port":
			// 严格解析 + 范围校验：toInt 的 Sscanf 部分解析（"80x"→80）与
			// 越界值（70000/0）此前静默接受/回退，连接期才暴露
			n, err := strictPort(v, 22, 1)
			if err != nil {
				return fmt.Errorf("port: %w", err)
			}
			h.Port = n
			explicitPort = true
		case "user":
			h.User = fmt.Sprint(v)
			explicitUser = true
		case "password":
			h.Password = fmt.Sprint(v)
		case "password_env":
			h.PasswordEnv = fmt.Sprint(v)
		case "key_path":
			h.KeyPath = fmt.Sprint(v)
			explicitKeyPath = true
		case "key_passphrase":
			h.KeyPassphrase = fmt.Sprint(v)
		case "key_passphrase_env":
			h.KeyPassphraseEnv = fmt.Sprint(v)
		case "conn":
			h.Conn = fmt.Sprint(v)
		case "agent_url":
			h.AgentURL = fmt.Sprint(v)
		case "agent_port":
			n, err := strictPort(v, 0, 0)
			if err != nil {
				return fmt.Errorf("agent_port: %w", err)
			}
			h.AgentPort = n
		case "host_key_check":
			// 严格解析：非布尔值直接报错（静默当 false 会关闭指纹校验）
			b, err := model.ParseBool(v)
			if err != nil {
				return fmt.Errorf("host_key_check: %w", err)
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
				return fmt.Errorf("keep_agent: %w", err)
			}
			h.KeepAgent = b
		case "become_password":
			h.BecomePassword = fmt.Sprint(v)
		case "become_password_env":
			h.BecomePasswordEnv = fmt.Sprint(v)
		case "tls":
			b, err := model.ParseBool(v)
			if err != nil {
				return fmt.Errorf("tls: %w", err)
			}
			h.TLS = b
		case "insecure_skip_verify":
			b, err := model.ParseBool(v)
			if err != nil {
				return fmt.Errorf("insecure_skip_verify: %w", err)
			}
			h.InsecureSkipVerify = b
		case "tls_skip_host_verify":
			b, err := model.ParseBool(v)
			if err != nil {
				return fmt.Errorf("tls_skip_host_verify: %w", err)
			}
			h.TLSSkipHostVerify = b
		case "tls_server_name":
			h.TLSServerName = fmt.Sprint(v)
		}
		return nil
	}
	// 组级连接键（低层）先应用，主机条目随后覆盖
	for k, v := range groupVars {
		if isHostKey(k) {
			if err := applyKeys(k, v); err != nil {
				return nil, err
			}
		}
	}
	for k, v := range vars {
		if !isHostKey(k) {
			h.Vars[k] = v
			continue
		}
		if err := applyKeys(k, v); err != nil {
			return nil, err
		}
	}
	if h.Address == "" {
		h.Address = name
	}
	// ~/.ssh/config 补全未显式给出的 SSH 参数（仅 ssh/push 通道；显式键
	// 优先），使交互 ssh 可达的主机 wdp 同样可达
	sshcfg.FillFromSSHConfig(h, explicitUser, explicitPort, explicitKeyPath)
	return h, nil
}

// strictPort 严格解析整数端口：类型不符/部分解析（"80x"）显式报错，
// 范围校验 0..65535（min 给出下界：SSH port >=1，agent_port 0=未设置）。
func strictPort(v any, def, min int) (int, error) {
	var n int
	switch x := v.(type) {
	case int:
		n = x
	case float64:
		if x != float64(int(x)) {
			return 0, fmt.Errorf("expected an integer, got %v", x)
		}
		n = int(x)
	case string:
		parsed, err := strconv.Atoi(strings.TrimSpace(x))
		if err != nil {
			return 0, fmt.Errorf("expected an integer, got %q", x)
		}
		n = parsed
	default:
		return 0, fmt.Errorf("expected an integer, got %T", v)
	}
	if n == 0 && def != 0 {
		return def, nil // 未设置语义交给调用方默认值
	}
	if n < min || n > 65535 {
		return 0, fmt.Errorf("%d is out of range %d..65535", n, min)
	}
	return n, nil
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
