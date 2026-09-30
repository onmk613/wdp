package render

// size/humansize 单位换算对的表驱动测试（facts 的 *_bytes 比对与展示
// 都压在这对函数上）。

import (
	"strings"
	"testing"
)

func TestParseSize(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		{"1073741824", 1 << 30},
		{"10G", 10 << 30},
		{"10g", 10 << 30},
		{"10GB", 10 << 30},
		{"10GiB", 10 << 30},
		{"512Mi", 512 << 20},
		{"1.5TB", int64(1.5 * (1 << 40))},
		{"256K", 256 << 10},
		{"512b", 512},
		{"0.5G", 1 << 29},
	}
	for _, c := range cases {
		got, err := ParseSize(c.in)
		if err != nil {
			t.Errorf("ParseSize(%q) 报错: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseSize(%q) = %d, want %d", c.in, got, c.want)
		}
	}
	for _, bad := range []string{"", "10Q", "G", "abc", "-5G"} {
		if _, err := ParseSize(bad); err == nil {
			t.Errorf("ParseSize(%q) 应报错", bad)
		}
	}
}

func TestHumanSize(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{512, "512"},
		{1 << 10, "1.0Ki"},
		{80530636800, "75.0Gi"},
		{10 << 30, "10.0Gi"},
		{2 << 40, "2.0Ti"},
		{-1, "-1"},
	}
	for _, c := range cases {
		if got := HumanSize(c.in); got != c.want {
			t.Errorf("HumanSize(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestSizeFuncsInTemplate 模板内可用：带单位字面量与 facts 字节数直接比对。
func TestSizeFuncsInTemplate(t *testing.T) {
	vars := map[string]any{"avail": int64(80530636800)} // 75Gi
	out, err := DefaultEngine().Render(`{{ if ge .avail (size "10G") }}ok{{ end }} {{ humansize .avail }}`, vars)
	if err != nil {
		t.Fatalf("渲染失败: %v", err)
	}
	if out != "ok 75.0Gi" {
		t.Fatalf("输出异常: %q", out)
	}
	// 非法单位 fail-loud：静默当 0 会把容量判断变成恒真/恒假
	if _, err := DefaultEngine().Render(`{{ size "10Q" }}`, nil); err == nil || !strings.Contains(err.Error(), "unknown unit") {
		t.Fatalf("非法单位应报错: %v", err)
	}
}
