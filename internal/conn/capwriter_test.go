package conn

// 输出封顶 writer 的单元回归（自 sshc capout_test.go 迁移并扩为三通道
// 共享口径）：超限保留前缀、丢弃余量并标记——标记保证结果能看出输出
// 不完整，而不是拿到一段"正常"的半截输出。

import (
	"strings"
	"testing"
)

func TestCapWriterTruncation(t *testing.T) {
	var w CapWriter
	w.limit = 16
	for i := 0; i < 10; i++ { // 写入 100 字节，上限 16
		if n, err := w.Write([]byte("0123456789")); err != nil || n != 10 {
			t.Fatalf("write: %d %v", n, err)
		}
	}
	if got := w.String(); got != "0123456789012345" {
		t.Fatalf("应保留前 16 字节: %q", got)
	}
	if !w.Truncated() {
		t.Fatal("超限应标记 Truncated")
	}
	// 未超限时不标记
	var w2 CapWriter
	w2.limit = 64
	_, _ = w2.Write([]byte("short"))
	if s := w2.String(); s != "short" || w2.Truncated() {
		t.Fatalf("未超限应原样返回且不标记: %q", s)
	}
	// limit 未设时按默认上限（写入小于 1MiB 不截断）
	var w3 CapWriter
	_, _ = w3.Write([]byte("x"))
	if s := w3.String(); s != "x" || w3.Truncated() {
		t.Fatalf("默认上限应可用: %q", s)
	}
	// 一次性写入跨越上限边界：保留前缀、整段不丢
	var w4 CapWriter
	w4.limit = 4
	if n, err := w4.Write([]byte("abcdef")); err != nil || n != 6 {
		t.Fatalf("跨边界写入应上报原始长度: %d %v", n, err)
	}
	if s := w4.String(); s != "abcd" || !w4.Truncated() {
		t.Fatalf("跨边界应保留前 4 字节并标记: %q", s)
	}
	if strings.Contains(w4.String(), "truncated") {
		t.Fatal("String 不含标记文案（由通道侧拼接）")
	}
}
