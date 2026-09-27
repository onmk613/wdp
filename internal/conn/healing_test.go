package conn

// healingConn 的回归：传输级错误作废底层连接、下次操作前自动重建；
// ctx 超时/取消不构成失效；可选能力（NativeExtractor/PrivilegedUploader）
// 经包装委托不丢失。此前连接池无失效机制，一次网络抖动后同 run 内该
// 主机的全部后续操作（含 auto_rollback）都死在缓存的死连接上。

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"net"
	"sync/atomic"
	"testing"

	"wdp/internal/model"
)

// flakyState 是跨连接实例共享的故障开关（重建后工厂造出新连接，开关仍指向同一状态）。
type flakyState struct {
	connects atomic.Int64
	mode     atomic.Int64 // 0=正常 1=传输级断连 2=ctx 超时
}

type flakyConn struct {
	st *flakyState
}

func (c *flakyConn) Connect(context.Context) error { c.st.connects.Add(1); return nil }
func (c *flakyConn) Close() error                  { return nil }
func (c *flakyConn) Hostname() string              { return "flaky" }
func (c *flakyConn) Exec(context.Context, ExecRequest) (ExecResult, error) {
	switch c.st.mode.Load() {
	case 1: // 模拟 TCP 连接被对端重置（net.Error 类 → 传输级失效）
		return ExecResult{}, &net.OpError{Op: "read", Net: "tcp", Err: errors.New("connection reset by peer")}
	case 2:
		return ExecResult{}, context.DeadlineExceeded
	}
	return ExecResult{Code: 0, Stdout: "ok"}, nil
}
func (c *flakyConn) UploadFile(context.Context, string, io.Reader, fs.FileMode) error { return nil }
func (c *flakyConn) DownloadFile(context.Context, string, io.Writer) error            { return nil }

func newFlakyManager(t *testing.T) (*Manager, *flakyState) {
	t.Helper()
	st := &flakyState{}
	RegisterFactory("healtest", func(h *model.Host, _ *Defaults) (Conn, error) {
		return &flakyConn{st: st}, nil
	})
	return NewManager(), st
}

// TestHealingReconnectsAfterTransportFailure 传输级错误 → 作废 → 下次
// 操作自动重建（建连次数 2），且失败的那次不自动重试。
func TestHealingReconnectsAfterTransportFailure(t *testing.T) {
	m, st := newFlakyManager(t)
	c, err := m.Get(context.Background(), &model.Host{Name: "h1", Conn: "healtest"})
	if err != nil {
		t.Fatal(err)
	}
	if out, err := c.Exec(context.Background(), ExecRequest{}); err != nil || out.Stdout != "ok" {
		t.Fatalf("首次执行应成功: %+v %v", out, err)
	}
	if n := st.connects.Load(); n != 1 {
		t.Fatalf("建连次数 = %d, want 1", n)
	}

	st.mode.Store(1)
	if _, err := c.Exec(context.Background(), ExecRequest{}); err == nil {
		t.Fatal("断连后执行应报错")
	}
	if n := st.connects.Load(); n != 1 {
		t.Fatalf("失败当场不应重建: %d", n)
	}

	st.mode.Store(0)
	out, err := c.Exec(context.Background(), ExecRequest{})
	if err != nil || out.Stdout != "ok" {
		t.Fatalf("下一次操作应自动重建并成功: %+v %v", out, err)
	}
	if n := st.connects.Load(); n != 2 {
		t.Fatalf("应发生一次重建（建连 2 次）: %d", n)
	}
}

// TestHealingKeepsConnOnCtxTimeout ctx 超时是调用方语义，不作废连接
// （下一次执行仍复用原连接，建连次数不变）。
func TestHealingKeepsConnOnCtxTimeout(t *testing.T) {
	m, st := newFlakyManager(t)
	c, err := m.Get(context.Background(), &model.Host{Name: "h2", Conn: "healtest"})
	if err != nil {
		t.Fatal(err)
	}
	st.mode.Store(2)
	if _, err := c.Exec(context.Background(), ExecRequest{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("应返回 ctx 超时: %v", err)
	}
	st.mode.Store(0)
	if out, err := c.Exec(context.Background(), ExecRequest{}); err != nil || out.Stdout != "ok" {
		t.Fatalf("ctx 超时后连接应仍可用: %+v %v", out, err)
	}
	if n := st.connects.Load(); n != 1 {
		t.Fatalf("ctx 超时不应触发重建: %d", n)
	}
}

// capableConn 实现全部可选能力，验证包装的委托不丢失。
type capableConn struct{ flakyConn }

func (c *capableConn) NativeExtract(context.Context, string, string) error { return nil }
func (c *capableConn) UploadFileAs(context.Context, string, io.Reader, fs.FileMode, string) error {
	return nil
}

// TestHealingDelegatesOptionalCapabilities 模块以类型断言探测连接能力，
// 包装漏实现一个能力就静默降级到 shell 路径。
func TestHealingDelegatesOptionalCapabilities(t *testing.T) {
	RegisterFactory("healcapable", func(h *model.Host, _ *Defaults) (Conn, error) {
		return &capableConn{flakyConn{st: &flakyState{}}}, nil
	})
	RegisterFactory("healplain", func(h *model.Host, _ *Defaults) (Conn, error) {
		return &flakyConn{st: &flakyState{}}, nil
	})
	m := NewManager()

	c, err := m.Get(context.Background(), &model.Host{Name: "h3", Conn: "healcapable"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := c.(NativeExtractor); !ok {
		t.Fatal("包装应委托 NativeExtractor")
	}
	if _, ok := c.(PrivilegedUploader); !ok {
		t.Fatal("包装应委托 PrivilegedUploader")
	}
	if err := c.(NativeExtractor).NativeExtract(context.Background(), "a", "b"); err != nil {
		t.Fatalf("NativeExtract 委托失败: %v", err)
	}

	p, err := m.Get(context.Background(), &model.Host{Name: "h4", Conn: "healplain"})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.(NativeExtractor).NativeExtract(context.Background(), "a", "b"); !errors.Is(err, ErrNativeUnsupported) {
		t.Fatalf("底层无能力应返回 ErrNativeUnsupported: %v", err)
	}
}
