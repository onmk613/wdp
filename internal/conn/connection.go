// Package connection 定义与远程主机交互的传输抽象。
// 模块仅依赖此处的原语（Exec / UploadFile / DownloadFile），
// 因此同一模块可透明运行在 ssh / agent / local 等任意传输之上。
package conn

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"wdp/internal/model"
)

// ErrNativeUnsupported 表示连接不支持该原生化操作（SSH 通道、旧版常驻
// agent、push 自举失败回退态等）。模块收到该哨兵应回退 shell 实现，
// 其余错误按真实失败上抛。
var ErrNativeUnsupported = errors.New("native op unsupported by this connection")

// NativeExtractor 是可选的连接能力：远端归档解压由 agent 侧 Go 原生完成，
// 不依赖目标机的 tar/unzip/xz 工具。agent/push 通道实现；模块以类型断言
// 探测，未实现或返回 ErrNativeUnsupported 时走 shell 路径。
type NativeExtractor interface {
	// NativeExtract 将远端归档 src 解压到 dest 目录（dest 不存在时自动创建）。
	NativeExtract(ctx context.Context, src, dest string) error
}

// Timeout 将时长转为毫秒值（<=0 表示不限）。
func Timeout(d time.Duration) int64 {
	if d <= 0 {
		return 0
	}
	return d.Milliseconds()
}

// ExecRequest 是一次远程脚本执行请求。
type ExecRequest struct {
	Script     string            // POSIX sh 脚本
	Stdin      string            // 附加到脚本 stdin 的数据（可选）
	Env        map[string]string // 环境变量
	TimeoutMs  int64             // 超时毫秒（conn.Timeout 换算），0 表示不限
	BecomeUser string            // 非空时以该用户执行（sudo -u）
}

// ExecResult 是脚本执行结果。
type ExecResult struct {
	Code   int
	Stdout string
	Stderr string
}

// Conn 是单主机传输连接。
type Conn interface {
	// Connect 建立连接（幂等，重复调用无副作用）。
	Connect(ctx context.Context) error
	Close() error
	Hostname() string

	// Exec 在远端以 /bin/sh 执行脚本，捕获 stdout / stderr / 退出码。
	Exec(ctx context.Context, req ExecRequest) (ExecResult, error)

	// UploadFile 将 r 的内容写入远端 dst（先写临时文件再原子改名）。
	UploadFile(ctx context.Context, dst string, r io.Reader, mode fs.FileMode) error

	// DownloadFile 将远端 src 的内容写入 w。
	DownloadFile(ctx context.Context, src string, w io.Writer) error
}

// Defaults 是连接层的显式注入默认值：由组合根（cli）从 wdp.cfg 归一后构造，
// 经 Manager 下发到各连接工厂。取代各连接包散落地读 config 全局单例——
// 连接层因此不依赖 config 包，测试可传 nil 或自定义值。
type Defaults struct {
	// SSH 用户/连接超时的归一化在 config 取值器完成（inventory 烘焙 host
	// 字段时也要用），组合根注入的是已归一化值，conn 层不再重复兜底。
	SSHUser             string            // 默认 SSH 用户（组合根注入 config.SSHUser() 归一化值）
	SSHConnectTimeout   int               // 连接超时秒（组合根注入 config.SSHConnectTimeout() 归一化值）
	AgentPort           int               // 默认 agent 端口（0 = 7602）
	AgentCertRotateMin  int               // push 临时证书轮换周期分钟（0 = 不轮换）
	AgentIdleTimeoutMin int               // push 临时 agent 空闲自动退出分钟（0 = 默认 60；<0 = 禁用）
	PushCADir           string            // push 会话 CA 落盘目录（空 = ~/.wdp/push-ca）
	PushBinary          map[string]string // push 自举按目标平台的二进制表（键 linux_amd64 等；控制端同平台时无需配置）
}

// AgentPortOrDefault 归一化默认 agent 端口。
func (d *Defaults) AgentPortOrDefault() int {
	if d != nil && d.AgentPort > 0 {
		return d.AgentPort
	}
	return 7602
}

// AgentCertRotateMinOrDefault 归一化 push 证书轮换周期（<=0 = 不轮换）。
func (d *Defaults) AgentCertRotateMinOrDefault() int {
	if d == nil {
		return 0
	}
	return d.AgentCertRotateMin
}

// PushCADirOrDefault 归一化 push 会话 CA 目录：空 = ~/.wdp/push-ca
// （会话 CA 落盘复用：1 天有效期内跨进程共享信任链，可经 wdp ca show 检视）。
func (d *Defaults) PushCADirOrDefault() string {
	if d != nil && d.PushCADir != "" {
		return d.PushCADir
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(os.TempDir(), "wdp-push-ca")
	}
	return filepath.Join(home, ".wdp", "push-ca")
}

// AgentIdleTimeoutMinOrDefault 归一化 push 临时 agent 空闲自动退出周期：
// 0/未配置 = 内置默认 60 分钟；<0 = 禁用（永不超时）；正值原样生效。
func (d *Defaults) AgentIdleTimeoutMinOrDefault() int {
	if d == nil || d.AgentIdleTimeoutMin == 0 {
		return 60
	}
	if d.AgentIdleTimeoutMin < 0 {
		return 0
	}
	return d.AgentIdleTimeoutMin
}

// Factory 按主机构造连接（dc 为组合根注入的默认值，可为 nil）。
type Factory func(h *model.Host, dc *Defaults) (Conn, error)

var (
	regMu     sync.RWMutex
	factories = map[string]Factory{}
)

// RegisterFactory 注册一种连接类型的工厂（由各实现包 init 调用）。
func RegisterFactory(connType string, f Factory) {
	regMu.Lock()
	defer regMu.Unlock()
	factories[connType] = f
}

// NewConnection 按主机的 Conn 类型构造连接（dc 可为 nil，取内置默认）。
func NewConnection(h *model.Host, dc *Defaults) (Conn, error) {
	regMu.RLock()
	f, ok := factories[h.Conn]
	regMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("host %s: unknown connection type %q (options: ssh/push/agent/local)", h.Name, h.Conn)
	}
	return f(h, dc)
}
