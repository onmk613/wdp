package web

// execHub 逐 run 事件流的语义：订阅晚也能完整重放、终态后重放含 done、
// run 不在本进程时按落库兜底（此处只测 hub 本体）。

import "testing"

func TestExecHubReplayAndLive(t *testing.T) {
	h := newExecHub()
	h.start(1)
	h.host(1, ExecHostResult{ID: 1, Name: "h1", Code: 0, Stdout: "a"}, "ok")

	// 订阅晚于首个事件：重放不丢
	replay, ch, done := h.subscribe(1)
	if done || ch == nil {
		t.Fatalf("进行中的 run 应有实时通道: replay=%v done=%v", replay, done)
	}
	if len(replay) != 1 || replay[0].Result.Name != "h1" || replay[0].Status != "ok" {
		t.Fatalf("重放应含首台结果: %+v", replay)
	}

	// 实时事件
	h.host(1, ExecHostResult{ID: 2, Name: "h2", Code: 1}, "failed")
	ev := <-ch
	if ev.Event != "host" || ev.Result.Name != "h2" || ev.Status != "failed" {
		t.Fatalf("实时事件异常: %+v", ev)
	}

	// 收尾：订阅者收 done；此后新订阅者重放全部事件且不再有通道
	h.finish(1, 1, 1)
	ev = <-ch
	if ev.Event != "done" || ev.OK != 1 || ev.Failed != 1 {
		t.Fatalf("done 事件异常: %+v", ev)
	}
	replay2, ch2, done2 := h.subscribe(1)
	if !done2 || ch2 != nil {
		t.Fatalf("终态 run 重放后不应有实时通道: %+v", done2)
	}
	if len(replay2) != 3 { // 2×host + 1×done
		t.Fatalf("终态重放应含全部事件: %+v", replay2)
	}
}

func TestExecHubUnknownRun(t *testing.T) {
	h := newExecHub()
	replay, ch, done := h.subscribe(42)
	if replay != nil || ch != nil || done {
		t.Fatalf("未知 run 应返回空重放无通道: %+v", replay)
	}
	// 未知 run 的事件被丢弃（不 panic）
	h.host(42, ExecHostResult{ID: 1, Name: "x"}, "ok")
	h.finish(42, 0, 1)
}
