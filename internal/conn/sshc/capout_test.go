package sshc

// 会话输出上限的单元回归：超限保留前缀、丢弃余量并标记——此前
// bytes.Buffer 全量驻留，高输出命令即控制端 OOM；标记保证结果能看出
// 输出不完整，而不是拿到一段"正常"的半截输出。

import (
	"strings"
	"testing"
)

func TestCapBufferTruncation(t *testing.T) {
	var w capBuffer
	w.limit = 16
	for i := 0; i < 10; i++ { // 写入 100 字节，上限 16
		if n, err := w.Write([]byte("0123456789")); err != nil || n != 10 {
			t.Fatalf("write: %d %v", n, err)
		}
	}
	if got := w.buf.String(); got != "0123456789012345" {
		t.Fatalf("应保留前 16 字节: %q", got)
	}
	s := w.string()
	if !strings.Contains(s, "truncated") {
		t.Fatalf("截断后应带标记: %q", s)
	}
	// 未超限时不追加标记
	var w2 capBuffer
	w2.limit = 64
	_, _ = w2.Write([]byte("short"))
	if s := w2.string(); s != "short" {
		t.Fatalf("未超限应原样返回: %q", s)
	}
	// limit 未设时按默认上限（写入小于 1MiB 不截断）
	var w3 capBuffer
	_, _ = w3.Write([]byte("x"))
	if s := w3.string(); s != "x" || w3.truncated {
		t.Fatalf("默认上限应可用: %q", s)
	}
}
