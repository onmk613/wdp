package worker

// BoundedForeach 的行为口径回归：
//   - 并发度上限：在途 fn 恒 ≤ concurrency，且上限可达（不是串行假并发）；
//   - ctx 取消：不再派发新项、返回 ctx.Err()；
//   - 错误传播：返回首个 fn 错误，其余项照常处理（不提前终止）；
//   - 防御：concurrency <= 0 串行兜底、空集直接返回 nil。

import (
	"context"
	"errors"
	"sync"
	"testing"
)

// genItems 造 n 个 int 项（顺带校验 fn 收到的下标与取值对齐）。
func genItems(n int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = i
	}
	return out
}

// TestBoundedForeachConcurrencyCap 前 concurrency 项各自占住一个 worker
// 等全员到齐（栅栏），期间在途数恰好等于 concurrency——证明池被填满且
// 不越界；后续项放行后全部执行、在途数归零。
func TestBoundedForeachConcurrencyCap(t *testing.T) {
	const (
		n    = 24
		conc = 3
	)
	items := genItems(n)
	var (
		mu       sync.Mutex
		inFlight int
		maxSeen  int
		ran      int
	)
	arrived := make(chan struct{}, conc) // 前 conc 项的占位信号
	allIn := make(chan struct{})
	go func() {
		for i := 0; i < conc; i++ {
			<-arrived
		}
		close(allIn) // conc 个槽位全部在途，放行
	}()
	err := BoundedForeach(context.Background(), items, conc, func(context.Context, int, int) error {
		mu.Lock()
		inFlight++
		if inFlight > maxSeen {
			maxSeen = inFlight
		}
		mu.Unlock()
		select {
		case arrived <- struct{}{}:
			<-allIn // 占住槽位直到 conc 个同时在途（把并发上限顶满）
		default: // 栅栏已过（缓冲满）：其余项直接通过
		}
		mu.Lock()
		inFlight--
		ran++
		mu.Unlock()
		return nil
	})
	if err != nil {
		t.Fatalf("无 fn 错误应返回 nil: %v", err)
	}
	if maxSeen != conc {
		t.Fatalf("在途峰值应恰为 %d（池要填满且不越界）: %d", conc, maxSeen)
	}
	if ran != n {
		t.Fatalf("全部 %d 项都应执行: %d", n, ran)
	}
	if inFlight != 0 {
		t.Fatalf("结束后在途应归零: %d", inFlight)
	}
}

// TestBoundedForeachCtxCancelStopsDispatch 预取消的 ctx：一项都不执行
// （快路径直接截断派发），返回 ctx.Err() 提示遍历未完成。
func TestBoundedForeachCtxCancelStopsDispatch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var ran int
	err := BoundedForeach(ctx, genItems(10), 2, func(context.Context, int, int) error {
		ran++
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("取消后截断应返回 ctx.Err(): %v", err)
	}
	if ran != 0 {
		t.Fatalf("预取消 ctx 不应执行任何项: %d", ran)
	}
}

// TestBoundedForeachErrorPropagation 单 worker（派发序即执行序）：fn 对
// 第 2、5 项报错，返回首个错误且其余项照常处理——批量语义是"逐台结果
// 各自落账"，错误聚合不得提前终止；顺带校验 i 与 items[i] 对齐。
func TestBoundedForeachErrorPropagation(t *testing.T) {
	items := genItems(12)
	boom2 := errors.New("boom at 2")
	boom5 := errors.New("boom at 5")
	var order []int
	err := BoundedForeach(context.Background(), items, 1, func(_ context.Context, i int, v int) error {
		if i != v {
			t.Errorf("下标与取值应对齐: i=%d v=%d", i, v)
		}
		order = append(order, v)
		switch v {
		case 2:
			return boom2
		case 5:
			return boom5
		}
		return nil
	})
	if !errors.Is(err, boom2) {
		t.Fatalf("应返回首个 fn 错误: %v", err)
	}
	if len(order) != len(items) {
		t.Fatalf("出错后其余项应继续处理: %v", order)
	}
	for i, v := range order {
		if v != items[i] {
			t.Fatalf("单 worker 应按序执行: %v", order)
		}
	}
}

// TestBoundedForeachDefensive concurrency<=0 按 1 兜底、空集返回 nil。
func TestBoundedForeachDefensive(t *testing.T) {
	var ran int
	if err := BoundedForeach(context.Background(), genItems(5), 0, func(context.Context, int, int) error {
		ran++
		return nil
	}); err != nil || ran != 5 {
		t.Fatalf("concurrency=0 应按 1 串行兜底: err=%v ran=%d", err, ran)
	}
	if err := BoundedForeach(context.Background(), nil, 8, func(context.Context, int, int) error {
		t.Fatal("空集不应执行 fn")
		return nil
	}); err != nil {
		t.Fatalf("空集应返回 nil: %v", err)
	}
}
