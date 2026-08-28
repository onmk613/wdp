package config

import (
	"fmt"
	"os"

	"github.com/BurntSushi/toml"
)

// DefaultPath 是默认配置文件路径
const DefaultPath = "wdp.cfg"

// current 声明一个配置存储器
var current = Config{}

// Current 返回当前生效的配置
func Current() *Config { return &current }

// Reset 恢复内置默认配置
func Reset() { current = Config{} }

type Config struct {
	Inventory InventoryConfig
	Run       RunConfig
	Output    OutputConfig
	SSH       SSHConfig
	Agent     AgentConfig
	Transfer  TransferConfig
}

// InventoryConfig 是 inventory 相关默认值。
type InventoryConfig struct {
	Path string `toml:"path"`
}

// RunConfig 是执行相关默认值。
type RunConfig struct {
	Forks       int    `toml:"forks"`        // 并发主机数（0 = 默认 5）
	Timeout     int    `toml:"timeout"`      // 全局墙钟超时秒（0 = 不限）
	TaskTimeout int    `toml:"task_timeout"` // 任务默认超时秒（0 = 不限）
	Verbose     bool   `toml:"verbose"`      // 逐主机全量输出
	Conn        string `toml:"conn"`         // 默认连接类型（空 = ssh；可选 push/agent/local，fleet 级默认）
}

// OutputConfig 是输出相关默认值。
type OutputConfig struct {
	Color *bool `toml:"color"` // 颜色输出（nil = 默认 true）
}

// SSHConfig 是 SSH/push 连接默认值（inventory 未显式指定时生效）。
type SSHConfig struct {
	User           string `toml:"user"`            // 默认 SSH 用户（空 = root）
	ConnectTimeout int    `toml:"connect_timeout"` // 连接超时秒（0 = 默认 10）
	HostKeyCheck   *bool  `toml:"host_key_check"`  // 主机指纹校验（nil = 默认 true；关闭需显式 false）
	KnownHosts     string `toml:"known_hosts"`     // known_hosts 路径（空 = ~/.ssh/known_hosts）
}

// AgentConfig 是 agent 连接默认值。
type AgentConfig struct {
	Port           int               `toml:"port"`             // 默认 agent 端口（0 = 7602）
	CertRotateMin  int               `toml:"cert_rotate_min"`  // push 临时证书轮换周期分钟（0 = 不轮换）
	PushCADir      string            `toml:"push_ca_dir"`      // push 会话 CA 落盘目录（空 = ~/.wdp/push-ca）
	IdleTimeoutMin int               `toml:"idle_timeout_min"` // push 临时 agent 空闲自动退出分钟（0 = 默认 60；<0 = 禁用）
	PushBinary     map[string]string `toml:"push_binary"`      // push 自举按目标平台的二进制表（键 linux_amd64/linux_arm64/…，值为本机预编译产物路径）
}

// TransferConfig 是文件传输相关上限。
type TransferConfig struct {
	MaxDownloadMB int `toml:"max_download_mb"` // get_url 下载响应体上限 MiB（0 = 默认 2048）
	MaxExtractMB  int `toml:"max_extract_mb"`  // chart tgz 解包总量上限 MiB（0 = 默认 2048）
}

// Load 加载配置文件。path 不存在且 required=false 时静默返回（保持内置默认）。
func Load(path string, required bool) error {
	current = Config{} // 先归零：文件缺失/为空时确定为内置默认，不残留旧状态
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) && !required {
			return nil
		}
		if os.IsNotExist(err) {
			return fmt.Errorf("config file not found: %s", path)
		}
		return err
	}
	cfg := Config{}
	if _, err := toml.DecodeFile(path, &cfg); err != nil {
		return fmt.Errorf("failed to parse config %s: %w", path, err)
	}
	current = cfg
	return nil
}

// ---- 归一化取值（零值回退内置默认） ----

// Forks 归一化并发数。
func (c *Config) Forks() int {
	if c.Run.Forks > 0 {
		return c.Run.Forks
	}
	return 5
}

// SSHUser 归一化默认 SSH 用户。
func (c *Config) SSHUser() string {
	if c.SSH.User != "" {
		return c.SSH.User
	}
	return "root"
}

// DefaultConn 归一化默认连接类型（[run].conn，空 = ssh）。未知值原样返回，
// 由连接工厂报 unknown connection type（错误信息含合法选项）。
func (c *Config) DefaultConn() string {
	if c.Run.Conn != "" {
		return c.Run.Conn
	}
	return "ssh"
}

// SSHConnectTimeout 归一化连接超时秒。
func (c *Config) SSHConnectTimeout() int {
	if c.SSH.ConnectTimeout > 0 {
		return c.SSH.ConnectTimeout
	}
	return 10
}

// SSHHostKeyCheck 归一化指纹校验（默认开启：安全默认；新主机首次连接前先采集指纹写入 known_hosts）。
func (c *Config) SSHHostKeyCheck() bool {
	if c.SSH.HostKeyCheck != nil {
		return *c.SSH.HostKeyCheck
	}
	return true
}

// AgentPort 归一化默认 agent 端口。
func (c *Config) AgentPort() int {
	if c.Agent.Port > 0 {
		return c.Agent.Port
	}
	return 7602
}

// AgentCertRotateMin 归一化 push 临时证书轮换周期（分钟，<=0 = 不轮换）。
// 轮换压缩共享证书对的暴露窗口；仅对仍有后续任务的主机生效，
// 已完成自销毁的主机不受影响。
func (c *Config) AgentCertRotateMin() int {
	if c.Agent.CertRotateMin > 0 {
		return c.Agent.CertRotateMin
	}
	return 0
}

// AgentIdleTimeoutMin 归一化 push 临时 agent 空闲自动退出周期（分钟）。
// 原样透传给连接层归一化（0 = 内置默认 60；<0 = 禁用）——控制端崩溃/
// 断网时 Close 不被调用，远端 agent 依赖该周期兜底自清理。
func (c *Config) AgentIdleTimeoutMin() int {
	return c.Agent.IdleTimeoutMin
}

// InventoryPath 归一化默认 inventory 路径。
func (c *Config) InventoryPath() string {
	if c.Inventory.Path != "" {
		return c.Inventory.Path
	}
	return "inventory.yaml"
}

// Color 归一化颜色输出。
func (c *Config) Color() bool {
	if c.Output.Color != nil {
		return *c.Output.Color
	}
	return true
}

// MaxExtractBytes 归一化 chart tgz 解包总量上限（字节；0 = chart 包内置
// 默认 2GiB——上限的内置默认只属于执行方 chart 包，config 只透传文件值）。
func (c *Config) MaxExtractBytes() int64 {
	if c.Transfer.MaxExtractMB > 0 {
		return int64(c.Transfer.MaxExtractMB) << 20
	}
	return 0
}
