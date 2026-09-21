package inventory

import (
	"fmt"
	"strconv"
	"strings"
	"sync"

	"wdp/internal/config"
	"wdp/internal/model"
)

// hostKeys 是主机条目中连接参数键的白名单（其余键进入 Vars）。
// 内置基线为通用键；各连接包经 RegisterHostKeys 在 init 中注册自己的
// 专属键（与 conn.RegisterFactory 同一 blank-import 路径），新增连接
// 类型无需改动本包。
var (
	hostKeysMu sync.RWMutex
	hostKeys   = map[string]bool{
		"host": true,
		"conn": true,
		// SSH 引导通道（server 推装 agent / inventory 显式 conn: ssh）
		"port": true, "user": true, "password": true, "password_env": true,
		"key_path": true, "key_passphrase": true, "key_passphrase_env": true,
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

// hostKeyApplier 把单个主机条目键写进 Host——buildHost 的字段 switch
// 提取为可直测单元（不经过 isHostKey 白名单门），对账测试对每个文档键
// 直接 apply 哨兵值、反射断言字段被填充。
type hostKeyApplier struct {
	h *model.Host
}

// apply 写入一个键；未识别的键静默跳过（buildHost 只对 isHostKey 命中的
// 键调用）。
func (a *hostKeyApplier) apply(k string, v any) error {
	h := a.h
	switch k {
	case "host":
		h.Address = fmt.Sprint(v)
	case "conn":
		h.Conn = fmt.Sprint(v)
	case "port":
		// 严格解析 + 范围校验：部分解析（"80x"→80）与越界值连接期才暴露
		n, err := strictPort(v, 22, 1)
		if err != nil {
			return fmt.Errorf("port: %w", err)
		}
		h.Port = n
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
		h.ConnectTimeoutSec = toInt(v, 10)
	case "agent_url":
		h.AgentURL = fmt.Sprint(v)
	case "agent_port":
		n, err := strictPort(v, 0, 0)
		if err != nil {
			return fmt.Errorf("agent_port: %w", err)
		}
		h.AgentPort = n
	case "ca_file":
		h.CAFile = fmt.Sprint(v)
	case "cert_file":
		h.CertFile = fmt.Sprint(v)
	case "key_file":
		h.KeyFile = fmt.Sprint(v)
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

// buildHost 构建主机对象。连接参数三层合并（高→低）：
//  1. 主机条目键（vars）
//  2. 组级键（groupVars：all.vars < 组 vars 链，与变量域合并同序）
//  3. wdp.cfg 默认值 / 内置默认
//
// groupVars 只提取连接参数键；非键组变量由 applyVars 合入变量域。
func buildHost(name string, vars, groupVars map[string]any, cfg *config.Config) (*model.Host, error) {
	// 连接默认值取调用方显式传入的 wdp.cfg 配置（inventory 未显式指定的
	// 键生效；组合根传 config.Current()，测试与内联构造传 nil 即内置默认）
	if cfg == nil {
		cfg = &config.Config{}
	}
	h := &model.Host{
		Name: name,
		Vars: map[string]any{},
		Conn: cfg.DefaultConn(),
		// SSH 引导通道缺省（conn: ssh）：指纹校验默认开启，新主机先用
		// ssh-keyscan 采集指纹写入 known_hosts（本分支无 scan-ssh 命令）
		Port:              22,
		User:              "root",
		HostKeyCheck:      true,
		ConnectTimeoutSec: 10,
	}
	ap := &hostKeyApplier{h: h}
	// 组级连接键（低层）先应用，主机条目随后覆盖
	for k, v := range groupVars {
		if isHostKey(k) {
			if err := ap.apply(k, v); err != nil {
				return nil, err
			}
		}
	}
	for k, v := range vars {
		if !isHostKey(k) {
			h.Vars[k] = v
			continue
		}
		if err := ap.apply(k, v); err != nil {
			return nil, err
		}
	}
	if h.Address == "" {
		h.Address = name
	}
	return h, nil
}

// strictPort 严格解析整数端口：类型不符/部分解析（"80x"）显式报错，
// 范围校验 0..65535（agent_port 0=未设置）。
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
