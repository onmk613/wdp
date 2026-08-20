package connection

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"strings"
	"sync"

	"wdp/internal/model"
)

// Fake 是内存实现的假连接，供单元测试使用：
// 文件操作落在内存 map，Exec 交给 ExecFn 回调（缺省返回成功空输出）。
type Fake struct {
	Host *model.Host

	mu    sync.Mutex
	Files map[string][]byte
	Modes map[string]fs.FileMode

	// ExecFn 按需注入脚本行为；nil 时返回 rc=0 空输出。
	ExecFn func(req ExecRequest) (ExecResult, error)

	// ExecLog 记录每次 Exec 请求（按序追加）。
	ExecLog []ExecRequest

	// ConnectErr / UploadErr 注入失败。
	ConnectErr error
	UploadErr  error
}

// NewFake 构造假连接。
func NewFake(host *model.Host) *Fake {
	return &Fake{
		Host:  host,
		Files: map[string][]byte{},
		Modes: map[string]fs.FileMode{},
	}
}

// New 构造假连接（实现 Factory 签名）。
func (f *Fake) New(h *model.Host) (Connection, error) { return f, nil }

// Connect 记录连接，可注入失败。
func (f *Fake) Connect(context.Context) error { return f.ConnectErr }

// Close 释放资源（无）。
func (f *Fake) Close() error { return nil }

// Hostname 返回主机名。
func (f *Fake) Hostname() string { return f.Host.Name }

// Exec 执行脚本（由 ExecFn 决定行为；尊重 ctx 取消以贴近真实连接）。
func (f *Fake) Exec(ctx context.Context, req ExecRequest) (ExecResult, error) {
	f.mu.Lock()
	f.ExecLog = append(f.ExecLog, req)
	f.mu.Unlock()
	if f.ExecFn == nil {
		return ExecResult{}, nil
	}
	type outcome struct {
		res ExecResult
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := f.ExecFn(req)
		done <- outcome{res, err}
	}()
	select {
	case o := <-done:
		return o.res, o.err
	case <-ctx.Done():
		return ExecResult{}, ctx.Err()
	}
}

// UploadFile 写入内存文件表。
func (f *Fake) UploadFile(ctx context.Context, dst string, r io.Reader, mode fs.FileMode) error {
	if f.UploadErr != nil {
		return f.UploadErr
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Files[dst] = data
	if mode == 0 {
		mode = 0o644
	}
	f.Modes[dst] = mode
	return nil
}

// DownloadFile 从内存文件表读取。
func (f *Fake) DownloadFile(ctx context.Context, src string, w io.Writer) error {
	f.mu.Lock()
	data, ok := f.Files[src]
	f.mu.Unlock()
	if !ok {
		return fmt.Errorf("no such file: %s", src)
	}
	_, err := io.Copy(w, strings.NewReader(string(data)))
	return err
}

// File 返回内存文件内容（测试断言用）。
func (f *Fake) File(path string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.Files[path]
	return string(b), ok
}
