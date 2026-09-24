package render

// merge 的安全性：sprig 原实现就地改写目标 map，而变量域里的嵌套 map
// 与 chart values 是同一对象、被批内所有主机的 goroutine 共享。这组测试
// 锁定"merge 不改写输入"这一契约（配合 -race 可捕获并发写）。
//
// 历史问题：{{ merge .app (dict "x" 1) }} 在多主机并发渲染下会写同一
// map，轻则跨主机配置互相污染，重则 concurrent map writes fatal。

import (
	"strings"
	"sync"
	"testing"
)

// TestSafeMergeDoesNotMutateDestination 合并结果正确，且目标 map 与其
// 嵌套子 map 都保持原样。
func TestSafeMergeDoesNotMutateDestination(t *testing.T) {
	dst := map[string]any{
		"name": "demo",
		"sub":  map[string]any{"a": 1},
	}
	merged, ok := safeMerge(dst, map[string]any{"port": 8080, "sub": map[string]any{"b": 2}}).(map[string]any)
	if !ok {
		t.Fatal("merge 应返回 map")
	}
	if merged["port"] != 8080 || merged["name"] != "demo" {
		t.Fatalf("合并结果异常: %+v", merged)
	}
	if _, leaked := dst["port"]; leaked {
		t.Fatalf("不得改写目标 map: %+v", dst)
	}
	if sub, _ := dst["sub"].(map[string]any); sub != nil {
		if _, leaked := sub["b"]; leaked {
			t.Fatalf("不得改写目标 map 的嵌套子 map: %+v", sub)
		}
	}
	// 深合并（同名键递归合并，sprig/Helm 语义）
	dst2 := map[string]any{"sub": map[string]any{"a": 1}}
	got, _ := safeMerge(dst2, map[string]any{"sub": map[string]any{"b": 2}}).(map[string]any)
	sub, _ := got["sub"].(map[string]any)
	if sub["a"] != 1 || sub["b"] != 2 {
		t.Fatalf("深合并语义应保留: %+v", got)
	}
}

// TestMergeTemplateConcurrent 多 goroutine 用同一份共享 values 渲染
// merge：结果互不污染，且在 -race 下无数据竞争。
func TestMergeTemplateConcurrent(t *testing.T) {
	shared := map[string]any{
		"app": map[string]any{"name": "demo"},
	}
	eng := DefaultEngine()
	const workers = 16
	var wg sync.WaitGroup
	errs := make([]error, workers)
	outs := make([]string, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			outs[i], errs[i] = eng.Render(`{{ $m := merge .app (dict "worker" .n) }}{{ $m.name }}-{{ $m.worker }}`, map[string]any{
				"app": shared["app"],
				"n":   i,
			})
		}(i)
	}
	wg.Wait()
	for i := 0; i < workers; i++ {
		if errs[i] != nil {
			t.Fatalf("worker %d 渲染失败: %v", i, errs[i])
		}
		if !strings.HasPrefix(outs[i], "demo-") {
			t.Fatalf("worker %d 结果异常: %q", i, outs[i])
		}
	}
	// 共享 map 未被任何 worker 写入
	if app, _ := shared["app"].(map[string]any); app != nil {
		if _, leaked := app["worker"]; leaked {
			t.Fatalf("共享 values 被 merge 改写: %+v", app)
		}
	}
}
