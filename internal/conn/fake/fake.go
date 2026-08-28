// Package fake 提供 Connection 接口的内存假实现，专供单元测试：
// 文件操作落在内存表，Exec 行为由 ExecFn 回调注入并全量记录到 ExecLog，
// 使测试能精确断言"生成了哪些命令、下发了什么内容"。
//
// 它不是任何 conn: 连接类型，也永远不应被生产代码引用——
// 需要真实本机执行时用 local，需要验证远端行为时用 ssh/agent 连接。
// 正式二进制不链接本包（仅 _test.go 文件导入）。
package fake

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"strings"
	"sync"

	"wdp/internal/conn"
	"wdp/internal/model"
)

// Fake 是内存实现的假连接。
type Fake struct {
	Host *model.Host

	mu    sync.Mutex
	Files map[string][]byte
	Modes map[string]fs.FileMode

	// ExecFn 按需注入脚本行为；nil 时返回 rc=0 空输出。
	ExecFn func(req conn.ExecRequest) (conn.ExecResult, error)

	// ExecLog 记录每次 Exec 请求（按序追加）。
	ExecLog []conn.ExecRequest

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
func (f *Fake) New(_ *model.Host) (conn.Conn, error) { return f, nil }

// Connect 记录连接，可注入失败。
func (f *Fake) Connect(context.Context) error { return f.ConnectErr }

// Close 释放资源（无）。
func (f *Fake) Close() error { return nil }

// Hostname 返回主机名。
func (f *Fake) Hostname() string { return f.Host.Name }

// Exec 执行脚本（由 ExecFn 决定行为；尊重 ctx 取消以贴近真实连接）。
func (f *Fake) Exec(ctx context.Context, req conn.ExecRequest) (conn.ExecResult, error) {
	f.mu.Lock()
	f.ExecLog = append(f.ExecLog, req)
	f.mu.Unlock()
	if f.ExecFn == nil {
		return conn.ExecResult{}, nil
	}
	type outcome struct {
		res conn.ExecResult
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
		return conn.ExecResult{}, ctx.Err()
	}
}

// UploadFile 写入内存文件表。
func (f *Fake) UploadFile(_ context.Context, dst string, r io.Reader, mode fs.FileMode) error {
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
func (f *Fake) DownloadFile(_ context.Context, src string, w io.Writer) error {
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
