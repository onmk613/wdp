package web

// per-host 执行闸门：同一主机同一时刻只允许一个执行流（应用执行/远程
// 命令整run持有；升级短暂持有）。多用户共享一个 server 时，两个操作者
// 并发部署到同一批主机会互相踩（脚本并行 + release marker 竞争），这是
// 数据损坏级问题——在 server 编排层串行化，agent 无需配合。
//
// 死锁预防：按主机 ID 升序逐台获取；run 级整体持有（请求内本就串行）。

import (
	"context"
	"sort"
	"sync"
	"time"
)

type hostGate struct {
	mu    sync.Mutex
	locks map[int64]*sync.Mutex
}

func newHostGate() *hostGate { return &hostGate{locks: map[int64]*sync.Mutex{}} }

func (g *hostGate) lockOf(id int64) *sync.Mutex {
	g.mu.Lock()
	defer g.mu.Unlock()
	l, ok := g.locks[id]
	if !ok {
		l = &sync.Mutex{}
		g.locks[id] = l
	}
	return l
}

// Acquire 阻塞获取一组主机（升序防死锁）；返回释放函数。空集直接过。
func (g *hostGate) Acquire(hostIDs []int64) func() {
	if len(hostIDs) == 0 {
		return func() {}
	}
	ids := append([]int64(nil), hostIDs...)
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		g.lockOf(id).Lock()
	}
	return func() {
		for _, id := range ids {
			g.lockOf(id).Unlock()
		}
	}
}

// AcquireCtx 阻塞获取一组主机，等待期间 ctx 取消即放弃（已获取的锁全部
// 回退，返回 false）。后台 run 用：server 关停/排队超时可中断，不再无限
// 堆积等待中的 goroutine。
func (g *hostGate) AcquireCtx(ctx context.Context, hostIDs []int64) (func(), bool) {
	if len(hostIDs) == 0 {
		return func() {}, true
	}
	ids := append([]int64(nil), hostIDs...)
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	held := make([]*sync.Mutex, 0, len(ids))
	for _, id := range ids {
		l := g.lockOf(id)
		for {
			if l.TryLock() {
				held = append(held, l)
				break
			}
			select {
			case <-ctx.Done():
				for _, h := range held {
					h.Unlock()
				}
				return nil, false
			case <-time.After(50 * time.Millisecond):
				// 轮询而非 channel 广播：锁逐台升序获取，广播需按锁
				// 注册/唤醒 waiter；闸门等待通常以秒计，50ms 轮询是
				// 最简取舍（TryAcquire 同款）。
			}
		}
	}
	return func() {
		for _, l := range held {
			l.Unlock()
		}
	}, true
}

// Forget 回收主机闸门锁（主机删除后调用，防 locks 只增不减）。锁正被持
// 有时不回收——持有者经指针解锁，强删会让后续 lockOf 建出第二个锁对象。
func (g *hostGate) Forget(id int64) {
	g.mu.Lock()
	l, ok := g.locks[id]
	if ok && l.TryLock() {
		delete(g.locks, id)
		l.Unlock()
	}
	g.mu.Unlock()
}

// TryAcquire 限时获取（升级等同步请求用：闸门被长执行占用时快速失败，
// 由用户稍后重试，而不是挂住 HTTP）。
func (g *hostGate) TryAcquire(hostIDs []int64, wait time.Duration) (func(), bool) {
	if len(hostIDs) == 0 {
		return func() {}, true
	}
	ids := append([]int64(nil), hostIDs...)
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	held := make([]*sync.Mutex, 0, len(ids))
	deadline := time.Now().Add(wait)
	for _, id := range ids {
		l := g.lockOf(id)
		for {
			if l.TryLock() {
				held = append(held, l)
				break
			}
			if time.Now().After(deadline) {
				for _, h := range held {
					h.Unlock()
				}
				return nil, false
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	return func() {
		for _, l := range held {
			l.Unlock()
		}
	}, true
}
