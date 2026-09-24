package module

// 提权分发回归：become 此前只作用于命令执行，对文件分发无效——非 root
// 登录 + become 时写 /etc 之类目录必然 permission denied，而 docs 明确
// 要求 owner/group 配合 become。现在优先走 conn.PrivilegedUploader。

import (
	"context"
	"io"
	"io/fs"
	"testing"

	"wdp/internal/conn"
	"wdp/internal/model"
)

// fakeConn 是内嵌的最小 Conn 实现（仅用于本测试的提权分支）。
type fakeConn struct{}

func (fakeConn) Connect(context.Context) error { return nil }
func (fakeConn) Close() error                  { return nil }
func (fakeConn) Hostname() string              { return "fake" }
func (fakeConn) Exec(context.Context, conn.ExecRequest) (conn.ExecResult, error) {
	return conn.ExecResult{}, nil
}
func (fakeConn) UploadFile(context.Context, string, io.Reader, fs.FileMode) error { return nil }
func (fakeConn) DownloadFile(context.Context, string, io.Writer) error            { return nil }

// privConn 同时实现 PrivilegedUploader。
type privConn struct {
	fakeConn
	uploadedAs string
	uploadedTo string
	plain      bool
}

func (p *privConn) UploadFileAs(_ context.Context, dst string, r io.Reader, _ fs.FileMode, user string) error {
	p.uploadedAs, p.uploadedTo = user, dst
	_, _ = io.Copy(io.Discard, r)
	return nil
}
func (p *privConn) UploadFile(context.Context, string, io.Reader, fs.FileMode) error {
	p.plain = true
	return nil
}

func TestUploadBytesUsesPrivilegedPathWhenBecome(t *testing.T) {
	pc := &privConn{}
	rc := &RunContext{Ctx: context.Background(), Conn: pc, Host: &model.Host{Name: "h"}, Become: true}
	if err := uploadBytes(rc, "/etc/app.conf", []byte("x"), 0o644, true); err != nil {
		t.Fatal(err)
	}
	if pc.plain {
		t.Fatal("become 时应走提权写路径")
	}
	if pc.uploadedAs != "root" {
		t.Fatalf("become 未指定用户应默认 root: %q", pc.uploadedAs)
	}
	if pc.uploadedTo != "/etc/app.conf" {
		t.Fatalf("目标路径错误: %q", pc.uploadedTo)
	}

	// 显式 become_user
	pc2 := &privConn{}
	rc2 := &RunContext{Ctx: context.Background(), Conn: pc2, Host: &model.Host{Name: "h"}, Become: true, BecomeUser: "app"}
	if err := uploadBytes(rc2, "/srv/app/x", []byte("x"), 0o644, true); err != nil {
		t.Fatal(err)
	}
	if pc2.uploadedAs != "app" {
		t.Fatalf("become_user 未透传: %q", pc2.uploadedAs)
	}
}

func TestUploadBytesFallsBackWithoutCapability(t *testing.T) {
	fc := fakeConn{}
	rc := &RunContext{Ctx: context.Background(), Conn: fc, Host: &model.Host{Name: "h"}, Become: true}
	// 连接不支持提权写：回退旧行为而不是报错（agent 通道本身以 root 运行）
	if err := uploadBytes(rc, "/etc/app.conf", []byte("x"), 0o644, true); err != nil {
		t.Fatal(err)
	}
	// 非 become：直接走普通上传
	pc := &privConn{}
	rc2 := &RunContext{Ctx: context.Background(), Conn: pc, Host: &model.Host{Name: "h"}}
	if err := uploadBytes(rc2, "/tmp/x", []byte("x"), 0o644, true); err != nil {
		t.Fatal(err)
	}
	if !pc.plain || pc.uploadedAs != "" {
		t.Fatal("非 become 不应走提权路径")
	}
}

// 编译期：privConn 必须同时满足 Conn 与 PrivilegedUploader。
var (
	_ conn.Conn               = (*privConn)(nil)
	_ conn.PrivilegedUploader = (*privConn)(nil)
)
