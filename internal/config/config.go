// Package config 提供 wdp.cfg（TOML）全局默认配置。
//
// 优先级：CLI flag 显式值 > wdp.cfg > 内置默认值。
// 查找顺序：--config 指定路径（不存在则报错）> 当前目录 wdp.cfg（不存在则静默跳过）。
//
// 示例 wdp.cfg：
//
//	[inventory]
//	path = "inventory.yaml"
//
//	[run]
//	forks = 20
//	task_timeout = 300
//
//	[ssh]
//	user = "root"
//	connect_timeout = 10
//	host_key_check = true
//
//	[agent]
//	port = 7602
package config

import (
	"fmt"
	"os"

	"github.com/BurntSushi/toml"
)

// Config 是全局配置（零值安全：零值即内置默认行为）。
type Config struct {
	Inventory InventoryConfig
	Run       RunConfig
	Output    OutputConfig
	SSH       SSHConfig
	Agent     AgentConfig
}

// InventoryConfig 是 inventory 相关默认值。
type InventoryConfig struct {
	Path string `toml:"path"` // 默认 inventory 文件路径
}

// RunConfig 是执行相关默认值。
type RunConfig struct {
	Forks       int  `toml:"forks"`        // 并发主机数（0 = 默认 5）
	Timeout     int  `toml:"timeout"`      // 全局墙钟超时秒（0 = 不限）
	TaskTimeout int  `toml:"task_timeout"` // 任务默认超时秒（0 = 不限）
	Verbose     bool `toml:"verbose"`      // 逐主机全量输出
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
	Port int `toml:"port"` // 默认 agent 端口（0 = 7602）
}

// DefaultPath 是默认配置文件路径（当前目录）。
const DefaultPath = "wdp.cfg"

// current 是已加载的配置（零值 = 未加载/全部内置默认）。
var current = Config{}

// Current 返回当前生效的配置。
func Current() *Config { return &current }

// Load 加载配置文件。path 不存在且 required=false 时静默返回（保持内置默认）。
func Load(path string, required bool) error {
	current = Config{} // 先归零：文件缺失/为空时确定为内置默认，不残留旧状态
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) && !required {
			return nil
		}
		if os.IsNotExist(err) {
			return fmt.Errorf("配置文件不存在: %s", path)
		}
		return err
	}
	cfg := Config{}
	if _, err := toml.DecodeFile(path, &cfg); err != nil {
		return fmt.Errorf("解析配置 %s 失败: %w", path, err)
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

// SSHConnectTimeout 归一化连接超时秒。
func (c *Config) SSHConnectTimeout() int {
	if c.SSH.ConnectTimeout > 0 {
		return c.SSH.ConnectTimeout
	}
	return 10
}

// SSHHostKeyCheck 归一化指纹校验（默认开启：安全默认；用 wdp key scan 采集指纹）。
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
