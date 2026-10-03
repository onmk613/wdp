package sshc

// 会话输出截断标记的回归：上限与保留语义的单元测试随实现迁移至
// conn.CapWriter（internal/conn/capwriter_test.go），此处只锁 sshc 侧
// 的标记文案拼接。

import (
	"strings"
	"testing"

	"wdp/internal/conn"
)

func TestCappedMarker(t *testing.T) {
	var w conn.CapWriter
	w.Write([]byte("short"))
	if got := capped(&w); got != "short" {
		t.Fatalf("未截断应原样返回: %q", got)
	}

	var big conn.CapWriter
	chunk := strings.Repeat("x", 64<<10)
	for range 20 { // 写入 1.25MiB，超过默认 1MiB 上限
		if _, err := big.Write([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	got := capped(&big)
	if !strings.HasPrefix(got, strings.Repeat("x", 1<<20)) {
		t.Fatalf("应保留前 1MiB: %d 字节", len(got))
	}
	if !strings.Contains(got, "[wdp-ssh] output exceeded 1MiB and was truncated") {
		t.Fatalf("截断后应带 sshc 标记: %q", got[len(got)-80:])
	}
}
