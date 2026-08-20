// Package localconn 在控制机本地执行的原语实现，
// 用于 conn: local 的主机（本机演练、CI 测试）。
package localconn

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"wdp/internal/connection"
	"wdp/internal/model"
)

func init() {
	connection.RegisterFactory("local", func(h *model.Host) (connection.Connection, error) {
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

// Exec 在本地以 sh 执行脚本。
func (l *Local) Exec(ctx context.Context, req connection.ExecRequest) (connection.ExecResult, error) {
	if req.TimeoutMs > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, msDur(req.TimeoutMs))
		defer cancel()
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
	return connection.ExecResult{Code: code, Stdout: stdout.String(), Stderr: stderr.String()}, nil
}

// UploadFile 写本地文件（临时文件 + 原子改名）。
func (l *Local) UploadFile(ctx context.Context, dst string, r io.Reader, mode fs.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".wdp-upload-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := io.Copy(tmp, r); err != nil {
		tmp.Close()
		return err
	}
	if mode == 0 {
		mode = 0o644
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return fmt.Errorf("落盘 %s 失败: %w", dst, err)
	}
	return nil
}

// DownloadFile 读本地文件。
func (l *Local) DownloadFile(ctx context.Context, src string, w io.Writer) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(w, f)
	return err
}

func envList(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}

func msDur(ms int64) time.Duration { return time.Duration(ms) * time.Millisecond }
