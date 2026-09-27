package config

import (
	"fmt"
	"os"
	"strings"

	"github.com/BurntSushi/toml"
)

// DefaultPath 是默认配置文件路径
const DefaultPath = "wdp.cfg"

// current 是包级配置存储器（可变全局）。仅启动期 Load 写入、运行期只读
// （Current 被各处并发调用）；无锁保护——启动后再并发 Load 即 data race。
var current = Config{}

// Current 返回当前生效的配置
func Current() *Config { return &current }

// Reset 恢复内置默认配置
func Reset() { current = Config{} }

type Config struct {
	Inventory InventoryConfig
	Run       RunConfig
	Output    OutputConfig
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
	Conn        string `toml:"conn"`         // 默认连接类型（空 = agent，见 DefaultConn；可选 agent/local，fleet 级默认）
}

// OutputConfig 是输出相关默认值。
type OutputConfig struct {
	Color *bool `toml:"color"` // 颜色输出（nil = 默认 true）
}

// AgentConfig 是 agent 连接默认值。
type AgentConfig struct {
	Port int `toml:"port"` // 默认 agent 端口（0 = 7602）
}

// TransferConfig 是文件传输相关上限。
type TransferConfig struct {
	MaxDownloadMB int `toml:"max_download_mb"` // get_url 下载响应体上限 MiB（0 = 默认 2048）
	MaxExtractMB  int `toml:"max_extract_mb"`  // chart tgz 解包总量上限 MiB（0 = 默认 2048）
	MaxUploadMB   int `toml:"max_upload_mb"`   // copy/unarchive 本地 src 读取上限 MiB（0 = 默认 2048；防误配大文件把控制端内存打爆）
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
	md, err := toml.DecodeFile(path, &cfg)
	if err != nil {
		return fmt.Errorf("failed to parse config %s: %w", path, err)
	}
	// 未知键（多为拼写错误：forks 写成 fork）此前被静默忽略，配置会静默
	// 失效回退默认值——列出键名告警，让拼写错误可见而不是默默吞掉
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, 0, len(undecoded))
		for _, k := range undecoded {
			keys = append(keys, k.String())
		}
		fmt.Fprintf(os.Stderr, "warning: %s: unknown config key(s) ignored: %s\n", path, strings.Join(keys, ", "))
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

// DefaultConn 归一化默认连接类型（[run].conn，空 = agent）。未知值原样返回，
// 由连接工厂报 unknown connection type（错误信息含合法选项）。
func (c *Config) DefaultConn() string {
	if c.Run.Conn != "" {
		return c.Run.Conn
	}
	return "agent"
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

// MaxExtractBytes 归一化 chart tgz 解包总量上限（字节；0 = chart 包内置
// 默认 2GiB——上限的内置默认只属于执行方 chart 包，config 只透传文件值）。
func (c *Config) MaxExtractBytes() int64 {
	if c.Transfer.MaxExtractMB > 0 {
		return int64(c.Transfer.MaxExtractMB) << 20
	}
	return 0
}
