// Package local 在控制机本地执行的原语实现，
// 用于 conn: local 的主机（本机演练、CI 测试）。
package local

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"sync"
	"time"

	"wdp/internal/conn"
	"wdp/internal/model"
)

func init() {
	conn.RegisterFactory("local", func(h *model.Host, dc *conn.Defaults) (conn.Conn, error) {
		return &Local{host: h.Name}, nil
	})
}

// Local 是本地连接。
type Local struct {
	host string
}

// Connect 本地连接无需建连。
func (l *Local) Connect(context.Context) error { return nil }

// Close 本地连接无资源可释放。
func (l *Local) Close() error { return nil }

// Hostname 返回主机名。
func (l *Local) Hostname() string { return l.host }

// warnBecomeIgnored local 通道忽略 become 的一次性告警（每进程一条，
// 避免多任务重复刷屏）。静默以非预期用户执行比告警更危险。
var warnBecomeIgnored = func() func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			fmt.Fprintln(os.Stderr, "[warn] local connection ignores become; tasks run as the current user")
		})
	}
}()

// Exec 在本地以 sh 执行脚本。
func (l *Local) Exec(ctx context.Context, req conn.ExecRequest) (conn.ExecResult, error) {
	if req.TimeoutMs > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, msDur(req.TimeoutMs))
		defer cancel()
	}
	if req.BecomeUser != "" {
		warnBecomeIgnored()
	}
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", req.Script)
	cmd.Dir = "/"
	cmd.Env = append(os.Environ(), envList(req.Env)...)
	if req.Stdin != "" {
		cmd.Stdin = bytes.NewReader([]byte(req.Stdin))
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		code = 1
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		}
	}
	return conn.ExecResult{Code: code, Stdout: stdout.String(), Stderr: stderr.String()}, nil
}

// UploadFile 写本地文件（conn.WriteLocalFile 共享实现：建父目录 +
// fsatomic 原子落盘，与 selfexec 同口径）。
func (l *Local) UploadFile(_ context.Context, dst string, r io.Reader, mode fs.FileMode) error {
	return conn.WriteLocalFile(dst, r, mode)
}

// DownloadFile 读本地文件（conn.ReadLocalFile 共享实现）。
func (l *Local) DownloadFile(_ context.Context, src string, w io.Writer) error {
	return conn.ReadLocalFile(src, w)
}

func envList(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k, v := range env {
		if !conn.EnvKeyAllowed(k) {
			continue // 越白名单的键静默丢弃（白名单在 conn.EnvKeyAllowed 单一实现）
		}
		out = append(out, k+"="+v)
	}
	return out
}

func msDur(ms int64) time.Duration { return time.Duration(ms) * time.Millisecond }
