package conn

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"wdp/internal/model"
)

// Manager 按主机懒建立并复用连接，play 结束时统一关闭。
// 建连并发由信号量限制，防止大规模主机瞬时握手洪峰。
// Get 返回的连接带自愈语义（healing.go）：失效自动作废、下次操作前重建。
type Manager struct {
	mu     sync.Mutex
	conns  map[string]Conn
	closed bool

	connectSem chan struct{} // 并发建连上限（nil 不限）
	defaults   *Defaults     // 组合根注入的连接默认值（nil = 内置默认）
}

// NewManager 创建连接管理器（使用注册表工厂，见 NewConnection）。
func NewManager() *Manager {
	return &Manager{conns: map[string]Conn{}}
}

// NewManagerWithDefaults 创建携带显式默认值的管理器（组合根使用）。
func NewManagerWithDefaults(dc *Defaults) *Manager {
	return &Manager{conns: map[string]Conn{}, defaults: dc}
}

// SetConnectConcurrency 设置并发建连上限（应在首次 Get 前调用；已设置
// 的上限不可再改）。connectSem 的写在本包 m.mu 内、dial 侧在锁内快照
// 引用后于锁外 acquire——否则并发 Set 与正在进行的 dial 对该字段的
// 读写是数据竞争。Get 的获取路径全程不在持 m.mu 时进入 dial，锁序
// 单向（m.mu 不会被 dial 反向持有），无死锁风险。
func (m *Manager) SetConnectConcurrency(n int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if n <= 0 || m.connectSem != nil {
		return
	}
	m.connectSem = make(chan struct{}, n)
}

// dial 建立一条新连接（建连限流 + 握手全程不持 m.mu：锁内握手会把并发
// 建连退化成串行，使限流形同虚设）。healingConn 重建时复用同一路径。
func (m *Manager) dial(ctx context.Context, h *model.Host) (Conn, error) {
	m.mu.Lock()
	sem := m.connectSem // 锁内快照：与 SetConnectConcurrency 的写入互斥
	m.mu.Unlock()
	if sem != nil {
		select {
		case sem <- struct{}{}:
			defer func() { <-sem }()
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	c, err := NewConnection(h, m.defaults)
	if err != nil {
		return nil, err
	}
	if err := c.Connect(ctx); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("host %s connection failed: %w", h.Name, err)
	}
	return c, nil
}

// Get 返回主机的活动连接，必要时建立。
func (m *Manager) Get(ctx context.Context, h *model.Host) (Conn, error) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, errors.New("connection manager is closed")
	}
	if c, ok := m.conns[h.Name]; ok {
		m.mu.Unlock()
		return c, nil
	}
	m.mu.Unlock()

	c, err := m.dial(ctx, h)
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		_ = c.Close()
		return nil, errors.New("connection manager is closed")
	}
	if old, ok := m.conns[h.Name]; ok { // 双重检查：并发下他人已建
		m.mu.Unlock()
		_ = c.Close()
		return old, nil
	}
	hc := &healingConn{mgr: m, host: h, inner: c}
	m.conns[h.Name] = hc
	m.mu.Unlock()
	return hc, nil
}

// CloseAll 关闭全部连接（幂等）。关闭的是自愈包装，包装再关底层连接。
func (m *Manager) CloseAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
	m.closed = true
	for _, c := range m.conns {
		_ = c.Close()
	}
	m.conns = map[string]Conn{}
}
