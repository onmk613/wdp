package connection

import (
	"context"
	"fmt"
	"sync"

	"wdp/internal/model"
)

// Manager 按主机懒建立并复用连接，play 结束时统一关闭。
// 建连并发由信号量限制，防止大规模主机瞬时握手洪峰。
type Manager struct {
	mu     sync.Mutex
	conns  map[string]Connection
	closed bool

	connectSem chan struct{} // 并发建连上限（nil 不限）
}

// NewManager 创建连接管理器（使用注册表工厂，见 NewConnection）。
func NewManager() *Manager {
	return &Manager{conns: map[string]Connection{}}
}

// SetConnectConcurrency 设置并发建连上限（应在首次 Get 前调用）。
func (m *Manager) SetConnectConcurrency(n int) {
	if n <= 0 || m.connectSem != nil {
		return
	}
	m.connectSem = make(chan struct{}, n)
}

// Get 返回主机的活动连接，必要时建立。
func (m *Manager) Get(ctx context.Context, h *model.Host) (Connection, error) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, fmt.Errorf("连接管理器已关闭")
	}
	if c, ok := m.conns[h.Name]; ok {
		m.mu.Unlock()
		return c, nil
	}
	m.mu.Unlock()

	// 建连限流（握手在锁外进行，允许并发）
	if m.connectSem != nil {
		select {
		case m.connectSem <- struct{}{}:
			defer func() { <-m.connectSem }()
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	// 握手全程不持锁（锁内握手会把并发建连退化成串行，使限流形同虚设）；
	// 并发下对同一主机的重复建连在落表时收敛，输家关闭自己多余的连接。
	c, err := NewConnection(h)
	if err != nil {
		return nil, err
	}
	if err := c.Connect(ctx); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("主机 %s 连接失败: %w", h.Name, err)
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		_ = c.Close()
		return nil, fmt.Errorf("连接管理器已关闭")
	}
	if old, ok := m.conns[h.Name]; ok { // 双重检查：并发下他人已建
		m.mu.Unlock()
		_ = c.Close()
		return old, nil
	}
	m.conns[h.Name] = c
	m.mu.Unlock()
	return c, nil
}

// CloseAll 关闭全部连接（幂等）。
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
	m.conns = map[string]Connection{}
}
