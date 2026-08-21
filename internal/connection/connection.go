// Package connection 定义与远程主机交互的传输抽象。
// 模块仅依赖此处的原语（Exec / UploadFile / DownloadFile），
// 因此同一模块可透明运行在 ssh / agent / local 等任意传输之上。
package connection

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
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
	TimeoutMs  int64             // 超时毫秒（connection.Timeout 换算），0 表示不限
	BecomeUser string            // 非空时以该用户执行（sudo -u）
}

// ExecResult 是脚本执行结果。
type ExecResult struct {
	Code   int
	Stdout string
	Stderr string
}

// Connection 是单主机传输连接。
type Connection interface {
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

// Factory 按主机构造连接。
type Factory func(h *model.Host) (Connection, error)

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

// NewConnection 按主机的 Conn 类型构造连接。
func NewConnection(h *model.Host) (Connection, error) {
	regMu.RLock()
	f, ok := factories[h.Conn]
	regMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("主机 %s: 未知的连接类型 %q（可选: ssh/agent/local）", h.Name, h.Conn)
	}
	return f(h)
}
