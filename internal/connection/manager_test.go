package connection

import (
	"context"
	"io"
	"io/fs"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"wdp/internal/model"
)

// slowConn 模拟握手耗时的连接，用于验证并发建连不被管理器锁退化成串行。
type slowConn struct {
	name string
}

func (c *slowConn) Connect(ctx context.Context) error {
	time.Sleep(120 * time.Millisecond)
	return nil
}
func (c *slowConn) Close() error     { return nil }
func (c *slowConn) Hostname() string { return c.name }
func (c *slowConn) Exec(context.Context, ExecRequest) (ExecResult, error) {
	return ExecResult{}, nil
}
func (c *slowConn) UploadFile(context.Context, string, io.Reader, fs.FileMode) error { return nil }
func (c *slowConn) DownloadFile(context.Context, string, io.Writer) error            { return nil }

// TestManagerConcurrentHandshake 并发建连必须真正并行：
// 8 台主机各 120ms 握手，串行耗时约 960ms，并行应远小于此。
func TestManagerConcurrentHandshake(t *testing.T) {
	var inFlight, maxInFlight atomic.Int64
	RegisterFactory("slowtest", func(h *model.Host, dc *Defaults) (Connection, error) {
		cur := inFlight.Add(1)
		for {
			m := maxInFlight.Load()
			if cur <= m || maxInFlight.CompareAndSwap(m, cur) {
				break
			}
		}
		time.Sleep(120 * time.Millisecond)
		inFlight.Add(-1)
		return &slowConn{name: h.Name}, nil
	})

	m := NewManager()
	m.SetConnectConcurrency(8)
	hosts := make([]*model.Host, 8)
	for i := range hosts {
		hosts[i] = &model.Host{Name: string(rune('a' + i)), Conn: "slowtest"}
	}

	start := time.Now()
	var wg sync.WaitGroup
	for _, h := range hosts {
		wg.Add(1)
		go func(h *model.Host) {
			defer wg.Done()
			if _, err := m.Get(context.Background(), h); err != nil {
				t.Errorf("Get(%s): %v", h.Name, err)
			}
		}(h)
	}
	wg.Wait()
	elapsed := time.Since(start)

	if elapsed > 600*time.Millisecond {
		t.Fatalf("8×120ms 握手耗时 %v，疑似被锁串行化（应约 120-200ms）", elapsed)
	}
	if got := maxInFlight.Load(); got < 2 {
		t.Fatalf("观测最大并发建连 = %d，握手未并行（应 ≥2）", got)
	}
}

// TestManagerSameHostDedup 并发请求同一主机只应保留一条连接，且返回同一实例。
func TestManagerSameHostDedup(t *testing.T) {
	RegisterFactory("deduptest", func(h *model.Host, dc *Defaults) (Connection, error) {
		return &slowConn{name: h.Name}, nil
	})

	m := NewManager()
	m.SetConnectConcurrency(4)
	h := &model.Host{Name: "same", Conn: "deduptest"}

	conns := make([]Connection, 16)
	var wg sync.WaitGroup
	for i := range conns {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c, err := m.Get(context.Background(), h)
			if err != nil {
				t.Errorf("Get: %v", err)
				return
			}
			conns[i] = c
		}(i)
	}
	wg.Wait()

	first := conns[0]
	for i, c := range conns {
		if c == nil || c != first {
			t.Fatalf("同一主机应复用同一连接: conns[%d]=%v first=%v", i, c, first)
		}
	}
}

// TestManagerClosedGetAfterClose 关闭后 Get 报错；关闭前已完成的连接被关闭。
func TestManagerClosedGetAfterClose(t *testing.T) {
	RegisterFactory("closedtest", func(h *model.Host, dc *Defaults) (Connection, error) {
		return &slowConn{name: h.Name}, nil
	})
	m := NewManager()
	if _, err := m.Get(context.Background(), &model.Host{Name: "h", Conn: "closedtest"}); err != nil {
		t.Fatal(err)
	}
	m.CloseAll()
	if _, err := m.Get(context.Background(), &model.Host{Name: "h", Conn: "closedtest"}); err == nil {
		t.Fatal("关闭后 Get 应报错")
	}
	m.CloseAll() // 幂等
}
