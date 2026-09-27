package fmtutil

// TruncateUTF8 的边界回归：截断点落在多字节字符各处时结果必须恒为合法
// UTF-8 且不丢/不错位字符。历史实现按 s[cut-1] 判定，在"边界正好落在
// 完整字符后"时多退（丢字符）、在 lead 字节后停住（留下残缺字节），
// 这里锁定正确语义。

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTruncateUTF8(t *testing.T) {
	s := "中中中中abc" // 12 字节中文 + 3 字节 ASCII
	cases := []struct {
		max  int
		want string
	}{
		{0, ""}, // 非正上限：空串
		{1, ""}, // 落在首字符中间：整字符丢弃
		{2, ""},
		{3, "中"}, // 边界恰好：不得多退（历史实现在此丢字符）
		{4, "中"}, // lead 后：回退到 3（历史实现留下残缺 lead 字节）
		{5, "中"}, // 续字节中：回退到 3
		{6, "中中"},
		{12, "中中中中"},
		{13, "中中中中a"},
		{15, s}, // 未超限原样返回
		{99, s},
	}
	for _, c := range cases {
		got := TruncateUTF8(s, c.max)
		if got != c.want {
			t.Fatalf("TruncateUTF8(max=%d) = %q, want %q", c.max, got, c.want)
		}
		if !utf8.ValidString(got) {
			t.Fatalf("TruncateUTF8(max=%d) 产生非法 UTF-8: %q", c.max, got)
		}
	}
	// 任意切点均为合法 UTF-8 前缀（含 4 字节字符）
	mixed := "a" + strings.Repeat("😀", 3) + "中b"
	for max := 0; max <= len(mixed); max++ {
		got := TruncateUTF8(mixed, max)
		if !utf8.ValidString(got) {
			t.Fatalf("TruncateUTF8(max=%d) 非法 UTF-8: %q", max, got)
		}
		if !strings.HasPrefix(mixed, got) {
			t.Fatalf("TruncateUTF8(max=%d) 不是原串前缀: %q", max, got)
		}
	}
}
