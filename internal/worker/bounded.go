package worker

// 有界并发遍历的公共 helper：worker 的周期扫描（Prober/Monitor）与 web
// 的批量操作（batchProbe/batchDelete）共用。放在 worker 包是依赖方向使
// 然——web 可以 import worker，worker 不得反向 import web（见包注释）。

import (
	"context"
	"sync"
)

// sweepConcurrency 周期扫描（探活/采样）的并发度：离线主机的单趟探测/
// 抓取要等满超时，串行会让整轮时延随主机数线性放大、1 分钟周期装不下；
// 再大则控制台自身的出向连接与内存压力跟着涨，8 是两者的折中。
const sweepConcurrency = 8

// BoundedForeach 以固定 concurrency 个 worker 从 channel 消费 items，
// 对每项调用 fn（i 为该项在 items 中的下标——批量场景按下标回填结果槽，
// 并发完成序不影响响应次序）。
//
// 为什么不是"逐项 go + 信号量限流"：信号量只约束同时在工作的 goroutine，
// 约束不了 goroutine 的创建——万台台账遇上慢 agent 时会驻留数万个阻塞
// 在信号量上的 goroutine（每项一个栈，纯内存浪费）；固定池把驻留量收敛
// 为常数 concurrency。
//
// 语义口径（从各处样板迁入时不得漂移）：
//   - fn 的 error 只向调用方传播：取首个非 nil 错误返回，其余项照常处理
//     （与 store.BatchAssign「单台失败记首个错误、其余继续」同款）。跳过
//     某项、标记逐台失败等语义由调用方在 fn 内自行决策，helper 不替调用
//     方收敛。
//   - ctx 取消后不再派发新项（已在途的项会执行完）；返回值优先取 fn 的
//     首个错误，无错误且确有项因取消未派发时返回 ctx.Err()。取消与空闲
//     worker 同时就绪的竞态下可能仍派发/启动个别项——"写库前判 ctx.Err()"
//     的兜底（Prober/Monitor 落库守卫的口径）由 fn 自带。
//   - concurrency <= 0 按 1 处理（防御误配，串行兜底）。
//
// 迁移备注：web/upgrade.go 的批量升级（loop+sem+wg 样板的另一处）留待
// 其后台化重写完成后再迁入——该文件正被并行重写，现在动它只会徒增
// 合并冲突。
func BoundedForeach[T any](ctx context.Context, items []T, concurrency int, fn func(ctx context.Context, i int, v T) error) error {
	if concurrency < 1 {
		concurrency = 1
	}
	// 无缓冲 channel：worker 全忙时派发端自然背压，在途 fn 恒 ≤ concurrency
	queue := make(chan int)
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
	)
	for w := 0; w < concurrency; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range queue {
				if err := fn(ctx, i, items[i]); err != nil {
					mu.Lock()
					if firstErr == nil {
						firstErr = err
					}
					mu.Unlock()
				}
			}
		}()
	}
	truncated := false
dispatch:
	for i := range items {
		// 快路径：已取消就不再起项（select 里投递与 Done 同时就绪时选择是
		// 随机的，先判一道把"取消后还派发"压到与取消几乎同瞬的窗口）
		if ctx.Err() != nil {
			truncated = true
			break dispatch
		}
		select {
		case queue <- i:
		case <-ctx.Done():
			truncated = true
			break dispatch // 剩余项不再投递（select 内裸 break 只断 select）
		}
	}
	close(queue)
	wg.Wait()
	if firstErr != nil {
		return firstErr
	}
	if truncated {
		return ctx.Err()
	}
	return nil
}
